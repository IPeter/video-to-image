# v2i on the home k3s cluster

`https://cardealer.peterilles.com` → Cloudflare (TLS) → Cloudflare Tunnel (`cloudflared`
systemd service on the node) → Traefik (`http://192.168.1.196:80`) → `v2i` namespace:

| Workload | What | Notes |
| --- | --- | --- |
| `web-cars` | Next.js frontend (`DOMAIN_KEY=cars`) | `/` on the host |
| `v2i-api` | `v2i serve` | `/v1`, `/openapi.json`, `/docs` on the host; runs `v2i migrate` as an init container |
| `v2i-worker` | `v2i work` (Gemini, FFmpeg, retention, cost guard) + `studio` sidecar | only pod with the Gemini key; the sidecar cuts cars out for studio backgrounds (below) |
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
deploy/k8s/deploy.sh                          # build (amd64), load onto the node, apply, wait
SKIP_BUILD=1 deploy/k8s/deploy.sh             # re-apply the tags in kustomization.yaml
SKIP_BUILD=1 TAG=<tag> deploy/k8s/deploy.sh   # v2i + v2i-web at an older tag (rollback)
STUDIO_TAG=<tag> deploy/k8s/deploy.sh         # keep/pin the studio sidecar image, no studio build
```

The script uses the current kubectl context. There is no registry: images are streamed
into the node's containerd through a short-lived `kubectl debug node` pod, and the
current and previous tag of each image are kept for rollback. Three images: `v2i` and
`v2i-web` (tag `TAG`, new on every build) and `v2i-studio` (the sidecar; before building
it the script runs `services/studio/fetch-models.sh`, which fails when a model file is
missing or has the wrong sha256). The studio image is ~1 GB and rarely changes: when a
rebuild gives the identical image, its previous tag is kept and nothing is streamed.
With `SKIP_BUILD`, images already on the node are not streamed again. First run creates
the git-ignored secrets in `overlays/home-k3s/`:

| File | Content |
| --- | --- |
| `secrets.env` | Postgres password + `DATABASE_URL`, internal token, IP-hash key. **Do not delete** once Postgres has data (the password is only applied on first init) |
| `gemini.env` | `GEMINI_API_KEY`, copied from the repo's `.env` |
| `basic-auth.users` / `basic-auth.password` | htpasswd line (bcrypt) / the plain login for you |

Settings (analyzer, limits, cost ceiling, trusted proxies, studio sidecar URL) are in
`overlays/home-k3s/config.env`; prompts, shot lists and the `studio:` block in
`configs/domains/cars.yaml`. Change either and re-run the script — the generated
ConfigMap names change, so the pods roll automatically. To re-apply without rebuilding:
`SKIP_BUILD=1 deploy/k8s/deploy.sh`.

## Login

```bash
cat deploy/k8s/overlays/home-k3s/basic-auth.password      # dealer:<password>
```

Change the password or add a user (one `user:hash` line per user), then re-apply:

```bash
htpasswd -nbB dealer 'new-password' | sed '/^$/d' > deploy/k8s/overlays/home-k3s/basic-auth.users
htpasswd -nbB peter  'another'      | sed '/^$/d' >> deploy/k8s/overlays/home-k3s/basic-auth.users
SKIP_BUILD=1 deploy/k8s/deploy.sh
```

## Operate

```bash
kubectl -n v2i get pods
kubectl -n v2i logs deploy/v2i-worker -f          # pipeline (JSON logs, job_id per line)
kubectl -n v2i logs deploy/v2i-worker -c studio -f  # studio sidecar (one JSON line per request)
kubectl -n v2i logs deploy/v2i-api -f
kubectl -n v2i exec -it statefulset/postgres -- psql -U v2i -d v2i \
  -c "select status, count(*) from jobs group by 1"
