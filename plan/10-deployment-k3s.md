# 10 — Deployment on the home k3s cluster

First target (decision 10): the existing k3s cluster at home, for testing and customer previews.
Everything is plain Kubernetes: kustomize base + one overlay. Moving to a hosted cluster later is
a second overlay, not a rewrite.

## Workloads

```
namespace: v2i
┌────────────────────────────────────────────────────────────────────────┐
│ Ingress (Traefik, ships with k3s)                                      │
│   cars.<host>   → web-cars:3000                                        │
│   homes.<host>  → web-homes:3000              (Phase 4)                │
│   api.<host>    → v2i-api:4000                                         │
│   s3.<host>     → minio:9000     (presigned URLs; Phase 3 direct upload)│
└────────────────────────────────────────────────────────────────────────┘
        │                     │                      │
  Deployment web-cars   Deployment v2i-api     Deployment v2i-worker
  image: ghcr/…/web     image: ghcr/…/v2i      image: ghcr/…/v2i
  env DOMAIN_KEY=cars   args: [serve]          args: [work]
  replicas 1            replicas 1             replicas 1  (CPU-bound; scale by node)
                        ConfigMap domains      ConfigMap domains
                        Secret v2i-secrets     Secret v2i-secrets
                                               emptyDir /scratch (sizeLimit 10Gi)
        Job v2i-migrate  args: [migrate]  (runs on every deploy, before api/worker roll)

  StatefulSet postgres   (CloudNativePG cluster, or bitnami chart, PVC local-path 20Gi)
  StatefulSet minio      (single node, PVC local-path 200Gi, bucket "v2i")
```

One container image `v2i` for api, worker and migrate; the subcommand is the container `args`.

## Manifests layout

```
deploy/k8s/
  base/
    kustomization.yaml
    namespace.yaml
    api-deployment.yaml      api-service.yaml
    worker-deployment.yaml
    migrate-job.yaml
    web-deployment.yaml      web-service.yaml     (one pair per domain via kustomize components or name suffix)
    ingress.yaml
    configmap-domains.yaml   (configMapGenerator from ../../configs/domains/*.yaml)
    servicemonitor.yaml      (if kube-prometheus-stack is present)
  components/
    domain-web/              reusable component: Deployment+Service+Ingress for one frontend, parameterised by DOMAIN_KEY and host
  overlays/
    home-k3s/
      kustomization.yaml     images: tags; patches: hosts, resources, storageClass local-path
      secrets.yaml           SealedSecret or SOPS-encrypted: GEMINI_API_KEY, DATABASE_URL, S3_* creds
      postgres.yaml          CloudNativePG Cluster (or a StatefulSet if the operator is not installed)
      minio.yaml
      certificates.yaml      cert-manager Certificate per host
```

`configMapGenerator` reads the domain YAML files straight from `configs/domains/`, so the
repo's config directory is the source of truth in dev and on the cluster. Kustomize appends a
hash to the ConfigMap name, which rolls the api and worker pods automatically on change.

## Key settings

| Item | Value | Why |
| --- | --- | --- |
| Ingress body size | Traefik `buffering` middleware disabled for the upload route, or `maxRequestBodyBytes: 524288000` | Phase 1 streams uploads through the API; Phase 3 moves to presigned direct upload and the limit becomes irrelevant |
| Ingress timeouts | 10 min read for `api.<host>` | Long uploads on home upstream bandwidth |
| Worker resources | requests 1 CPU / 1 Gi, limits 4 CPU / 4 Gi | FFmpeg is the hog; measure on the real node |
| API resources | requests 100m / 128 Mi, limits 1 CPU / 512 Mi | Thin service |
| Worker scratch | `emptyDir` with `sizeLimit: 10Gi` at `/scratch`, `V2I_SCRATCH_DIR=/scratch` | 300 full-res JPEGs + preview per job; deleted per job |
| Probes | api: readiness `/healthz`, liveness `/livez`; worker: liveness on a small `/livez` port, no readiness | |
| Postgres | CloudNativePG if installed (backups, PITR), else a simple StatefulSet; `local-path` storage class | Home cluster; single node is fine for previews |
| MinIO | single-node StatefulSet, bucket `v2i` created by an init Job; lifecycle rule as a safety net (expire objects after 3 days) | Retention is enforced by the app; MinIO lifecycle catches leaks |
| Secrets | **SealedSecrets** (simplest on a single home cluster) or **SOPS + age** | Never commit plaintext; the `GEMINI_API_KEY` is the sensitive one |
| TLS | cert-manager, Let's Encrypt with DNS-01 (Cloudflare) if hosts are public DNS; otherwise a local CA issuer and a trusted root on preview devices | Customer previews need a green lock |
| Images | GHCR, tags `sha-<gitsha>` + `main`; k3s pulls with an `imagePullSecret` | CI builds on push to `main` |
| Deploy | `kubectl apply -k deploy/k8s/overlays/home-k3s` from CI (self-hosted runner on the LAN, or `kubectl` over Tailscale) or manually; ArgoCD later if wanted | Keep it simple first |

## Frontend runtime config

`NEXT_PUBLIC_*` is baked at build time, which would force one image per domain. Instead the web
container reads `DOMAIN_KEY` and `API_URL` from env at runtime and exposes them at
`/api/config`. One image, one Deployment per domain (decision 13).

## Direct-to-MinIO upload (Phase 3)

1. `POST /v1/domains/cars/uploads` → API creates a job in `uploading` state and returns a
   presigned PUT URL for `cars/jobs/<id>/original.mp4` plus the job id.
2. Browser PUTs the file to `s3.<host>` with progress.
3. `POST /v1/domains/cars/jobs/<id>/complete` → API `Head`s the object, downloads it to scratch
   for ffprobe (or ffprobe reads the presigned GET URL directly), validates, and enqueues.
   `uploading` becomes `queued` or the job is rejected with the usual codes.

The job state machine gains one state (`uploading`) and a periodic sweep expires `uploading`
jobs older than 1 hour.

## Observability

- `/metrics` on api and worker; ServiceMonitor if kube-prometheus-stack exists on the cluster,
  otherwise a plain Prometheus scrape config.
- Dashboards: jobs by status over time, step duration p50/p95, model latency, daily `cost_usd`,
  queue depth (River exposes counts via SQL; a small gauge in the worker).
- Alerts: `failed` ratio > 10% over 1 h; daily cost > ceiling; worker pod restarts; PVC > 80%.
- Logs: JSON `slog` to stdout; `kubectl logs` is enough to start, Loki later.

## Local cluster workflow

- `make image` builds both images with the local Docker; `make load` pushes to the k3s
  containerd (or use a local registry on the cluster) for fast iteration without GHCR.
- Optional **Tilt** `Tiltfile` for live-reload of the Go binary into the cluster.
- `docker compose` remains the zero-cluster path for quick pipeline work.

## Moving off the home cluster later

New overlay `overlays/hosted`: different hosts, storage class, a managed Postgres URL and an S3
endpoint in the Secret, replica counts. The base does not change. Data migration is `pg_dump`
plus an `mc mirror` of the bucket, both of which are optional given the 24-hour retention.
