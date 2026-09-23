# 01 — Architecture

## System overview

```
 cars.example.com          homes.example.com          (future domains)
 ┌───────────────┐         ┌───────────────┐
 │ Next.js app   │         │ Next.js app   │   same image, different
 │ DOMAIN=cars   │         │ DOMAIN=homes  │   env + theme per Deployment
 └──────┬────────┘         └──────┬────────┘
        │  HTTPS (JSON + multipart upload) via Traefik ingress
        ▼                         ▼
 ┌─────────────────────────────────────────────────┐
 │ v2i serve  (Go, Huma/chi HTTP)                  │
 │  - resolves domain from path + Origin allowlist │
 │  - accepts upload, runs ffprobe validation      │
 │  - inserts job row + River job (same tx)        │
 │  - serves job status, results, images, ZIP      │
 └──────────────┬──────────────────────────────────┘
                │ Postgres (River job queue tables)
                ▼
 ┌─────────────────────────────────────────────────┐
 │ v2i work  (Go, River worker)                    │
 │  1. build low-res preview (FFmpeg)              │
 │  2. Gemini: domain gate + scene candidates      │
 │  3. FFmpeg: dense frame sampling in candidates  │
 │  4. sharpness + dHash dedupe (Go image loops)   │
 │  5. final selection by shot coverage + score    │
 │  6. export JPGs + ZIP, write results            │
 │  + periodic: retention cleanup                  │
 └──────────────┬──────────────────────────────────┘
                │
   ┌────────────┼──────────────┐
   ▼            ▼              ▼
 Postgres     Object storage   Gemini API
 (jobs,       (MinIO on k3s,   (Files API for
  results,     S3 later)        video upload)
  usage,
  queue)
```

`serve` and `work` are subcommands of the **same Go binary and container image**. Locally you
can run `v2i serve --embed-worker` to get both in one process. On k3s they are two Deployments
so the worker (which needs FFmpeg, CPU and scratch disk) scales and restarts independently of
the API. Only Postgres and MinIO are additional services; there is no Redis.

## Components

### Frontend (one Deployment per domain)
- Uploads a video, shows progress, previews results with timestamps, downloads images/ZIP.
- Knows its `domainKey` from an environment variable. It never sends a prompt. The prompt lives
  on the backend in the domain config.
- Talks only to the API. No model credentials ever reach the browser.

### API (`v2i serve`)
- Thin. Validates input, persists state, enqueues work, serves results.
- Domain resolution: the path carries the domain key
  (`/v1/domains/cars/jobs`) and the request `Origin` must be in that domain's allowlist.
  Two independent checks so a misconfigured frontend fails loudly instead of processing under
  the wrong prompt.
- Streams uploads to storage rather than buffering in memory. Rejects by size early using
  `Content-Length` and the domain's `maxUploadBytes`, and enforces the limit on the stream too.
- Job creation inserts the `jobs` row and the River job in one transaction, so a job can never
  exist without work scheduled or vice versa.

### Worker (`v2i work`)
- Pulls jobs from River, runs the pipeline in `03-pipeline.md`, writes step-level progress and
  per-step token/cost usage.
- Every step is a separate Go package with a narrow interface (the brief asks for this):
  `preview`, `analyze`, `extract`, `score`, `selectr`, `export`. Each is testable with fixtures
  and swappable (e.g. an `Analyzer` implementation for a different model provider, and a
  `FakeAnalyzer` for offline tests).
- Also runs River **periodic jobs**: retention cleanup every 10 minutes, Gemini Files sweep daily.

### Domain registry
- A directory of YAML files (`configs/domains/cars.yaml`, `homes.yaml`) parsed into a typed
  `map[string]DomainConfig` at startup and validated. A bad file crashes startup.
- On k3s the directory is a ConfigMap mounted into both `serve` and `work`. Changing a prompt
  is a ConfigMap change and a rollout restart, not a code change.
- Full spec in `04-domain-config.md`.

### Storage
- Go interface with two implementations: local disk (dev, tests) and S3-compatible via the
  AWS SDK v2 (MinIO on k3s, any S3 later). Layout in `06-data-model.md`.

### Database
- Postgres. Tables: `jobs`, `job_steps`, `scenes`, `candidate_frames`, `selected_images`,
  `usage_events`, plus River's own tables. Schema in `06-data-model.md`.

### Model provider
- Gemini via the official `google.golang.org/genai` SDK, wrapped behind an `Analyzer` interface.
  Model name is read from domain config with a global default.

## End-to-end request flow

1. User picks a video in the cars frontend. Client checks size and MIME type for fast feedback.
2. `POST /v1/domains/cars/jobs` with multipart body. API checks Origin, size, MIME, streams
   the file to storage, runs `ffprobe` to confirm it decodes and read duration/resolution/fps.
   Rejects with a specific error code if invalid. Inserts job + queue entry. Returns `202 { jobId }`.
3. Frontend polls `GET /v1/domains/cars/jobs/:id` (Phase 2: SSE) and renders step progress.
4. Worker runs the pipeline. If the domain gate fails, the job ends in `rejected` with the
   model's reason ("This video shows a kitchen, not a vehicle."). No frames are extracted.
5. On success, job is `completed` with a list of images (URL, timestamp, shot label, scores,
   explanation) and a ZIP URL.
6. Frontend shows a grid of images with timestamps and labels, download buttons, and a
   "Download all" ZIP link.
7. The periodic cleanup deletes the video, temp frames and exports after the domain's retention
   window (24 hours) and marks the job `expired`. Metadata and scoring rows are kept.

## Multi-domain rules

- A domain is a tenant boundary for **behaviour**, not for **users**. There is no login in Phase 1.
  Rate limiting is per IP per domain.
- One domain, one prompt set, one shot list, one accepted subject. Cross-domain uploads are
  rejected by the gate.
- Domains can override the model, sampling density, image count and retention. Everything else
  is shared code.
- Adding a domain: add `configs/domains/<key>.yaml`, add the origin to its allowlist, add a
  frontend Deployment + Ingress with `NEXT_PUBLIC_DOMAIN_KEY=<key>`. Nothing else changes.

## Non-goals for Phase 1

- Authentication, accounts, billing.
- Image generation or enhancement. Frames are exported as-is from the source.
- Audio analysis. Not needed for any planned domain.
- Real-time processing. Jobs are async and take tens of seconds.
