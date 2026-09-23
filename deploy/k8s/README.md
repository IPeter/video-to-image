# v2i on the home k3s cluster

`https://cardealer.peterilles.com` → Cloudflare (TLS) → Cloudflare Tunnel (`cloudflared`
systemd service on the node) → Traefik (`http://192.168.1.196:80`) → `v2i` namespace:

| Workload | What | Notes |
| --- | --- | --- |
| `web-cars` | Next.js frontend (`DOMAIN_KEY=cars`) | `/` on the host |
| `v2i-api` | `v2i serve` | `/v1`, `/openapi.json`, `/docs` on the host; runs `v2i migrate` as an init container |
| `v2i-worker` | `v2i work` (Gemini, FFmpeg, retention, cost guard) | only pod with the Gemini key |
| `postgres` | Postgres 17, 10 Gi `local-path` PVC | reachable only from api/worker (NetworkPolicy) |
| `v2i-data` | 50 Gi `local-path` PVC | originals, images, ZIPs (`V2I_STORAGE=local`) |

Everything on the host is behind Traefik **basic auth** (`basic-auth` Middleware,
realm "Car Listing Photos"). Metrics (`:4001/metrics`) are scraped by the existing
kube-prometheus-stack via the `v2i` ServiceMonitor and are not exposed on the host.

## Public route (outside Kubernetes)

- Cloudflare DNS: `cardealer` CNAME (proxied), like `home` and `monitor`.
- The locally managed tunnel on the node routes it: `/etc/cloudflared/config.yml` has
  `hostname: cardealer.peterilles.com → service: http://192.168.1.196:80` before the
  catch-all (added 2026-09-23; backup `config.yml.bak-20260923132128`). After editing:
  `cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate` and
  `sudo systemctl restart cloudflared`.
- A hostname without a tunnel rule gets an empty-body 404 from the tunnel's catch-all;
  Traefik's own 404 says "404 page not found".

## Deploy / redeploy

```bash
deploy/k8s/deploy.sh          # build (amd64), load onto the node, apply, wait
```

The script uses the current kubectl context. There is no registry: images are streamed
into the node's containerd through a short-lived `kubectl debug node` pod, and the last
two image tags are kept for rollback. First run creates the git-ignored secrets in
`overlays/home-k3s/`:

| File | Content |
| --- | --- |
| `secrets.env` | Postgres password + `DATABASE_URL`, internal token, IP-hash key. **Do not delete** once Postgres has data (the password is only applied on first init) |
| `gemini.env` | `GEMINI_API_KEY`, copied from the repo's `.env` |
| `basic-auth.users` / `basic-auth.password` | htpasswd line (bcrypt) / the plain login for you |

Settings (analyzer, limits, cost ceiling, trusted proxies) are in
`overlays/home-k3s/config.env`; prompts and shot lists in `configs/domains/cars.yaml`.
Change either and re-run the script — the generated ConfigMap names change, so the pods
roll automatically. To re-apply without rebuilding:
`SKIP_BUILD=1 TAG=<tag from kustomization.yaml> deploy/k8s/deploy.sh`.

## Login

```bash
cat deploy/k8s/overlays/home-k3s/basic-auth.password      # dealer:<password>
```

Change the password or add a user (one `user:hash` line per user), then re-apply:

```bash
htpasswd -nbB dealer 'new-password' | sed '/^$/d' > deploy/k8s/overlays/home-k3s/basic-auth.users
htpasswd -nbB peter  'another'      | sed '/^$/d' >> deploy/k8s/overlays/home-k3s/basic-auth.users
SKIP_BUILD=1 TAG=$(sed -n 's/^    newTag: //p' deploy/k8s/overlays/home-k3s/kustomization.yaml | head -1) deploy/k8s/deploy.sh
```

## Operate

```bash
kubectl -n v2i get pods
kubectl -n v2i logs deploy/v2i-worker -f          # pipeline (JSON logs, job_id per line)
kubectl -n v2i logs deploy/v2i-api -f
kubectl -n v2i exec -it statefulset/postgres -- psql -U v2i -d v2i \
  -c "select status, count(*) from jobs group by 1"
# Cost per day (internal token from secrets.env):
TOKEN=$(sed -n 's/^V2I_INTERNAL_TOKEN=//p' deploy/k8s/overlays/home-k3s/secrets.env)
kubectl -n v2i port-forward svc/v2i-api 4000:4000 &
curl -s -H "X-Internal-Token: $TOKEN" localhost:4000/internal/usage
```

Rollback: set `newTag` in `overlays/home-k3s/kustomization.yaml` to the previous tag
(still on the node) and run with `SKIP_BUILD=1 TAG=<previous>`.

Uninstall (deletes all jobs, images and the database):
`kubectl delete namespace v2i` (the `local-path` volumes are deleted with their PVCs).

## Limits of this setup

- **Uploads ≤ 95 MiB** (`V2I_MAX_UPLOAD_BYTES`): Cloudflare rejects request bodies over
  100 MB on Free/Pro plans, tunnels included. Longer or 4K phone videos exceed that;
  chunked uploads (split in the browser, reassembled by the API) would lift it without
  opening router ports.
- Client IPs come from Cloudflare's `CF-Connecting-IP`, trusted only from the Traefik pod
  network (`V2I_TRUSTED_PROXIES=10.42.0.0/16`). With the tunnel there are no open router
  ports, so only a device on the LAN talking to Traefik directly could spoof it — rate
  limits are a courtesy here, the basic auth is the gate.
- Single node: api and worker share the `v2i-data` volume. Backups are not configured.
