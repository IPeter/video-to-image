# 03 — Processing Pipeline

Each step is a Go package under `internal/pipeline/` with a typed input and output. The worker
(`internal/pipeline/run.go`) wires them together and records progress, duration and cost per
step in `job_steps`. Steps never touch the DB directly; the runner does, so packages stay
testable with fixtures.

```
upload ─► 0 validate ─► 1 preview ─► 2 analyze ─► 3 extract ─► 4 score ─► 5 select ─► 6 export
                                        │
                                        └─ gate failed ─► job.rejected (no further steps)
```

## Step 0 — Validate upload (runs in `serve`, synchronously)

Input: the streamed upload. Output: stored original + `VideoInfo`.

- Reject if `Content-Length` exceeds `limits.maxUploadBytes` (500 MB for cars). Also wrap the
  body in an `io.LimitReader` so a lying client is cut off at the limit.
- Sniff the first 4 KB with `h2non/filetype` (MP4/MOV/WebM/MKV). Trust magic bytes over the
  declared MIME.
- Run `ffprobe -v error -show_entries format=duration:stream=width,height,r_frame_rate,codec_name:stream_side_data=rotation -of json`
  on the stored file (local temp copy in the API pod; streamed to storage in parallel via
  `io.TeeReader`).
- Reject if it does not decode, duration outside `[minDurationSec, maxDurationSec]`, or short
  side < `minShortSidePx`.
- Respect rotation metadata (phone videos). Record the display width/height, not the coded ones.
- Store `VideoInfo{DurationSec, Width, Height, FPS, Codec, Rotation, SizeBytes, MIME}` on the job.

Error codes: `UPLOAD_TOO_LARGE`, `UNSUPPORTED_FORMAT`, `CANNOT_DECODE`, `TOO_LONG`, `TOO_SHORT`,
`RESOLUTION_TOO_LOW`. Each maps to a user-readable message in the frontend.

## Step 1 — Build analysis preview

Never send the original to the model. Build a small preview to cut tokens and upload time.

```
ffmpeg -y -i in.mp4 -vf "scale=-2:480,fps=2" -c:v libx264 -preset veryfast -crf 28 -an preview.mp4
```

- 480p, 2 fps, no audio. Gemini samples video at 1 fps by default; supplying 2 fps and asking
  for the higher sampling rate helps with fast pans around a car. Confirm the current
  `VideoMetadata.FPS` option in the Go SDK before relying on it.
- For a 2-minute clip this is a few MB. Upload via the **Gemini Files API**; the returned file
  handle backs any retry without re-uploading. Delete the file after the job.
- Record preview build time. If it dominates, consider hardware encode or a lower preset.

## Step 2 — Analyze: domain gate + scene candidates (one model call)

One call returns both answers (decision 3). The gate is evaluated first inside the response; if
it fails the runner ignores everything else and ends the job as `rejected`.

Request:
- Model: `model.name` (default `gemini-3.5-flash-lite`).
- Parts: the preview file reference + the composed prompt.
- `ResponseMIMEType: application/json` with a `ResponseSchema` so the output is machine-readable.
- `Temperature` 0.2. `ThinkingBudget` 0 for Flash-Lite unless the eval says otherwise.
- `context.WithTimeout` of 120 s.

Prompt composition (from the domain YAML, see `04-domain-config.md`):

```
SYSTEM
You analyze short videos and return structured JSON only.

USER
Step 1 — Gate. Decide whether this video primarily shows: <gate.subject>.
<gate.instructions>
If it does not, set gate.passed=false, explain briefly in gate.reason, and return an empty scenes array.

Step 2 — Scenes. Only if the gate passed.
<selection.instructions>

Shot list (find the best moment for each, if present):
- front_three_quarter: Front 3/4 view showing front and one side ...
- side_profile: ...
...

Return up to <selection.maxScenes> scenes. For each: start and end time as "MM:SS", the shotId
it best fulfils (or "other"), a one-sentence reason, and a relevance score 0–1. Prefer moments
where the camera is steady and the subject fills the frame. Avoid near-duplicate moments.
```

Response schema (Go struct → `genai.Schema`):

```json
{
  "gate": { "passed": true, "confidence": 0.94, "reason": "A silver sedan on a dealership lot ...", "detectedSubject": "car" },
  "videoSummary": "Walkaround of a silver sedan, exterior then interior.",
  "scenes": [
    { "start": "00:04", "end": "00:09", "shotId": "front_three_quarter", "reason": "...", "relevance": 0.9 }
  ]
}
```

Post-processing:
- Parse `MM:SS` (also accept `M:SS`, `HH:MM:SS`, and bare seconds). Clamp to `[0, duration]`.
  Drop ranges shorter than 0.3 s or that fail to parse. Log every drop into `job_steps.meta`.
- Merge overlapping ranges with the same `shotId`.
- If `gate.passed` is false **or** `gate.confidence < gate.minConfidence` (0.6), set job to
  `rejected` with the reason. Delete the original. Stop.
- If the gate passed but `scenes` is empty, set job to `completed` with zero images and the
  message "No suitable scenes found" (the brief requires this over returning unrelated images).
- Record `UsageMetadata` (prompt, output, thinking tokens) into `usage_events` and compute cost
  from `internal/pricing`.

## Step 3 — Extract candidate frames

For each accepted range, sample densely from the **original** at source resolution. One FFmpeg
invocation per range produces both the full-resolution candidate and a 640 px scoring copy, so
Go never decodes a 4K JPEG:

