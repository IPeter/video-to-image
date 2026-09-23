#!/usr/bin/env bash
# deploy.sh — build v2i, load the images onto the k3s node and apply an overlay.
#
#   deploy/k8s/deploy.sh                 # overlay home-k3s, current kubectl context
#   OVERLAY=home-k3s NODE=ipetike-home-lab deploy/k8s/deploy.sh
#   SKIP_BUILD=1 TAG=<existing tag> deploy/k8s/deploy.sh   # re-apply without building
#
# There is no registry: images are streamed into the node's containerd through a
# short-lived `kubectl debug node` pod (host access), which is deleted afterwards.
# First run: creates the overlay's git-ignored secrets (random DB password, internal
# token, IP-hash key, basic-auth password) and copies GEMINI_API_KEY from ./.env.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
overlay="${OVERLAY:-home-k3s}"
ov="$root/deploy/k8s/overlays/$overlay"
ns=v2i
node="${NODE:-$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')}"
tag="${TAG:-$(git -C "$root" rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)}"
platform="${PLATFORM:-linux/$(kubectl get node "$node" -o jsonpath='{.status.nodeInfo.architecture}')}"
basic_user="${BASIC_AUTH_USER:-dealer}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf 'deploy: %s\n' "$*" >&2; exit 1; }

[ -d "$ov" ] || die "no overlay at $ov"
for bin in kubectl docker openssl htpasswd; do command -v "$bin" >/dev/null || die "$bin not found"; done
log "context $(kubectl config current-context) · node $node ($platform) · overlay $overlay · tag $tag"

# ---- secrets (first run only; never regenerate secrets.env once Postgres has data) ----
umask 077
if [ ! -f "$ov/secrets.env" ]; then
  log "creating $ov/secrets.env"
  pw="$(openssl rand -hex 24)"
  cat >"$ov/secrets.env" <<EOF
POSTGRES_PASSWORD=$pw
DATABASE_URL=postgres://v2i:$pw@postgres:5432/v2i?sslmode=disable
V2I_INTERNAL_TOKEN=$(openssl rand -hex 24)
V2I_IP_HASH_SALT=$(openssl rand -hex 32)
EOF
fi
if [ ! -f "$ov/gemini.env" ]; then
  key="${GEMINI_API_KEY:-}"
  if [ -z "$key" ] && [ -f "$root/.env" ]; then
    key="$(grep -E '^GEMINI_API_KEY=' "$root/.env" | head -1 | cut -d= -f2- | tr -d '[:space:]')"
  fi
  [ -n "$key" ] || die "GEMINI_API_KEY not set (export it or put it in $root/.env)"
  printf 'GEMINI_API_KEY=%s\n' "$key" >"$ov/gemini.env"
  log "created $ov/gemini.env"
fi
if [ ! -f "$ov/basic-auth.users" ]; then
  pass="$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
  htpasswd -nbB "$basic_user" "$pass" | sed '/^$/d' >"$ov/basic-auth.users"
  printf '%s:%s\n' "$basic_user" "$pass" >"$ov/basic-auth.password"
  log "created basic-auth login for '$basic_user' (password in $ov/basic-auth.password)"
fi
umask 022

# ---- build ----
if [ -z "${SKIP_BUILD:-}" ]; then
  log "building localhost/v2i:$tag ($platform)"
  docker build --platform "$platform" -f "$root/deploy/docker/Dockerfile.v2i" \
    --build-arg VERSION="$tag" -t "localhost/v2i:$tag" "$root"
  log "building localhost/v2i-web:$tag ($platform)"
  docker build --platform "$platform" -f "$root/deploy/docker/Dockerfile.web" \
    -t "localhost/v2i-web:$tag" "$root/web"
fi

# ---- load images into the node's containerd ----
log "starting a node debug pod on $node"
dbg="$(kubectl debug "node/$node" -n default --profile=sysadmin --image=alpine:3.22 -- sleep 1800 2>&1 \
  | grep -o 'node-debugger-[a-z0-9-]*' | head -1)"
[ -n "$dbg" ] || die "could not start the node debug pod"
trap 'kubectl delete pod -n default "$dbg" --wait=false >/dev/null 2>&1 || true' EXIT
kubectl wait -n default --for=condition=Ready "pod/$dbg" --timeout=180s >/dev/null
k3s_ctr=(kubectl exec -i -n default "$dbg" -- chroot /host /usr/local/bin/k3s ctr -n k8s.io)
"${k3s_ctr[@]}" version >/dev/null || die "k3s ctr not usable on the node (is k3s at /usr/local/bin/k3s?)"
for img in "localhost/v2i:$tag" "localhost/v2i-web:$tag"; do
  log "importing $img"
  docker save "$img" | "${k3s_ctr[@]}" images import - >/dev/null
done

# ---- apply ----
prev_tags="$(sed -n 's/^    newTag: //p' "$ov/kustomization.yaml" | sort -u | tr '\n' ' ')"
sed -i.bak "s/^    newTag: .*/    newTag: $tag/" "$ov/kustomization.yaml" && rm -f "$ov/kustomization.yaml.bak"
log "applying overlay $overlay"
kubectl kustomize --load-restrictor LoadRestrictionsNone "$ov" | kubectl apply -f -

log "waiting for rollouts"
kubectl -n "$ns" rollout status statefulset/postgres --timeout=300s
for d in v2i-api v2i-worker web-cars; do
  kubectl -n "$ns" rollout status "deploy/$d" --timeout=600s
done

# ---- prune images older than the previous deploy (keep current + previous for rollback) ----
keep="$tag $prev_tags"
for ref in $("${k3s_ctr[@]}" images ls -q | grep -E '^localhost/v2i(-web)?:' || true); do
  t="${ref##*:}"
  case " $keep " in *" $t "*) ;; *) "${k3s_ctr[@]}" images rm "$ref" >/dev/null && log "pruned $ref" ;; esac
done

log "done: tag $tag"
kubectl -n "$ns" get pods -o wide
