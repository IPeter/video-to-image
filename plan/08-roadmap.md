# 08 — Roadmap

Phases are ordered so that every phase ends with something runnable. Estimates assume one
developer working with AI assistance; treat them as relative sizes, not commitments.

## Phase 0 — Foundations (½–1 day)

- Go module, `cmd/v2i` with cobra subcommands (`serve`, `work`, `migrate`, `eval` stubs).
- `docker-compose.yml` with Postgres and MinIO. `.env.example`. `Makefile`.
- `internal/domain`: YAML loader, validation, registry. `configs/domains/cars.yaml` from
  `04-domain-config.md`. Unit test that every file in `configs/domains` loads.
- `internal/httpapi`: Huma on chi, `/healthz`, `/livez`, `/metrics`, `GET /v1/domains/:key`,
  Origin allowlist middleware, error envelope.
- `migrations/` with goose; `sqlc.yaml`; `v2i migrate` runs River + goose migrations.
- `web/`: Next.js scaffold, Tailwind, shadcn/ui, `openapi-typescript` wired to `/openapi.json`.
- CI (GitHub Actions): `go vet`, `golangci-lint`, `go test`, `sqlc diff`, openapi type drift
  check, `pnpm build`. Container images built and pushed to GHCR on `main`.

Exit: `make dev` runs, `curl localhost:4000/v1/domains/cars` returns the public config,
`web` renders the upload page with the domain's copy.

## Phase 1 — End-to-end MVP for `cars` (4–6 days)

Goal: upload a real car walkaround, get 8 JPGs and a ZIP back, no design polish.

1. **Upload + validate** (`03-pipeline` step 0). Streaming multipart with size cap, magic-byte
   sniff, ffprobe, error codes, local disk storage, `jobs` table, `POST /jobs` → 202 with the
   River job inserted in the same transaction.
2. **Worker + state machine**. River client, `pipeline` queue, `run.go` orchestration,
   `job_steps` writes, `GET /jobs/:id`. `--embed-worker` flag for local single-process runs.
3. **Preview + analyze**. FFmpeg preview, Gemini Files API upload, combined gate + scenes
   prompt with `ResponseSchema`, timestamp parsing/clamping, `usage_events` with cost.
   `FakeAnalyzer` and `RecordingAnalyzer` implementations.
4. **Extract + score + select**. Dense sampling with the split filter graph, Laplacian
   sharpness, dHash dedupe, exposure check, coverage/fill selection with `missingShots`.
   Table-driven tests on fixture frames (sharp/blurry/duplicate pairs).
5. **Export**. Copy full-res candidates, thumbnails, manifest, streamed ZIP, download endpoints.
6. **Frontend**. Upload page, progress page, results grid, rejected state, download buttons.
   Plain Tailwind. Runtime `/api/config` for domain key and API URL.
7. **Cleanup**. River periodic retention job, Gemini Files deletion, rejected/failed jobs delete
   the original immediately.

Exit: 5 real dealership videos produce usable listing photos; 3 wrong-domain videos (a room, a
person, a landscape) are rejected with sensible reasons; cost per video is logged.

## Phase 2 — Quality and evaluation (2–4 days)

- `v2i eval --dir <videos> --analyzer=gemini|recorded --out report/`: runs the pipeline over a
  folder, writes an HTML report per video (selected frames, timestamps, reasons, scores) and a
  JSON summary (cost, latency, missing shots, duplicate rate).
- Build the **eval corpus** the brief asks for: 20–30 videos. Daylight lot, indoor showroom,
  night, phone-portrait, fast pans, people in frame, and wrong-domain controls.
- Recorded analyzer responses make re-runs of scoring/selection changes free and deterministic.
- Tune one variable at a time: `previewFps`, `sampleFps`, sharpness floor, `dedupeMaxHamming`,
  weights, prompt wording, `thinkingBudget`.
- Compare `gemini-3.5-flash-lite` vs `gemini-3.8-flash` on the same corpus; keep Flash-Lite
  unless the report shows a clear win. Try the model re-rank on the hardest 5 videos.
- SSE progress endpoint (Postgres `LISTEN/NOTIFY`) replaces polling.
- Rate limiting per IP per domain. Daily cost ceiling that pauses the queue.

Exit: written eval report with numbers; defaults in `cars.yaml` updated from evidence.