```
ffmpeg -y -ss <start-0.5> -to <end+0.5> -i original.mp4 -filter_complex \
  "[0:v]fps=<sampleFps>,split=2[full][small];[small]scale=640:-2[thumb]" \
  -map "[full]" -qscale:v 2 cand/<scene>_%04d.jpg \
  -map "[thumb]" -qscale:v 4 small/<scene>_%04d.jpg
```

- `sampleFps` from domain config, default 4. Ranges shorter than 1.5 s bump to 8.
  Padding of 0.5 s on each side compensates for imprecise model timestamps.
- Cap total candidate frames at `limits.maxCandidateFrames` (300). If ranges would exceed this,
  lower `sampleFps` proportionally rather than truncating ranges.
- `-ss` **before** `-i` for fast seeking; ±1 frame accuracy is acceptable here.
- Each candidate records `{SceneIdx, ShotID, TimestampSec, FullPath, SmallPath}`. Timestamp
  derives from range start + frame index / fps.
- Rotation: modern FFmpeg auto-rotates by default. Verify with a portrait fixture in tests.
- Work happens in a per-job scratch dir (`emptyDir` volume on k3s), deleted in a `defer`.

## Step 4 — Score frames

Runs on the 640 px copies. Scores are stored per candidate in `candidate_frames`.

**Sharpness** — variance of the Laplacian over the grayscale image (3×3 kernel, plain loop over
`image.Gray`). Higher is sharper. Normalize within each scene (z-score) because a dark interior
scene and a bright exterior scene have different absolute ranges. Also compute a global
percentile to drop truly blurry frames (below the 15th percentile of the job).

**Exposure** — from a 256-bin luma histogram: penalty proportional to the share of pixels in the
darkest or brightest 5% of the range. Hard reject above 40%.

**Near-duplicate detection** — dHash via `goimagehash.DifferenceHash` (64-bit). Two frames are
duplicates if Hamming distance ≤ `selection.dedupeMaxHamming` (6). Compare within a scene first,
then across scenes. Keep the sharper one of each duplicate pair; record `duplicate_of`.

**Optional model re-rank** (`selection.modelRerank`, off by default per decision 5) — send the
top 2–3 candidates per shot as images and ask the model to pick the best for the shot. Turn on
only if the eval shows sharpness alone picks the wrong frame (sharp but the car half out of frame).

Output per candidate: `{Sharpness, SharpnessZ, ExposurePenalty, DHash, DuplicateOf}`.

Scoring runs in a bounded worker pool (`errgroup.SetLimit(runtime.NumCPU())`).

## Step 5 — Select final set

Goal: `selection.imageCount` images (8 for cars) that cover the shot list, are sharp and are
visually distinct.

1. Group candidates by `ShotID`. Within each group, drop duplicates and frames below the
   sharpness floor.
2. **Coverage pass:** for each shot in `shots` in priority order, take the best-scoring frame
   from its group. Score = `w.relevance·sceneRelevance + w.sharpness·SharpnessZ − w.exposure·ExposurePenalty`.
3. **Fill pass:** if fewer than `imageCount` selected, add remaining frames globally by score,
   skipping any within `dedupeMaxHamming` of an already-selected frame and any within 1 s of one.
4. **Missing pass:** every shot with `required: true` and no frame goes into `missingShots`.
5. If total < `imageCount`, return what exists with an `explanation`: "Only 5 distinct
   high-quality views were found. Missing: rear view, interior." Fewer-with-explanation beats
   padding with junk.

Output: ordered `[]SelectedImage{CandidateID, ShotID, TimestampSec, Scores, Reason}` in
shot-list order so the car frontend shows front → side → rear → interior consistently.

## Step 6 — Export

- The Step 3 full-resolution candidates were extracted at source resolution with `-qscale:v 2`,
  so export copies them as-is. Re-encoding would only lose quality.
- Thumbnails: the 640 px scoring copy, re-encoded at quality 85 if needed.
- Filenames: `<domainKey>_<jobShortId>_<index>_<shotId>_<mm-ss>.jpg`.
- Write `manifest.json` with timestamps, shot labels, reasons and scores.
- Stream a ZIP (`archive/zip` over `io.Pipe`) of all images + manifest into storage.
- Persist `selected_images` rows and mark job `completed`.
- Delete the scratch dir and the Gemini file immediately. Keep original + exports until
  retention expires.

PNG/WebP export: later, as a `?format=` parameter on the download endpoint transcoding on the
fly. Not in Phase 1.

## Error handling and retries

- Model call: retry up to 2 times on 5xx / 429 with exponential backoff, reusing the Files API
  handle. On repeated failure job → `failed` with `MODEL_UNAVAILABLE`.
- Malformed JSON from the model: one retry with an appended "Return valid JSON only" nudge.
  Then `failed` with `MODEL_BAD_OUTPUT`. Store the raw text in `jobs.internal_log`.
- FFmpeg failure: no retry (deterministic). `failed` with `PROCESSING_ERROR` and the stderr tail
  in `internal_log` (never exposed to the user).
- River retries the whole job only on infrastructure errors (DB, storage). Pipeline errors are
  terminal and set job status explicitly so River does not re-run a job that already spent money.
- Worker pod restarts mid-job: River re-delivers; the runner resumes from the first step not
  marked `done`, reusing stored preview/Files handle where present.

## Cost controls (from the brief)

- One video ingest per job by default. Re-rank off.
- `limits.maxCandidateFrames` bounds FFmpeg and scoring work.
- River queue `analyze` with `MaxWorkers: 2` caps concurrent model calls; a global token bucket
  caps calls per minute.
- Per-job cost is computed and stored; a daily cost ceiling (env `DAILY_COST_CEILING_USD`)
  pauses the queue when the day's `usage_events` sum exceeds it.
- Token usage logged per step to validate the $0.01–$0.05 per video estimate in the brief.
