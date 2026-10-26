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

## Round 3 (2026-09-24): finding the moments the analyze call misses

Driven by the same iPhone walkaround. Round 2 delivered 3–5 photos of a video that
holds nine or ten (front at 0–1 s, side profile at 6.5–10.5 s, rear with the trunk open
at 18.5–20.5 s, rear 3/4 at 21.5–23 s, dashboard, cluster, seats, wheel, headlight). The
stored verdicts of four runs showed where the photos were lost:

1. Frames were only extracted inside the analyze call's scene windows (±0.5 s), about
   14 of 59 s. The model watches a 2 fps 480p preview and answers in whole seconds; it
   put the side profile at 4–7 s while the car was whole in frame from 6.5 s to 10.5 s,
   so every side-profile candidate was from the mid-turn seconds and correctly judged
   "cut off". The good seconds never became candidates.
2. The scene list changed every run (6, 8, 8, 6 scenes; rear 3/4, wheel and odometer
   came and went) at temperature 0.2 with "the best moment" per shot.
3. The review relabelled the straight-on rear (tailgate open) as trunk; with one photo
   per view, rear and trunk collapsed into one.
4. The review called the 0.75 s front frame "cut off" while its own box had margin on
   all four sides (another run accepted the same frame).

Changes:

- **Locate pass** (`internal/pipeline/locate`, `locate:` block in the domain YAML, on
  for cars): after the analyze call accepted the video and before anything is saved,
  the preview is sampled at 1 fps and laid out on contact sheets (4×3 tiles of ≤ 320 px,
  the second printed in each tile's corner with a 3× bitmap font; a one-minute video is
  5 sheets ≈ 5 images of tokens). One call (`Step "locate"`, own usage rows, label
  "Finding the moments" in the web) returns, per view, the seconds whose tiles show the
  whole subject. Each second becomes the range [s−0.5, s+0.5]; `analyze.MergeScenes`
  joins touching ranges of one shot and the analyze call's own scenes (the higher
  relevance keeps its reason), capped at `selection.maxScenes` (cars: 30). The merged
  list is what `SaveAnalysis` stores, so a retry extracts the same ranges. A failed
  locate call (unavailable, bad output, no frames) is logged in the analyze step meta
  (`locateError`) and the job goes on with the analyze scenes alone; only a context
  error propagates. The fake analyzer answers an empty list, so fixtures are unchanged.
- **Analyze prompt**: up to 2 distinct moments per shot instead of "the best moment";
  `cars.yaml` temperature 0; `selection.scenePaddingSec` (new, cars: 1 s; default stays
  0.5 s = `extract.PaddingSec`).
- **Review shortlist spread** (`review.shortlistGap`): the frames of one shot sent to
  the review must be at least (the shot's summed scene span ÷ candidatesPerShot) apart,
  not just 1 s, so an 8 s side-profile scene is sampled at its start, middle and end
  instead of at its three sharpest (first, cut-off) seconds. cars: `candidatesPerShot`
  4.
- **Box over boolean** (`review.EdgeMargin`, `Assessment.VisibleByBox`): a "not fully
  visible" verdict is overruled when the model's own box keeps ≥ 2 % from every image
  edge. A box touching an edge, or no box, leaves the verdict alone. Counted in the
  review log (`visible_by_box`).
- **Prompt rules (cars review and locate)**: rear with the tailgate open is `rear` when
  the whole car is visible, `trunk` only when the cargo area fills the picture; the
  photographer's shadow or reflection is not a defect. (A code rule letting a frame
  fill its scene's shot as well as the review's view was considered and dropped: it
  would let a wheel close-up the review re-labelled fill a side-profile slot.)
- **Crop fallback** (`crop.fallbackAspect`, cars: 3:4): when the subject does not fit
  the primary aspect without being cut, the fallback shape is planned once with the same
  margin and floor. In a 9:16 frame a 3:4 crop can hold a subject up to 75 % of the
  height; a taller one still stays uncropped, which is the right output for it.

