# 11 — Implementation notes (Phase 0 + Phase 1)

Written 2026-09-23 after the first implementation. Records where the code deliberately
differs from, or adds to, documents 01–10, and what is still open. Everything here was
reviewed (four review lenses, each finding checked by an independent verifier) and is
covered by tests.

## Status

- **Phase 0 and Phase 1 are implemented** and run end to end locally
  (`make deps-up && make dev-fake`, `make web-dev`), including the fake analyzer, the
  rejected/failed/expired states, downloads, retention and the Playwright e2e.
- Gemini integration is implemented, unit/HTTP-tested and verified against the live
  API (2026-09-23): a 30 s synthetic clip was uploaded through the Files API, analysed
  with the structured-output schema and correctly rejected ("an animated simulation of a
  box-like object moving across color bars, not a real passenger vehicle", confidence 1.0);
  the uploaded file was deleted afterwards. The Phase 1 exit test with real dealership and
  wrong-domain videos is still to be run.
- Phase 2+ (eval tool, SSE, k8s manifests, direct-to-MinIO upload, design) not started.
  `v2i eval` is a stub that says so.

## Confirmed against the live Gemini API (plan 09 "still to confirm")

- Model IDs `gemini-3.5-flash-lite` and `gemini-3.8-flash` exist.
- **`thinkingBudget: 0` is rejected by gemini-3.5-flash-lite (HTTP 400).** Gemini 3.x uses
  `thinkingLevel`; flash-lite does no thinking at `minimal`/`low` (and by default).
  gemini-3.8-flash rejects `minimal` but accepts `thinkingBudget: 0`. The domain config
  therefore has `model.thinkingLevel` next to `model.thinkingBudget` (set at most one);
  `cars.yaml` uses `thinkingLevel: minimal`.
- Accepted: `responseMimeType: application/json`, a response schema with string enums
  (`format: enum`), min/max, `propertyOrdering`, system instruction, `maxOutputTokens`,
  `videoMetadata.fps`.
- Measured on the 30 s clip (2 fps, 480p preview): 4,619 input tokens (~154 tokens per
  second of video, inside the brief's 100–300 estimate), 102 output, 0 thinking tokens,
  **$0.0016 per analysis**, ~1.8 s model latency, ~9 s for the analyze step including the
  upload and file processing.

## Additions to the domain config (04)

| Key | Default | Why |
| --- | --- | --- |
| `limits.maxPixels` | 4096×2176 | A 200 KB 8K file expanded to ~3 GB of frames in ffmpeg. Uploads above it → `RESOLUTION_TOO_HIGH`; every ffmpeg decode also gets `-max_pixels` (ffprobe 4× the cap, so a bomb fails cheaply as `CANNOT_DECODE`) |
| `limits.maxFps` | 120 | Same reason for extreme frame rates |
| `model.maxOutputTokens` | 4096 | A runaway or prompt-injected answer is otherwise billed in full |

Extra validation: `selection.imageCount` must be at least the number of required shots.
The public subset also exposes `minShortSidePx`, `maxPixels` and `retentionHours`.

## API (05)

- Additional error codes: `INVALID_REQUEST`, `UNAUTHORIZED`, `JOB_NOT_FOUND`,
  `IMAGE_NOT_FOUND`, `NOT_READY` (ZIP before completion, 409), `NOT_FOUND`,
  `INTERNAL_ERROR`, `RESOLUTION_TOO_HIGH`, `SERVER_BUSY` (503: too many concurrent
  uploads or scratch disk low). Validation errors carry `details` with the numbers.
- Result images also have `fileName` and `downloadUrl` (`?download=1`).
- Origin rule: an `Origin` header, when present, must be allowed. Without one, GETs are
  allowed (image tags, downloads, server-side fetches) and POST/DELETE need
  `X-Internal-Token`. Browsers always send `Origin` on cross-origin POST/DELETE, so a
  misconfigured frontend still fails loudly.
- `/metrics` is served only on the admin listener `V2I_ADMIN_ADDR` (default `:4001`, for
  both `serve` and `work`), never on the public API port. `/healthz` reports the ffmpeg
  version only to requests with `X-Internal-Token` and is rate limited.
- Uploads are written to a local temp file, probed, then copied to storage (not teed in
  parallel). Admission control: `V2I_MAX_CONCURRENT_UPLOADS` (4) globally,
  `V2I_MAX_UPLOADS_PER_CLIENT` (2), a free-disk check, and a minimum body throughput
  (1 MiB per 60 s) instead of a server ReadTimeout.
- Client IP: `V2I_TRUSTED_PROXIES` (CIDRs) replaces a boolean; `X-Forwarded-For` is
  walked from the right and only honoured from a trusted peer. Rate-limit keys use the
  IPv6 /64. `client_ip_hash` is an HMAC with `V2I_IP_HASH_SALT` (random per process when
  unset). Wrong internal tokens are throttled; with a paid analyzer the token must be
  ≥ 24 random characters.
- A rejection caused only by low gate confidence shows a neutral reason instead of the
  model's (positive) one.

## Pipeline (03)

- **Analyze is recorded atomically**: gate, summary, scenes, the finished step and — for
  a rejection — the status change are one transaction (`store.SaveAnalysis`), written on
  a detached context with in-process retries. A worker restart or DB blip right after the
  model answered no longer pays for a second call. Remaining edge: if the database stays
  down through all retries (~1 min), River's retry calls the model again.
- The preview is capped with `-t` at the validated duration (a container can lie about
  its duration) and re-probed before any model call; container metadata (GPS location,
  title) is stripped before it goes to Google.
- Extraction uses `fps=<F>:round=up:start_time=0,trim=end_frame=<N>`: the plain `fps`
  filter lags up to half a sampling interval, which broke "frame i at start + i/fps".
- Scoring, two refinements to validate in the Phase 2 eval:
  - the job-wide 15th-percentile blur floor never rejects a scene's sharpest frame
    (a dim, low-texture interior is not "truly blurry");
  - a near-duplicate pair that spans two shots keeps the frame of the higher-priority
    shot (required first), so an optional shot cannot knock out a required one. Same-shot
    pairs still keep the sharper frame, as planned.
- Model strings are length-capped (summary 500, reasons 300, subject 80 runes).
- With `modelRerank` on, picks are stored in the select step and reused on a retry.
- Gemini Files handles are reused within one attempt and released right after analyze;
  they are not persisted across worker restarts (a re-delivered job uploads the preview
  again — uploads are free, the daily sweep removes leftovers).

## Queue and retention (06)

- `PipelineMaxAttempts` 5 with backoff 30 s → 2 min → 10 min → 30 min, so a short
  storage/DB restart does not burn every attempt.
- A reconciler (every 10 min) fails jobs left `queued`/`processing` after River gave up
  on them (e.g. the worker was OOM-killed on the last attempt), so users never wait on a
  spinner until expiry.
- A job failed after the last attempt now also deletes its files and closes its steps.
- User deletion and retention delete files, flip the status, then delete again, so an
  upload that raced the deletion cannot survive it.
- `DAILY_COST_CEILING_USD` defaults to **5** (fail safe). Set 0 only on purpose.
- The model-files sweep also runs on worker start (River periodic timers reset on restart).

## Frontend (07)

- Runtime config: the root layout (dynamic) reads `DOMAIN_KEY`, `API_URL` and optional
  `API_INTERNAL_URL` per request and passes them to the client; `/api/config` exists as
  planned. `pnpm check:dynamic` fails the build if any page gets prerendered.
- `/api/healthz` (liveness, always 200) and `/api/readyz` (503 when env is missing).
- Extra states: deletion confirmation, expired-while-open, 429/503 with a countdown from
  `Retry-After`, screen-reader announcements on terminal states.

## Round 2 (2026-09-23): real-video quality, cost display, speed

Driven by the user's own iPhone walkaround (59 s, 4K HEVC 60 fps, portrait, phone
turned mid-recording, 377 MB). The Phase 1 pipeline produced 8 photos of which 5 were
sideways, one exterior shot cut the car in half, two views were duplicated and all were
portrait; the job took 242 s.

- **Model frame review** (`internal/pipeline/review`, replaces the plan's optional
  re-rank; `review:` block in the domain YAML, on for cars). Two calls after scoring:
  1. *Orientation*: one 2x2 grid per shortlisted frame showing it rotated
     0/90/180/270°; the model names the upright cell. Asking for "degrees clockwise" or
     "which edge is up" was wrong on 2-3 of 4 real frames; the grid got 4 of 4
     (flash-lite, no thinking). Per frame, not per scene: on the user's video the phone
     turns at ~6 s inside the side-profile scene. Retried once on bad output or an
     unavailable model; if it still fails, the judgement prompt says the frames may be
     sideways and asks for a `rotation` per frame. The rotations reach the runner
     before the judgement call, so a failed judgement still exports frames upright.
  2. *Judgement* on upright 512 px copies: the view it really shows, whether the whole
     subject is inside the frame, quality 1-5, the subject box, and `sameAs` (a
     near-duplicate of an earlier frame). Only usable frames are eligible.
  Output budgets scale with the shortlist (judgement: max(domain cap, 512 + 160 per
  frame), orientation: 256 + 48 per grid, both ≤ 32768); `issues` is ≤ 8 words.
  The outcome is merged into the select step's `job_steps.meta` as soon as the review
  returns (`Store.MergeStepMeta`), before selection and export: verdicts (`review`) and
  rotations (`reviewRotations`) keyed by scene, shot and timestamp, counts, and on
  failure `reviewError` + `reviewFailure` (`badOutput`, `tooFew`, `unavailable`,
  `other`) and `reviewOrientationError`. A River retry reuses verdicts only when every
  stored one maps onto a current candidate (Usable recomputed with the current
  `minQuality`); a stored `badOutput`/`tooFew`/`other` failure selects on scores alone
  without calling the model again; only `unavailable` is reviewed again.
- **Incomplete or failed review**: an answer covering fewer than half of the frames sent
  (empty, unknown ids, cut short) counts as a failed review and the job selects on scores
  alone instead of ending with no photos; above half, unanswered frames stay ineligible.
  Frames without a verdict are exported rotated only (their own orientation answer,
  else the nearest oriented frame of the same scene within 1 s).
- **One photo per view** (`selection.maxPerShot: 1`) plus the review's `sameAs` links
  (resolved transitively: b sameAs a and c sameAs b never yield both a and c): the fill
  pass no longer adds a second frame of a covered view. `maxPerShot` is opt-in (unset =
  no cap); validation rejects an `imageCount` above `maxPerShot × (shots + 1)`. A frame
  the review re-classified gets "<Label> view" as its reason instead of the scene's
  reason for the old view.
- **Rotate + crop** (`crop:` block; cars: 4:3, 6% margin, ≥ 1080 px short side):
  frames are turned upright and cropped around the reviewed box; a crop that would cut
  the subject or drop below the minimum size is skipped. The margin is best effort
  (shrinks down to 0 before a crop is given up), and a crop that changes the orientation
  (upright portrait → 4:3 landscape) may go down to the frame's widest 4:3 rectangle, not
  below 480 px, so 1080p portrait (the iPhone default) becomes 1080x810. Edited frames are re-encoded
  once (q92) with a new thumbnail; unedited frames are still copied byte for byte.
- **Why views are missing**: when every take of a required view had the subject cut off,
  the explanation says so and asks to film from further away (a view with any fully
  visible take, rejected for blur or as a duplicate, gets no such hint).
- **Usage and cost on the job page**: `usage` in the job payload (per step and model:
  calls, input/output/thinking tokens, USD) shown as "AI usage: N tokens · $X".
- **Speed**: keyframe-only preview (`-skip_frame nokey`) when no stretch of the source
  goes longer than one preview frame interval (+0.25 s) without a keyframe, judged from
  the packet flags of the whole stream (demux only, 0.08–0.2 s on the 377 MB clip), at
  min(previewFps, ⌊1/largest gap⌋) fps. Phone video: preview 172.9 s → 4.4 s at 1 fps,
  also half the video tokens. Every skip-mode preview is read back (a video stream,
  ≥ 80 % of the expected frames, reaching the source's end, and — for keyframes-only —
  no gap between decoded frames, printed by `metadata` filters in the same run); if it
  fails it is rebuilt with `noref`, then from a full decode. The reason: H.264 open-GOP
  and intra-refresh sources flag recovery points as keyframes, and under nokey give an
  empty MP4 or a frozen picture while ffmpeg exits 0.
  Extract ranges can run in parallel (`V2I_EXTRACT_CONCURRENCY`, 0 = auto: CPUs /
  (FFMPEG_THREADS × V2I_PIPELINE_WORKERS)): 68.6 s → 28.1 s with 3 at once on an
  unconstrained 8-core machine, but 34 s vs 39 s under the worker's 4 CPU / 4Gi limit
  for 3× the memory (≈ 450 MiB per 4K HEVC process at 2 threads), so home-k3s uses 1.
- **Prompts**: the car instructions demand the whole car in exterior shots, distinct
  angles only, and judge content rather than orientation.

Result on the same video: 48–60 s end to end; 5 upright photos (dashboard, front, front
seats, instrument cluster close-up, trunk), no duplicates, no half cars. The exterior
3/4 and side views are reported missing because the car never fit in the frame.
Cost $0.011–0.017 per job (analyze ≈ $0.003, review ≈ $0.008–0.013: images are billed
≈ 1,100 tokens each at the default media resolution; LOW resolution was ~4× cheaper
but got 3 of 4 orientation grids right, so it is not used for the grids).

Open: the review's image cost (try LOW resolution for the judgement call only), and a
fallback crop aspect for tall subjects (a straight-on front view in a portrait frame
stays uncropped because it does not fit 4:3).

## Deployment (Phase 3, first cut — 2026-09-23)

Live at `https://cardealer.peterilles.com` on the home k3s node; details and runbook in
`deploy/k8s/README.md`. Differences from plan/10:

- One host for web and API (path routing: `/v1`, `/openapi.json`, `/docs` → API, `/` →
  web): same origin, no CORS, and Traefik **basic auth** (a requirement added at deploy
  time) covers everything.
- TLS terminates at Cloudflare; traffic arrives through the locally managed Cloudflare
  Tunnel on the node (`/etc/cloudflared/config.yml`, rule added for the new host), like
  the other `*.peterilles.com` apps; no cert-manager, no open router ports. Cloudflare caps request bodies at 100 MB on Free/Pro, so
  the browser uploads in 16 MiB chunks (see "Chunked uploads" below) and only the
  single-request upload is capped at 95 MiB (`V2I_MAX_REQUEST_BYTES`). Client IPs come from
  `CF-Connecting-IP` (`V2I_CLIENT_IP_HEADER`), trusted only from the Traefik pod network.
- Storage is a `local-path` PVC shared by api and worker (`V2I_STORAGE=local`) instead of
  MinIO: single node, fewer moving parts. `V2I_STORAGE=s3` switches without code changes.
- No registry: `deploy.sh` streams images into the node's containerd via
  `kubectl debug node`. Migrations run as init containers (serialised by an advisory
  lock) instead of a separate Job. Secrets are git-ignored env files rendered by
  kustomize's secretGenerator (SealedSecrets/SOPS later).
- Direct-to-storage upload (decision 9) is not done; with Cloudflare in front it would not
  lift the 100 MB cap anyway unless the upload host is DNS-only.

### Chunked uploads

A 1-minute 4K phone walkaround is ~380 MB, far above Cloudflare's 100 MB request limit,
so the browser always uploads in pieces (small files are one piece):

- `POST /v1/domains/{key}/uploads {fileName, sizeBytes, contentType}` → 201
  `{uploadId, chunkSizeBytes, chunkCount, expiresAt}`; `PUT …/uploads/{id}/chunks/{i}`
  (raw bytes, any order, 2 in flight, idempotent) → 204; `POST …/uploads/{id}/complete`
  → 202 with the job, running the same ingest as the single-request `POST …/jobs`
  (sniff, sha256, probe, limits, store, enqueue); `DELETE …/uploads/{id}` aborts.
- Limits: the domain's `maxUploadBytes` (500 MB for cars) is the real limit again. The
  hourly per-IP upload limit is charged when the upload starts, not per chunk. An open
  upload holds one of the global upload slots (shared with single-request uploads) and
  one of `V2I_MAX_UPLOAD_SESSIONS_PER_CLIENT`. Scratch space is reserved up front
  (2 × size + 256 MiB, like the single-request path); the data file is sparse.
- Sessions live in the API process with a `session.json` next to the data under
  `<scratch>/uploads/upl_…/`, so a container restart resumes them. A new pod (deploy)
  starts with an empty emptyDir: uploads in progress then fail with UPLOAD_NOT_FOUND
  and must start again. Unfinished uploads expire (5 min before the first chunk,
  `V2I_UPLOAD_SESSION_TTL`, default 15 min, after the last one); the browser also
  deletes its session on cancel and on page close.
- A repeated `complete` replays the first outcome (same job), so the browser can retry
  it after a lost answer; chunks are kept after a 500 for the same reason.
- Settings: `V2I_UPLOAD_CHUNK_BYTES` (16 MiB, clamped below `V2I_MAX_REQUEST_BYTES`),
  `V2I_UPLOAD_SESSION_TTL`, `V2I_MAX_UPLOAD_SESSIONS_PER_CLIENT`. Server-side callers
  (eval CLI, scripts) going through Cloudflare must use the chunked API too, or call the
  API directly.

## Operations

- Local compose uses host ports 55432 (Postgres) and 59000/59001 (MinIO). MinIO images
  come from quay.io; Docker Hub `minio/*` images are no longer published.
- The `v2i` image is ~550 MB because Debian's ffmpeg pulls in mesa/SDL via libavdevice;
  a pinned static ffmpeg would bring it to ~150 MB (Phase 3).
- `ffmpeg` gets `-threads 2` by default (`FFMPEG_THREADS`): on a 4K clip this cut extract
  memory from 724 to 253 MB.
- CI (`.github/workflows/ci.yml`) is validated with actionlint; golangci-lint is clean;
  e2e runs against the production standalone build and fails (not skips) in CI when the
  API is missing. Images are pushed only when e2e passes.

## Open items for Phase 2

- Run the Phase 1 exit test on real videos and tune from evidence: weights (the plan's
  score mixes an unbounded sharpness z-score with 0..1 terms), blur floor, dedupe
  distance, `previewFps`, `sampleFps`, prompt wording.
- Confirm on real footage: `videoMetadata.fps` honoured by flash-lite, enum format in the
  response schema accepted, Files API limits/TTL, HDR (HLG/PQ) phone video colours
  (no tone-mapping yet).
- `v2i eval`, SSE progress, k8s manifests (probes: web `/api/healthz` + `/api/readyz`,
  API `/healthz` + `/livez`, worker `/livez` on the admin port; worker
  `terminationGracePeriodSeconds` ≥ `V2I_SHUTDOWN_TIMEOUT` + 15 s).
