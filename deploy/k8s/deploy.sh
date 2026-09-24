#!/usr/bin/env bash
# deploy.sh — build v2i, load the images onto the k3s node and apply an overlay.
#
#   deploy/k8s/deploy.sh                 # overlay home-k3s, current kubectl context
#   OVERLAY=home-k3s NODE=ipetike-home-lab deploy/k8s/deploy.sh
#   SKIP_BUILD=1 deploy/k8s/deploy.sh                      # re-apply the tags in kustomization.yaml
#   SKIP_BUILD=1 TAG=<existing tag> deploy/k8s/deploy.sh   # re-apply / roll back without building
#   STUDIO_TAG=<existing tag> deploy/k8s/deploy.sh         # pin the studio sidecar image (no studio build)
#
# Three images: v2i (api + worker), v2i-web and v2i-studio (the segmentation sidecar in
# the worker pod, built from services/studio with the models fetched by
# services/studio/fetch-models.sh). TAG names the v2i and v2i-web images; the studio
# image keeps its previous tag when a rebuild produced the identical image (it is ~1 GB
# and rarely changes, so it is streamed to the node only when it changed).
#
# There is no registry: images are streamed into the node's containerd through a
# short-lived `kubectl debug node` pod (host access), which is deleted afterwards.
# First run: creates the overlay's git-ignored secrets (random DB password, internal
# token, IP-hash key, basic-auth password) and copies GEMINI_API_KEY from ./.env.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
overlay="${OVERLAY:-home-k3s}"
ov="$root/deploy/k8s/overlays/$overlay"
kust="$ov/kustomization.yaml"
studio_dir="$root/services/studio"
ns=v2i
basic_user="${BASIC_AUTH_USER:-dealer}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf 'deploy: %s\n' "$*" >&2; exit 1; }

# cur_tag <image>: the image's newTag in the overlay's kustomization.yaml ("" if none).
cur_tag() { awk -v n="$1" '/^  - name: / { cur = $3 } /^    newTag: / && cur == n { print $2; exit }' "$kust"; }
# set_tag <image> <tag>: rewrite that image's newTag.
set_tag() {
  awk -v n="$1" -v t="$2" '
    /^  - name: / { cur = $3 }
    /^    newTag: / && cur == n { sub(/newTag: .*/, "newTag: " t); found = 1 }
    { print }
    END { if (!found) exit 3 }' "$kust" >"$kust.tmp" || { rm -f "$kust.tmp"; die "no image '$1' with a newTag in $kust"; }
  mv "$kust.tmp" "$kust"
}

[ -d "$ov" ] || die "no overlay at $ov"
for bin in kubectl docker openssl htpasswd awk; do command -v "$bin" >/dev/null || die "$bin not found"; done
node="${NODE:-$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')}"
platform="${PLATFORM:-linux/$(kubectl get node "$node" -o jsonpath='{.status.nodeInfo.architecture}')}"
if [ -n "${SKIP_BUILD:-}" ]; then
  tag="${TAG:-$(cur_tag v2i)}"
  studio_tag="${STUDIO_TAG:-$(cur_tag v2i-studio)}"
else
  tag="${TAG:-$(git -C "$root" rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)}"
  studio_tag="${STUDIO_TAG:-}" # empty: built below
fi
[ -n "$tag" ] || die "no tag: set TAG (SKIP_BUILD needs an image that is already built)"
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
built=" " # refs built in this run: always imported, even if the tag is on the node
if [ -z "${SKIP_BUILD:-}" ] && [ -z "$studio_tag" ]; then
  # First, so a missing model fails the deploy before anything is built. The models are
  # not in git (~0.6 GB); fetch-models.sh copies or downloads them into
  # services/studio/models/ and verifies their sha256, or fails.
  [ -f "$studio_dir/Dockerfile" ] && [ -f "$studio_dir/fetch-models.sh" ] \
    || die "no studio sidecar at $studio_dir (Dockerfile + fetch-models.sh)"
  log "checking the studio models (services/studio/fetch-models.sh)"
  bash "$studio_dir/fetch-models.sh" \
    || die "studio models missing or corrupt in $studio_dir/models: see $studio_dir/README.md. To deploy without rebuilding the sidecar: STUDIO_TAG=<a v2i-studio tag on the node>"
