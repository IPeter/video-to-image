# Video-to-Image Platform (v2i)

One backend, many domain frontends. A frontend (e.g. `cars`) uploads a short video; the
backend checks that the video belongs to the domain, finds the relevant scenes with a
vision model, extracts sharp, non-duplicate frames from the original with FFmpeg, and
exports them as JPGs plus a ZIP. For cars it can also put exterior photos on a studio
backdrop without touching the car ([studio backgrounds](#studio-backgrounds)).

- Product brief: [`docs/main-app.md`](docs/main-app.md)
- Architecture and plan: [`plan/`](plan/README.md) (start with `plan/README.md`, then `08-roadmap.md`)
- Status: **Phase 0 (foundations) and Phase 1 (end-to-end MVP for `cars`)** are implemented.
  Phase 2 (eval tool, SSE, tuning), Phase 3 (k3s manifests, direct-to-MinIO upload) and
  later phases are not started yet. Deviations from the plan and open items:
  [`plan/11-implementation-notes.md`](plan/11-implementation-notes.md).

## Layout

```
cmd/v2i/                 single binary: serve | work | migrate | eval (Phase 2 stub) | openapi
internal/config          env → struct
internal/domain          domain YAML types, loader, validation, registry, public subset
internal/httpapi         Huma v2 on chi: routes, origin/CORS guard, rate limits, upload, downloads
internal/store           Postgres repository (pgx + sqlc; generated code in store/sqlcdb)
internal/queue           River clients, pipeline worker, periodic maintenance jobs
internal/storage         Storage interface: local disk + S3/MinIO
internal/media           ffprobe/ffmpeg wrappers, container sniffing
internal/analyzer        model boundary: gemini/, fake/, recording/ (record + replay), setup/
internal/pipeline        runner.go orchestrates preview → analyze → extract → score → select → export
internal/pricing         model price table (reference date 2026-09-23)
internal/metrics         Prometheus collectors
internal/studio          studio backgrounds: gates, box mapping, compositor, QA check, sidecar client
migrations/              goose SQL (embedded into the binary)
configs/domains/         one YAML file per domain (cars.yaml)
api/openapi.json         generated OpenAPI 3.1 document (source of the frontend types)
web/                     Next.js frontend (one image, deployed once per domain)
services/studio/         segmentation sidecar (Python, ONNX Runtime: BiRefNet-lite + SAM 2.1) for studio backgrounds
deploy/docker/           Dockerfile.v2i (with FFmpeg), Dockerfile.web
deploy/k8s/              kustomize base + home-k3s overlay, deploy.sh (see deploy/k8s/README.md)
scripts/gen-fixtures.sh  synthetic test videos into testdata/videos
```

## Quick start (local)

Requirements: Go 1.27+, FFmpeg/ffprobe on `PATH`, Docker (for Postgres + MinIO), Node 24+.

```bash
cp .env.example .env          # defaults: fake analyzer, local disk storage
make deps-up                  # Postgres on :55432, MinIO on :59000 (console :59001)
make dev-fake                 # migrate + API with embedded worker on :4000, no model key needed
make web-dev                  # frontend on http://localhost:3000 (cars domain)
```

With a real model: set `GEMINI_API_KEY` and `V2I_ANALYZER=gemini` in `.env`, then `make dev`.

Try the API directly:

```bash
make fixtures                                   # testdata/videos/*.mp4 (synthetic)
curl -s localhost:4000/v1/domains/cars | jq .
curl -s -H 'Origin: http://localhost:3000' \
     -F video=@testdata/videos/car-walkaround-like.mp4 \
     localhost:4000/v1/domains/cars/jobs         # → 202 {"jobId":"job_...","status":"queued"}
curl -s localhost:4000/v1/domains/cars/jobs/<jobId> | jq .
```

The fake analyzer returns canned scenes for every shot; a file name containing
`reject` makes it fail the domain gate, so both outcomes can be tested for free.

## Studio backgrounds

Exterior photos (front/side/rear views that the frame review found fully visible) get a
second version on a studio backdrop — grey showroom (default), light or dark. The car is
**not** retouched or redrawn: a cut-out mask comes from a local segmentation sidecar
(BiRefNet-general-lite, MIT, plus SAM 2.1 base-plus, Apache-2.0, as a "rescue union"
that only ever adds car pixels back), and the composite is
`alpha·original + (1−alpha)·backdrop·shadow`, so every car pixel is the original pixel
(scratches, dents, rust and dirt stay visible). Mask checks and a model QA check
withhold the studio version when a part of the car may be missing; the original is never
modified and stays the default. Studio versions are labelled "Studio background · car
unchanged".

The studio work runs as a separate River job (queue `studio`) after the job has
completed: the normal photos appear as before, the studio versions follow on the same
page. Routes: `GET /v1/domains/{d}/jobs/{j}/images/{i}/studio/{style}` (`?download=1`),
`…/studio/{style}/thumb`, `GET /v1/domains/{d}/jobs/{j}/studio.zip`.

Locally (needs Docker; the models, ~0.6 GB, are not in git):

```bash
make studio-up        # fetch + verify the models, build and start the sidecar on 127.0.0.1:4100
# .env: V2I_STUDIO_URL=http://127.0.0.1:4100 (and studio.enabled: true in the domain YAML)
make dev-fake         # or make dev
make studio-down
```

`V2I_STUDIO_URL=fake` uses a built-in fake segmenter (tests, e2e) without the sidecar;
empty turns the feature off. On the cluster the sidecar runs in the worker pod
([`deploy/k8s/README.md`](deploy/k8s/README.md#studio-sidecar)).

## Commands

| Command | What it does |
| --- | --- |
| `v2i serve [--embed-worker] [--analyzer=…]` | HTTP API on `V2I_HTTP_ADDR` (`:4000`); `/livez` + `/metrics` on `V2I_ADMIN_ADDR` (`:4001`). `--embed-worker` also runs the pipeline in-process |
| `v2i work [--analyzer=…]` | River worker: pipeline queue + periodic retention cleanup, orphan reconciler, cost guard, stats, model-files sweep. `/livez` and `/metrics` on `V2I_ADMIN_ADDR` (`:4001`) |
| `v2i migrate` | River + goose migrations (idempotent; a Kubernetes Job before each rollout) |
| `v2i openapi -o api/openapi.json` | Writes the OpenAPI document generated from the Go handlers |
| `v2i eval` | Phase 2 (not implemented yet) |

Analyzers: `gemini` (real calls), `fake` (canned, free), `recording` (real calls, responses
saved to `V2I_RECORDINGS_DIR`), `recorded` (replays saved responses, no network).

## Configuration

All deployment settings are environment variables; see [`.env.example`](.env.example) and
`internal/config/config.go`. Production-relevant ones: `DAILY_COST_CEILING_USD` (default
5; pauses the pipeline), `V2I_TRUSTED_PROXIES` (CIDRs of the ingress, so client IPs and
rate limits are right behind Traefik), `V2I_INTERNAL_TOKEN` (≥ 24 random characters with a
paid analyzer), `V2I_IP_HASH_SALT`, `V2I_MAX_CONCURRENT_UPLOADS`, `V2I_MAX_REQUEST_BYTES`
(the ingress body limit; browsers upload in `V2I_UPLOAD_CHUNK_BYTES` chunks below it),
`FFMPEG_THREADS`, `V2I_EXTRACT_CONCURRENCY`, `V2I_STUDIO_URL` (segmentation sidecar;
empty = studio backgrounds off), `V2I_STUDIO_TIMEOUT` (per photo, 45s),
`V2I_STUDIO_WORKERS`. Domain behaviour (prompts, shot list, limits, selection weights,
retention, copy, the `studio:` block) lives in `configs/domains/<key>.yaml` and is
validated at startup — a bad file stops the process. Adding a domain is one YAML file plus one
frontend deployment with `DOMAIN_KEY=<key>` (see `plan/04-domain-config.md`).

## API

Contract: [`plan/05-api.md`](plan/05-api.md); the live OpenAPI document is served at
`/openapi.json` (interactive docs at `/docs`). Every error body is
`{"error":{"code","message"}}` with a stable `code`.

Domain routes (`/v1/domains/{key}/…`) check the domain key and the request `Origin`
against `allowedOrigins`. Browsers always send `Origin` on cross-origin POST/DELETE, so
those require an allowed Origin (or `X-Internal-Token` for non-browser clients); GETs
without an Origin (image tags, downloads, server-side fetches) are allowed.

Uploads are limited per domain (size, duration, resolution, frame rate — see
`configs/domains/cars.yaml`), rate limited per client IP, and admitted only while there
are free upload slots and scratch disk space.

## Development

```bash
make test         # go test -race ./... (Postgres tests use V2I_TEST_DATABASE_URL; `make deps-up` first)
make test-short   # skip Postgres tests
make lint         # go vet + golangci-lint
make gen          # sqlc + api/openapi.json + web/lib/api.d.ts
make build        # bin/v2i
make image        # both container images
make studio-test  # studio sidecar unit tests (pytest, no models needed)
make studio-models / studio-build / studio-up / studio-down   # the sidecar in docker compose
make e2e          # Playwright (next dev) against a running `make dev-fake`
make e2e-prod     # same against the production standalone build
```

S3 storage tests run against the compose MinIO when `V2I_TEST_S3_ENDPOINT=http://localhost:59000`.

Generated code is committed and checked in CI: `internal/store/sqlcdb` (`sqlc diff`),
`api/openapi.json` and `web/lib/api.d.ts` (drift checks). If you change a handler DTO,
run `make gen` and commit the result — the frontend build then fails on any field the
API no longer has.

## Observability

- Logs: JSON `slog` on stdout; every pipeline line carries `job_id`, `domain`, `step`.
- Metrics (`/metrics` on the admin port, not the public API port): HTTP requests/latency by route, jobs created/finished by status,
  step durations, model latency/tokens/cost, daily cost, queue depth, table sizes.
- Cost: one `usage_events` row per model call with tokens and USD (`internal/pricing`);
  `GET /internal/usage?from=&to=` (with `X-Internal-Token`) sums them per domain per day.
  `DAILY_COST_CEILING_USD` pauses the pipeline queue once the day's spend reaches it.