## Phase 3 — Deploy to the home k3s cluster (2–3 days)

Details in `10-deployment-k3s.md`.

- `deploy/docker/Dockerfile.v2i` (distroless-ish Debian slim + pinned FFmpeg) and
  `Dockerfile.web` (Next standalone). Both pushed to GHCR by CI.
- `deploy/k8s/base`: Namespace, `v2i-api` Deployment + Service, `v2i-worker` Deployment,
  `v2i-migrate` Job, domains ConfigMap, Secrets (via SealedSecrets or SOPS), Ingress per
  frontend + one for the API, Postgres (CloudNativePG or a simple StatefulSet with a PVC),
  MinIO (operator or single-node StatefulSet with a PVC).
- `deploy/k8s/overlays/home-k3s`: hostnames, storage class (`local-path`), resource limits,
  replica counts, TLS via cert-manager (Let's Encrypt with DNS challenge, or a local CA for
  LAN-only previews).
- S3 storage adapter pointed at in-cluster MinIO; presigned download URLs through the MinIO
  ingress host.
- Presigned **direct-to-MinIO upload** from the browser (decision 9), with the API issuing the
  URL and then running ffprobe on the stored object.
- Prometheus + Grafana (kube-prometheus-stack if not already on the cluster) scraping `/metrics`;
  alerts on `failed` rate and daily cost.
- Frontend Deployment `web-cars` with `DOMAIN_KEY=cars` and its Ingress host.

Exit: a customer can open the cars preview URL on your k3s cluster, upload a video, and download
the images.

## Phase 4 — Second domain (1 day, proves the architecture)

- Add `homes.yaml` to the ConfigMap. Add its origin. Add `web-homes` Deployment + Ingress.
- Run `v2i eval` on 10 property tours.
- Whatever needed a code change to make this work is a design bug; fix it in the shared layer.

## Phase 5 — Design pass

- Apply the design system to `web/`. Per-domain themes and assets.
- Lightbox, motion, compare-with-video view.

## Phase 6 — Later ideas (not planned in detail)

- Moving from the home cluster to a hosted Kubernetes (Hetzner, DigitalOcean, GKE) is a new
  kustomize overlay plus external Postgres/S3 if wanted. No application change.
- Accounts and job history per dealership; billing per job.
- PNG/WebP export, custom crops, batch upload.
- Per-domain post-processing (auto-level horizon, license-plate blur for cars, likely as a
  small Python CV service).
- Webhooks so a dealership's inventory system can pull images automatically.

## Evaluation metrics (from the brief, made concrete)

| Metric | How measured | Phase 2 target |
| --- | --- | --- |
| Gate accuracy | manual label vs `gate.passed` on the corpus | ≥ 95%, zero false rejects on real car videos |
| Shot coverage | required shots found / required shots visible in video | ≥ 90% |
| Relevance | manual 1–5 rating per selected image | mean ≥ 4 |
| Sharpness | % of selected images rated "sharp" by a reviewer | ≥ 90% |
| Duplicate rate | % of selected images a reviewer calls near-duplicates | ≤ 5% |
| Latency | upload complete → completed, p50 / p95 | ≤ 30 s / ≤ 60 s for a 90 s clip on the home server |
| Cost | `cost_usd` per job, p50 / p95 | ≤ $0.02 / ≤ $0.05 |

## Risks

- **Model timestamps are loose.** Mitigated by ±0.5 s padding, dense sampling, and picking by
  measured sharpness rather than trusting the timestamp.
- **Flash-Lite may misclassify shot angles** (front 3/4 vs side). If eval shows this, enable the
  model re-rank on images, or coarsen the shot list and let scoring differentiate.
- **Phone videos with rotation metadata** produce sideways frames if FFmpeg flags are wrong.
  Include portrait clips in the corpus and in `testdata/` from day one.
- **Home server CPU.** FFmpeg extraction of 300 frames from 4K video is the slowest step.
  Worker `MaxWorkers` and `maxCandidateFrames` are the knobs; measure on the actual hardware.
- **Home server availability for customer previews.** Uptime depends on your network and power.
  Acceptable for previews; the k8s manifests are the escape hatch to a hosted cluster.
- **Pricing and model IDs in the brief may change.** Pricing table lives in one package with a
  reference date; model name is config.