fi
if [ -z "${SKIP_BUILD:-}" ]; then
  log "building localhost/v2i:$tag ($platform)"
  docker build --platform "$platform" -f "$root/deploy/docker/Dockerfile.v2i" \
    --build-arg VERSION="$tag" -t "localhost/v2i:$tag" "$root"
  log "building localhost/v2i-web:$tag ($platform)"
  docker build --platform "$platform" -f "$root/deploy/docker/Dockerfile.web" \
    -t "localhost/v2i-web:$tag" "$root/web"
  built="$built localhost/v2i:$tag localhost/v2i-web:$tag "
  if [ -z "$studio_tag" ]; then
    log "building localhost/v2i-studio:$tag ($platform)"
    docker build --platform "$platform" -f "$studio_dir/Dockerfile" -t "localhost/v2i-studio:$tag" "$studio_dir"
    studio_tag="$tag"
    prev="$(cur_tag v2i-studio)"
    new_id="$(docker image inspect -f '{{.Id}}' "localhost/v2i-studio:$tag")"
    if [ -n "$prev" ] && [ "$prev" != "$tag" ] \
      && [ "$(docker image inspect -f '{{.Id}}' "localhost/v2i-studio:$prev" 2>/dev/null || true)" = "$new_id" ]; then
      studio_tag="$prev" # identical image: reuse the tag (and skip the import if the node has it)
      docker image rm "localhost/v2i-studio:$tag" >/dev/null
      log "studio image unchanged: keeping localhost/v2i-studio:$prev"
    else
      built="$built localhost/v2i-studio:$tag "
    fi
  fi
fi
case "$studio_tag" in
  "" | unbuilt) die "the v2i-studio image was never built for $overlay: run once without SKIP_BUILD, or set STUDIO_TAG" ;;
esac

# ---- load images into the node's containerd ----
log "starting a node debug pod on $node"
dbg="$(kubectl debug "node/$node" -n default --profile=sysadmin --image=alpine:3.22 -- sleep 1800 2>&1 \
  | grep -o 'node-debugger-[a-z0-9-]*' | head -1)"
[ -n "$dbg" ] || die "could not start the node debug pod"
trap 'kubectl delete pod -n default "$dbg" --wait=false >/dev/null 2>&1 || true' EXIT
kubectl wait -n default --for=condition=Ready "pod/$dbg" --timeout=180s >/dev/null
k3s_ctr=(kubectl exec -i -n default "$dbg" -- chroot /host /usr/local/bin/k3s ctr -n k8s.io)
"${k3s_ctr[@]}" version >/dev/null || die "k3s ctr not usable on the node (is k3s at /usr/local/bin/k3s?)"
on_node="$("${k3s_ctr[@]}" images ls -q)"
for img in "localhost/v2i:$tag" "localhost/v2i-web:$tag" "localhost/v2i-studio:$studio_tag"; do
  case "$built" in
    *" $img "*) ;;
    *) if grep -Fxq -- "$img" <<<"$on_node"; then log "$img is already on the node"; continue; fi ;;
  esac
  docker image inspect "$img" >/dev/null 2>&1 || die "$img is neither on the node nor in the local docker"
  log "importing $img ($(docker image inspect -f '{{.Size}}' "$img" | awk '{printf "%.0f MB", $1 / 1e6}'))"
  docker save "$img" | "${k3s_ctr[@]}" images import - >/dev/null
done

# ---- apply ----
keep=" " # current + previous ref of each image (for rollback); older ones are pruned
for n in v2i v2i-web v2i-studio; do
  t="$(cur_tag "$n")"
  [ -z "$t" ] || keep="$keep localhost/$n:$t "
done
set_tag v2i "$tag"
set_tag v2i-web "$tag"
set_tag v2i-studio "$studio_tag"
keep="$keep localhost/v2i:$tag localhost/v2i-web:$tag localhost/v2i-studio:$studio_tag "
log "applying overlay $overlay (v2i + v2i-web $tag, v2i-studio $studio_tag)"
kubectl kustomize --load-restrictor LoadRestrictionsNone "$ov" | kubectl apply -f -

log "waiting for rollouts"
kubectl -n "$ns" rollout status statefulset/postgres --timeout=300s
for d in v2i-api v2i-worker web-cars; do
  # The worker pod is Ready once the studio sidecar has loaded its models (its startup
  # probe allows 10 min); the worker container itself runs from the start.
  t=600s; [ "$d" != v2i-worker ] || t=900s
  kubectl -n "$ns" rollout status "deploy/$d" --timeout="$t" \
    || die "deploy/$d did not become ready (kubectl -n $ns get pods; kubectl -n $ns logs deploy/$d --all-containers)"
done

# ---- prune images older than the previous deploy (keep current + previous for rollback) ----
# Only after a build: a re-apply or rollback (SKIP_BUILD) keeps every image on the node.
if [ -z "${SKIP_BUILD:-}" ]; then
  for ref in $("${k3s_ctr[@]}" images ls -q | grep -E '^localhost/v2i(-web|-studio)?:' || true); do
    case "$keep" in *" $ref "*) ;; *) "${k3s_ctr[@]}" images rm "$ref" >/dev/null && log "pruned $ref" ;; esac
  done
fi

log "done: tag $tag (studio $studio_tag)"
kubectl -n "$ns" get pods -o wide