# Cost per day (internal token from secrets.env):
TOKEN=$(sed -n 's/^V2I_INTERNAL_TOKEN=//p' deploy/k8s/overlays/home-k3s/secrets.env)
kubectl -n v2i port-forward svc/v2i-api 4000:4000 &
curl -s -H "X-Internal-Token: $TOKEN" localhost:4000/internal/usage
```

Rollback: `SKIP_BUILD=1 TAG=<previous> deploy/k8s/deploy.sh` (the previous tag is still
on the node; `STUDIO_TAG=<previous studio tag>` for the sidecar, whose tag differs).

Uninstall (deletes all jobs, images and the database):
`kubectl delete namespace v2i` (the `local-path` volumes are deleted with their PVCs).

## Studio sidecar

Studio backgrounds (exterior photos on a grey/light/dark backdrop, car pixels unchanged)
need a cut-out mask of the car. The `studio` container in the worker pod
(`services/studio`: BiRefNet-general-lite + SAM 2.1 base-plus on ONNX Runtime, both
models baked into the image) computes it; the worker's River queue `studio` (one job at a
time, `V2I_STUDIO_WORKERS=1`) calls it after a job has completed, so users get the normal
photos first and the studio versions appear on the same page afterwards.

- **Network**: it listens on `127.0.0.1:4100` only (`V2I_STUDIO_URL` in `config.env`); no
  photo leaves the pod. The kubelet cannot reach a localhost listener with `httpGet`, so
  the probes are `exec` GETs inside the container (a bare socket under `python3 -S`: a
  probe shares the container's CPU quota with a running inference): startup (`/readyz`,
  up to 10 min to load and warm up the models — ~20 s normally, 6 min seen on a
  contended laptop), readiness (`/readyz`), liveness (`/healthz`, generous).
- **Independent of the worker**: a plain container, not an init/native sidecar. The
  worker container starts and processes pipeline jobs while the sidecar is loading or
  restarting; the studio job retries (per photo `V2I_STUDIO_TIMEOUT`, 45 s). The pod only
  reports Ready once the models are loaded, so `deploy.sh` waits for that; the
  `v2i-worker` Service publishes not-ready addresses, so metrics are scraped anyway. On
  pod stop the sidecar keeps serving until the worker's admin port closes (≤ 35 s), so
  the photo in progress can finish.
- **Resources** (node: 12 CPUs, 15 GiB):

  | Container | Requests | Limits |
  | --- | --- | --- |
  | `worker` | 1 CPU, 1 GiB | 4 CPU, 4 GiB |
  | `studio` | 500m, 1.5 GiB | 4 CPU, 3.5 GiB |

  The sidecar needs ~2.7 GB with both models running at once (local arm64 run on a
  2.8 MP photo: OOM-killed under a 2.6 GB limit, fine under 3.0 GB); the spike saw
  2.5–2.9 GB on 2–8 MP frames running them one after the other. The peaks in
  `/v1/info` grow up to whatever limit it has (mapped model files count), so watch for
  OOM restarts instead:
  `kubectl -n v2i get pod -l app.kubernetes.io/name=v2i-worker -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="studio")].lastState}'`.
  If they happen, raise the limit (4 GiB still fits the node) or set `STUDIO_PARALLEL=false`.

  The worker Deployment uses `strategy: Recreate`: a surge pod during a rollout would
  double the pod's 7.5 GiB of limits. The sidecar raises the node's summed memory
  limits (all namespaces, 2026-09-23) from ~9.7 to ~13.2 GiB of 14.9 GiB allocatable.
- **Image**: `localhost/v2i-studio`, ~1 GB: python:3.12-slim + onnxruntime/numpy/
  OpenCV/Pillow + ~0.6 GB of models (BiRefNet-lite 181 MB, SAM 2.1 encoder fp32 306 MB
  and int8 99 MB, decoder 21 MB; `services/studio/models.sha256`, checked at build
  time). The models are not in git: `services/studio/fetch-models.sh` copies them from
  the research spike or downloads them (SAM from Hugging Face at a pinned revision;
  BiRefNet from `STUDIO_BIREFNET_URL`) and verifies the hashes.
- **Tuning** (env of the `studio` container in `base/worker.yaml`): `STUDIO_THREADS`
  (4, = the CPU limit; BiRefNet and SAM run concurrently and share them),
  `STUDIO_SAM_ENCODER` (`fp32` or `int8`), `STUDIO_PARALLEL` (`false` runs the models
  one after the other).
- **Inspect / benchmark on the node**: `kubectl port-forward` connects to the pod's
  localhost, so it reaches the sidecar (only from your machine, with cluster access):

  ```bash
  kubectl -n v2i port-forward deploy/v2i-worker 4100:4100 &
  curl -s localhost:4100/v1/info | jq .          # phase, requests, threads, SAM encoder
  # cold + warm timings on the public spike fixtures only (the user0x frames stay local)
  python3 services/studio/bench.py --url http://127.0.0.1:4100 --only pub
  ```

**Turning it off**:

- one domain: `studio.enabled: false` in `configs/domains/<key>.yaml` (the sidecar idles);
- the whole feature: `V2I_STUDIO_URL=` (empty) in `overlays/home-k3s/config.env`;
- to also free the sidecar's memory, remove the container in the overlay and deploy
  with an old `STUDIO_TAG` (or any tag on the node) to skip its build:

  ```yaml
  patches:
    - target: { kind: Deployment, name: v2i-worker }
      patch: |-
        - op: remove
          path: /spec/template/spec/containers/1
  ```

## Limits of this setup

- **Uploads are chunked**: Cloudflare rejects request bodies over 100 MB on Free/Pro
  plans, tunnels included, so the browser sends videos in 16 MiB pieces
  (`V2I_UPLOAD_CHUNK_BYTES`) that the API reassembles in its scratch volume; the domain's
  `maxUploadBytes` (500 MB for cars) is the real limit. The single-request upload
  (`POST /v1/domains/{key}/jobs`, used by scripts) stays capped at 95 MiB
  (`V2I_MAX_REQUEST_BYTES`). Unfinished uploads are dropped after 15 minutes idle
  (`V2I_UPLOAD_SESSION_TTL`). Uploads in progress live in the API pod's scratch
  emptyDir: a deploy (new pod) makes them fail with "upload expired", and the user
  uploads again.
- Client IPs come from Cloudflare's `CF-Connecting-IP`, trusted only from the Traefik pod
  network (`V2I_TRUSTED_PROXIES=10.42.0.0/16`). With the tunnel there are no open router
  ports, so only a device on the LAN talking to Traefik directly could spoof it — rate
  limits are a courtesy here, the basic auth is the gate.
- Single node: api and worker share the `v2i-data` volume. Backups are not configured.
- Studio timings on this node are estimates from the research spike (8–12 s per photo
  on amd64): run `bench.py` through a port-forward (above) after the first deploy, and
  check `timingsMs` in the sidecar's logs on the first real jobs.