Result on the same video (one run, 2026-09-24 16:53, local, gemini-3.5-flash-lite):
8 of 8 photos — side profile (7 s, whole car), rear 3/4 (21.6 s), rear with the trunk
open (20.3 s), front (1.75 s, 3:4 fallback crop), dashboard, cluster, front seats, rear
seats through the rear door; only front 3/4 reported missing (it is not in the footage).
The locate call named 13 seconds on 5 sheets in 4.2 s for $0.0033; the job cost $0.027
(review $0.021 for 24 frames, up from 13) and took 97 s end to end, of which extract
63 s (10 scenes with 1 s padding on 4K HEVC, up from ~30 s). The locate call listed
5 s and 6 s for the side profile (car cut off; 7–10 s were the whole-car seconds) — the
shortlist spread still reached 7 s through the merged 3–8.5 s scene; the prompt now
says explicitly that an edge-touching subject is never "shown well" and that sideways
tiles count like upright ones (not yet re-run against the model).

Open: extract time with more scenes (the worker's 4 CPU limit will feel it; consider
`V2I_EXTRACT_CONCURRENCY` 2 there or a lower `sampleFps` for located ranges), the
review cost (24 frames), and the 3:4 front crop's bottom margin (tight).

First job on the cluster (2026-09-24 17:23, job_01M3A07MV25YSPNJK28BHHFR9S): 8 photos,
but all three studio versions withheld — rear 3/4 and front because the car touched the
photo edge (both are crops around the review's box, which runs a few percent tight at
512 px; a 6 % margin did not cover it), the side profile because the frame chosen was
22.75 s with the tailgate open (the locate call again did not list 7–10 s, where the
car lies sideways and nearly fills a 320 px tile). Follow-up in the same round:
`crop.marginPct` 10 and `crop.minMarginPct` 3 for cars (a crop whose fitted margin
would fall below the floor is not made); the locate sheets show every second as a 2×2
quad of its rotations (as recorded, 90°, 180°, 270°; 240 px cells, 6 seconds per sheet,
10 sheets per minute) because the model missed the sideways side-profile seconds in
three runs when shown a single tile, while the review's orientation grids of the same
shape scored 4 of 4; the model also names the upright copy; and the prompt asks for
every second where the view is recognisable with `wholeSubject` telling whether it is complete — moments with
`wholeSubject` false are kept at 0.6× relevance, since the review judges the
full-resolution frame anyway.

Third local run after the quads (17:37): the locate call named 24 seconds including the
side profile at 8 s and 10 s (rotation 270 reported correctly), yet the job fell to 6
photos: the review shortlist for the side profile took the sharpest frames under the
spread rule, at 4.7, 7.0, 10.8 and 24.5 s — exactly the edges of the 7.5–10.5 s window
— and all were cut off. The two passes were not connected. Now `review.Input.Preferred`
(per shot, the seconds the locate pass saw the subject whole; stored in the analyze
step meta as `locatedMoments` for a resumed run) makes the shortlist take the best
candidate within 0.5 s of each such second first, then fill by score with the spread
rule. `scenePaddingSec` is back to 0.5 for cars: with the located windows the 1 s
padding only doubled the 4K decode (extract 1m57 for 293 candidates).

Fourth local run (17:42), with the shortlist preferring the located seconds: 7 photos —
side profile at 7.9 s (whole car, uncropped), rear 3/4 at 20.75 s, rear at 19.1 s,
front at 0.25 s (3:4), dashboard, front seats, rear seats; missing only front 3/4.
20 frames reviewed, 11 of them at located seconds; locate $0.0053 (10 quad sheets),
job $0.027, extract 56 s. Not yet deployed.

Second cluster job on the new build (job_01M3A3J7F3YFMD49MABBHTV42E, 18:22): 8 photos
with the side profile at 7.75 s, but the studio again withheld all three: the side
profile and the front for "roof antenna tip" (the QA policy question left open on
2026-09-23), the rear 3/4 because the frame chosen (11.5 s) really is cut off at the
right — the review called it fully visible, which is model noise; the gate is right.
One QA answer was cut off at the 512-token cap with 490 thinking tokens although the
budget is 0. Decided: a thin roof antenna or aerial (or its tip) is minor — the QA
prompt and schema have `minorParts`, any missingParts item naming an antenna/aerial is
moved there (`studio.minorPartWords`), and minor parts never withhold; the QA output cap
is 2048.

## Spike (2026-09-24 evening): generative studio backgrounds

The user wants a real photo-studio look (turntable, cyclorama) on every photo,
interiors included. Spike in `data/research/studio-gen-spike/` (scripts + outputs) with
`gemini-3.1-flash-image` on the seven photos of a local run of the reference video:

- Sending the photo and asking for a new background: beautiful studio, but the model
  recomposes (shrinks and recentres the car), so pasting the original car pixels back
  through our mask does not line up.
- Sending the car cut out on flat magenta and asking to fill only the magenta: pixel
  aligned when the car has room (rear, rear 3/4: mean car diff 7/255), still recomposed
  when the car fills the frame (side profile, front: ~80/255).
- Same, on a canvas padded 25 % with magenta on every side: aligned for all four
  exteriors (diff 6.6–7.9). The output is the studio around the original car pixels,
  turntable under the car, correct perspective. ~10 s and ~1,450 output tokens per image.
- Interiors: asking to replace only what is seen through the glass keeps the cabin and
  puts the studio in the windows (dashboard, rear seats); the front-seats shot barely
  changed. A diff-based paste-back keeps the cabin pixels original.

Plan: a generative style in the studio step — padded magenta cut-out → image model →
alpha composite on the padded canvas → alignment check (car diff < 15/255, else retry
once, else the procedural styles) → the existing gates and QA; interiors go through the
window prompt with the diff paste-back. Needs the analyzer boundary to carry image
output (response modalities, inline image bytes), a price for the image model, and a
worker time budget (~10 s per photo, sequential per job).

Try it on any photo: `make studio-try PHOTOS="a.jpg b.jpg" [ARGS="--interior --style white"]`
(`scripts/studio-try.py`: car box from gemini-3.5-flash-lite → sidecar mask → padded
magenta → image model → composite + alignment verdict; outputs in data/studio-try/).

## Round 4 (2026-09-24 evening): generative studio, every photo

Built from the spike above.

- **Analyzer boundary** carries images: `Request.ResponseModalities` / `ImageAspectRatio`,
  `Response.Images` (inline bytes + MIME); the Gemini client maps them onto
  `responseModalities` / `imageConfig` and collects inline image parts; recordings store
  images base64; the fake answers `studio-gen` with the request's own image, its
  border-connected magenta turned into a grey gradient (a name containing "misaligned"
  shifts it). Price row for gemini-3.1-flash-image ($60/M output tokens).
- **`internal/studio/gen.go`**: `BuildCanvas` (car on magenta with a margin), `GenRequest`
  (prompt by look: grey/white/dark; interior prompt for the window treatment),
  `Alignment` (mean luminance difference inside the eroded mask at 384 px; > 15 = the
  model moved the car), `ComposeGenerated` (alpha under the original car),
  `CropCanvas` (keep 10 % margin, scale to the photo's long side), `InteriorCompose`
  (generated pixels only where the answer differs, 2–60 % of the picture).
- **Studio job**: the generative call runs in the photo's background goroutine before
  the QA check; a misaligned answer is retried once with a wider margin (+0.15); a
  failed call leaves an exterior with its drawn styles and ends an interior withheld.
  Interior views (`studio.generative.interiorViews`) are eligible without a box, skip
  the mask, gates and QA. `studio.views` now lists every exterior shot.
- **Config**: `studio.generative {enabled, model, look, pad, keepPad, interiorViews}`;
  the "studio" style is first and the default when enabled (`EffectiveStyles`,
  `EffectiveDefaultStyleWithGenerative`). API enums, `studioStyles`, `studioEligible`,
  web labels/swatch and the usage label "Studio backgrounds" follow.
- The studio-job tests pin the pre-round-4 config (five views, no generative) and
  three new tests cover the exterior, misaligned and interior paths with the fake.

First local run of round 4 on the reference video (19:35): 5 of 7 studio versions ready
(side profile, front, trunk, dashboard, front seats); rear 3/4 withheld by the QA (open
tailgate, correct), rear seats withheld (the model changed 77 % of the picture). The
dashboard result exposed a hole: the diff-based interior blend let a repainted dashboard
through. Interiors now get a window-box call first (gemini-3.5-flash-lite,
`WindowBoxesRequest`): generated pixels are taken only inside the glass boxes
(feathered), the cabin stays original by construction, and no glass → no image call.
Cost: the image model bills ~1,450 output tokens per photo at $60/M ≈ $0.087 (not the
$0.04 estimated earlier); a job with 7 studio photos cost $0.65, of which $0.59 the
image calls. Usage rows of the image calls are step `studio-gen` ("Studio backgrounds").
