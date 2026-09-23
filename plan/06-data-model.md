# 06 — Data Model, Storage, Retention

## Database (Postgres, pgx + sqlc, goose migrations)

### `jobs`
| column | type | notes |
| --- | --- | --- |
| id | text PK | `job_` + ULID |
| domain_key | text | indexed |
| status | enum | queued, processing, completed, rejected, failed, expired |
| created_at, updated_at | timestamptz | |
| expires_at | timestamptz | `created_at + domain.retentionHours`; indexed for the cleanup job |
| client_ip_hash | text | for rate limiting and abuse review; hashed, not raw |
| video_path | text | storage key of the original |
| video_info | jsonb | `{ durationSec, width, height, fps, codec, rotation, sizeBytes, mime }` |
| model_name | text | as used, for later comparison |
| gate | jsonb | `{ passed, confidence, reason, detectedSubject }` |
| video_summary | text | |
| result_explanation | text | |
| missing_shots | jsonb | `string[]` |
| zip_path | text | |
| error_code, error_message | text | user-safe |
| internal_log | text | last N lines of stderr / raw model output, never returned to the client |
| cost_usd | numeric(10,6) | sum of usage_events |

### `job_steps`
| column | type |
| --- | --- |
| id | serial PK |
| job_id | FK → jobs |
| name | text (preview, analyze, extract, score, select, export) |
| status | enum (pending, running, done, failed, skipped) |
| started_at, finished_at | timestamptz |
| duration_ms | int |
| meta | jsonb (e.g. candidate count, scenes returned, dropped timestamps) |

### `scenes`
Model output after post-processing. Kept for evaluation, not shown to users.
| column | type |
| --- | --- |
| id | serial PK |
| job_id | FK |
| start_sec, end_sec | real |
| shot_id | text |
| relevance | real |
| reason | text |
| raw_start, raw_end | text (what the model actually said) |

### `candidate_frames`
Files are deleted with the job's scratch dir; rows are **kept indefinitely** (decision 11) so
the eval tool can inspect why a frame was or was not chosen. ~300 small rows per job.
| column | type |
| --- | --- |
| id | text PK (`cf_` + ULID) |
| job_id, scene_id | FK |
| timestamp_sec | real |
| sharpness, sharpness_z, exposure_penalty | real |
| dhash | bigint |
| duplicate_of | FK nullable |
| score | real |
| selected | boolean |

### `selected_images`
| column | type |
| --- | --- |
| id | text PK (`img_` + ULID) |
| job_id | FK |
| candidate_id | FK |
| idx | int (1-based display order) |
| shot_id | text |
| timestamp_sec | real |
| width, height | int |
| reason | text |
| image_path, thumb_path | text |
| bytes | int |

### `usage_events`
One row per model call. Source of truth for cost reporting.
| column | type |
| --- | --- |
| id | serial PK |
| job_id | FK |
| domain_key | text |
| step | text (analyze, rerank) |
| model | text |
| input_tokens, output_tokens, thinking_tokens | int |
| video_seconds | real |
| cost_usd | numeric(10,6) |
| latency_ms | int |
| created_at | timestamptz |

Prices live in `internal/pricing` keyed by model name, with the reference date from the brief. The worker computes `cost_usd` at insert time so later price changes do not rewrite
history.

## Object storage layout

One bucket (`v2i`), prefixed by domain and job so a domain can be wiped or migrated independently.
MinIO on k3s in Phase 3, any S3-compatible service later.

```
<bucket>/
  cars/
    jobs/
      job_01J.../
        original.mp4
        preview.mp4                  (deleted after analyze)
        candidates/0003_0012.jpg     (deleted after export)
        images/img_01J....jpg
        thumbs/img_01J....jpg
        manifest.json
        export.zip
```

Local dev adapter mirrors this under `./data/storage/`. The `Storage` interface:

```go
type Storage interface {
    Put(ctx context.Context, key string, r io.Reader, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error) // local impl returns an API URL
    DeletePrefix(ctx context.Context, prefix string) error
    Head(ctx context.Context, key string) (*ObjectInfo, error)
}
```

## Retention and cleanup

- Default `retentionHours: 24` per domain. Shown to the user as "Your images are available until
  <expiresAt>".
- A River **periodic job** runs every 10 minutes: `SELECT id FROM jobs WHERE expires_at < now()
  AND status NOT IN ('expired')`, for each: `storage.DeletePrefix(...)`, set `expired`, null out
  `video_path`, `zip_path`, `image_path`, `thumb_path`. All metadata and scoring rows are kept
  indefinitely (decision 11). Add a Prometheus gauge for table sizes and revisit if
  `candidate_frames` passes ~10 M rows.
- Gemini Files API uploads are deleted right after the analyze step succeeds or finally fails.
  A daily sweep lists remaining files older than 2 hours and deletes them as a safety net.
- Failed and rejected jobs delete their original immediately. There is nothing to retain.

## Job state machine

```
queued ──► processing ──► completed
                │
                ├──────► rejected   (gate failed)
                └──────► failed     (error after retries)

completed / rejected / failed ──► expired   (cleanup)
```

Transitions are performed only by the worker (and cleanup). The API never mutates status except
to create `queued` and to handle user `DELETE` → `expired`. The `jobs` insert and the River
enqueue share one transaction (`river.InsertTx`).

## River tables

River creates its own `river_job`, `river_leader`, `river_queue` tables via its migration
command, run by `v2i migrate` before the goose migrations. Queues: `pipeline` (MaxWorkers 2 in
Phase 1, bounded by CPU and the model rate limit) and `maintenance` (MaxWorkers 1).
