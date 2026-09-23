# 02 — Tech Stack

## Decision: Go backend

Decided 2026-09-23 (see `09-decisions.md`). The language matters less than the ecosystem
around it because FFmpeg and Gemini do the heavy work; Go was chosen for a single static binary,
simple concurrency, and a small container image that fits a home k3s cluster well.

The one real cost of Go versus TypeScript is sharing types with the Next.js frontend. This plan
closes that gap with **OpenAPI generated from the Go handlers** and TypeScript types generated
from that spec, so the contract is still defined once, in Go.

| Criterion | Go | TypeScript (Node) | Python |
| --- | --- | --- | --- |
| Shelling out to FFmpeg | Excellent | Good | Good |
| Gemini SDK | Official (`google.golang.org/genai`) | Official | Official |
| Job queue | **River** (Postgres), Asynq (Redis) | BullMQ (Redis) | Celery / arq |
| Image analysis libs | stdlib `image` + small libs; enough for sharpness and dHash | `sharp` | OpenCV |
| Share types with frontend | via OpenAPI codegen | directly | via OpenAPI codegen |
| Single binary, small image | **Yes (~30 MB with FFmpeg ~100 MB)** | No | No |
| Concurrency | goroutines | event loop | processes |

When to add Python: only if frame scoring later needs real computer vision (detect the car,
crop, blur plates). Then a small Python scoring service called by the worker over HTTP.

## Backend stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Language | **Go 1.23+** | Modules, `slog`, generics |
| Binary layout | **one binary `v2i`** with subcommands `serve`, `work`, `migrate`, `eval` | One image, different `args` per k8s Deployment. `cobra` for the CLI |
| HTTP | **Huma v2** on **chi** | Handlers are typed Go structs; Huma validates input and emits OpenAPI 3.1 at `/openapi.json`. Multipart upload handled via a raw chi route |
| Queue | **River** (`riverqueue.com/river`) | Postgres-backed, transactional enqueue, retries with backoff, per-queue concurrency, periodic jobs. Removes Redis from the stack |
| DB access | **pgx v5 + sqlc** | Type-safe queries generated from SQL. Migrations with **goose** (embedded, run by `v2i migrate` as an init container) |
| Config | **env → struct** via `caarlos0/env`; domain YAML via `gopkg.in/yaml.v3` + `go-playground/validator` | Fail fast on missing `GEMINI_API_KEY`, bad YAML |
| Storage | **AWS SDK v2 S3 client** behind a `Storage` interface; local-disk implementation for dev/tests | MinIO on k3s, any S3 later |
| Video | **FFmpeg / ffprobe** via `os/exec` with context timeouts | Plain args, no wrapper library. Pinned version in the worker image |
| Images | stdlib `image/jpeg`, `golang.org/x/image/draw` for resize, `corona10/goimagehash` for dHash | Laplacian variance is a 20-line loop. FFmpeg produces the 640 px scoring copies, so Go decodes small images only |
| Model | **`google.golang.org/genai`** behind an `Analyzer` interface, plus `FakeAnalyzer` (canned JSON) and `RecordingAnalyzer` (writes responses to disk for eval replays) | Files API upload, `ResponseSchema` for structured JSON |
| ZIP | stdlib `archive/zip` streaming into a storage `io.Pipe` | No temp file |
| IDs | **ULID** (`oklog/ulid`) with type prefixes `job_`, `img_`, `cf_` | Unguessable enough to act as the result handle in Phase 1 |
| Logging | **`log/slog`** JSON | Every line carries `job_id`, `domain`, `step` |
| Metrics | **Prometheus** `/metrics` via `promhttp` | Jobs by status, step durations, model latency, cost counter. k3s-friendly |
| Rate limiting | in-memory token bucket per IP per domain (`golang.org/x/time/rate`) | Single API replica in Phase 1; move to Postgres counters if replicas > 1 |
| Tests | stdlib `testing` + `testify`; fixture frames under `testdata/`; API integration test with `FakeAnalyzer` and local storage; `dockertest` or a compose Postgres for store tests | |

## Frontend stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Framework | **Next.js (App Router)**, `output: "standalone"` | Small container image, runs on k3s |
| Styling | **Tailwind CSS** | Design tokens per domain via CSS variables |
| Components | **shadcn/ui** | Reskinnable later, accessible by default |
| Data fetching | **TanStack Query** | Polling with backoff, SSE in Phase 2 |
| API types | **`openapi-typescript`** generating `web/src/lib/api.d.ts` from the Go `/openapi.json`; **`openapi-fetch`** as the typed client | Regenerated in CI; PR fails if the frontend uses a field the API no longer has |
| Upload | `XMLHttpRequest` for progress | Presigned direct-to-MinIO upload in Phase 3 |

Alternative considered: **SvelteKit**. Lighter, but the polished-component ecosystem (shadcn,
Radix, motion) is deeper in React, and design will be a differentiator.

## Repository layout

Single repo. Go module at the root, Next.js app in `web/`, Kubernetes manifests in `deploy/`.

```
video-to-image/
├── go.mod
├── cmd/v2i/                    main.go + cobra subcommands: serve, work, migrate, eval
├── internal/
│   ├── config/                 env config
│   ├── domain/                 DomainConfig types, YAML loader, validation, registry
│   ├── httpapi/                Huma routes, middleware (origin check, rate limit), error mapping
│   ├── store/                  sqlc output + repository functions
│   ├── queue/                  River client, job args types, periodic jobs
│   ├── storage/                Storage interface, s3 + local implementations
│   ├── media/                  ffprobe/ffmpeg wrappers with typed args
│   ├── pipeline/
│   │   ├── preview/  analyze/  extract/  score/  selectr/  export/
│   │   └── run.go              orchestrates steps, writes job_steps
│   ├── analyzer/               Analyzer interface, gemini/, fake/, recording/
│   └── pricing/                model → price table with reference date
├── migrations/                 goose SQL files (embedded)
├── configs/domains/            cars.yaml, homes.yaml ...
├── web/                        Next.js app
├── deploy/
│   ├── docker/                 Dockerfile.v2i (with FFmpeg), Dockerfile.web
│   └── k8s/                    kustomize base + overlays/home-k3s (see 10-deployment-k3s.md)
├── testdata/                   fixture videos and frames (git LFS or download script)
├── docker-compose.yml          Postgres + MinIO for local dev
├── Makefile                    dev, test, gen (sqlc + openapi), build, deploy
├── docs/                       product brief
└── plan/                       this folder
```

## Local development

- `docker compose up -d` → Postgres and MinIO.
- `make gen` → sqlc code, `/openapi.json` snapshot, TypeScript types.
- `go run ./cmd/v2i migrate && go run ./cmd/v2i serve --embed-worker` → API + worker in one
  process on `:4000`, FFmpeg from the host `PATH`.
- `cd web && pnpm dev` → frontend on `:3000` with `NEXT_PUBLIC_DOMAIN_KEY=cars`.
- `go run ./cmd/v2i eval --dir testdata/cars --analyzer=recorded` → offline pipeline runs.

Optional: **Tilt** or **Skaffold** to run the whole thing on the local k3s instead of compose,
once the manifests exist. Compose stays the zero-cluster path.
