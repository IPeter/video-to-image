# 05 — API Contract

Base path: `/v1`. JSON everywhere except the upload (multipart) and file downloads. Handlers
are typed Go structs registered with Huma, which validates requests and serves the OpenAPI 3.1
document at `/openapi.json`. The frontend's TypeScript types are generated from that document
(`openapi-typescript`), so the contract is defined once, in Go.

Domain resolution on every domain-scoped route:
1. `:domainKey` must exist in the registry → else `404 DOMAIN_NOT_FOUND`.
2. Request `Origin` must be in `domain.allowedOrigins` → else `403 ORIGIN_NOT_ALLOWED`.
   CORS preflight uses the same allowlist. Non-browser clients (the eval CLI) pass an
   `X-Internal-Token` instead.

## Endpoints

### `GET /v1/domains/:domainKey`
Public config for the frontend. Cached with `Cache-Control: max-age=300`.

```json
{
  "key": "cars",
  "name": "Car Listing Photos",
  "limits": { "maxUploadBytes": 524288000, "maxDurationSec": 180, "minDurationSec": 5 },
  "shots": [ { "id": "front_three_quarter", "label": "Front 3/4", "required": true }, ... ],
  "imageCount": 8,
  "copy": { "uploadTitle": "...", "uploadHint": "...", "rejectedTitle": "..." }
}
```

### `POST /v1/domains/:domainKey/jobs`
Multipart with one field `video`. Streams to storage, runs ffprobe validation, enqueues.

- `202 Accepted` → `{ "jobId": "job_01J...", "status": "queued" }`
- `400` → `{ "error": { "code": "UNSUPPORTED_FORMAT" | "CANNOT_DECODE" | "TOO_LONG" | "TOO_SHORT" | "RESOLUTION_TOO_LOW", "message": "..." } }`
- `413` → `UPLOAD_TOO_LARGE`
- `429` → `RATE_LIMITED` (per IP per domain, from `limits.ratePerIpPerHour`)

Job IDs are ULIDs, prefixed `job_`. They are unguessable enough to act as the access token for
the result in Phase 1 (no accounts). Rate-limit the status endpoint too.

### `GET /v1/domains/:domainKey/jobs/:jobId`
Current state. Frontend polls every 1.5 s while `queued`/`processing`.

```json
{
  "jobId": "job_01J...",
  "domainKey": "cars",
  "status": "queued" | "processing" | "completed" | "rejected" | "failed" | "expired",
  "createdAt": "2026-09-23T10:00:00Z",
  "expiresAt": "2026-09-24T10:00:00Z",
  "video": { "durationSec": 74.2, "width": 1920, "height": 1080, "fps": 29.97 },
  "progress": {
    "step": "analyze" | "extract" | "score" | "select" | "export" | null,
    "percent": 35,
    "steps": [
      { "name": "preview", "status": "done", "ms": 2100 },
      { "name": "analyze", "status": "running" }
    ]
  },
  "result": null | {
    "summary": "Walkaround of a silver sedan, exterior then interior.",
    "images": [
      {
        "id": "img_01J...",
        "index": 1,
        "shotId": "front_three_quarter",
        "shotLabel": "Front 3/4",
        "timestampSec": 6.25,
        "timestampLabel": "00:06",
        "width": 1920, "height": 1080,
        "reason": "Whole car visible at 45°, steady camera, even light.",
        "url": "/v1/domains/cars/jobs/job_01J.../images/img_01J...",
        "thumbnailUrl": "/v1/domains/cars/jobs/job_01J.../images/img_01J.../thumb"
      }
    ],
    "missingShots": [ { "id": "rear_three_quarter", "label": "Rear 3/4" } ],
    "explanation": "8 images selected. The rear 3/4 angle was not clearly visible in the video.",
    "zipUrl": "/v1/domains/cars/jobs/job_01J.../download.zip"
  },
  "rejection": null | { "reason": "The video shows a kitchen interior, not a vehicle.", "detectedSubject": "room interior" },
  "error": null | { "code": "MODEL_UNAVAILABLE" | "MODEL_BAD_OUTPUT" | "PROCESSING_ERROR", "message": "..." }
}
```

`404` if unknown; `410 EXPIRED` once files are cleaned up (status also becomes `expired`).

### `GET /v1/domains/:domainKey/jobs/:jobId/events` (Phase 2)
Server-Sent Events stream of the same payload on every change. Replaces polling. Implemented
with `http.Flusher` in Go; the worker publishes progress via Postgres `LISTEN/NOTIFY` on a
`job_progress` channel, which the API relays.

### `GET /v1/domains/:domainKey/jobs/:jobId/images/:imageId`
Full-resolution JPG. `?format=png|webp` in a later phase transcodes with `sharp`.
`Content-Disposition: attachment; filename="cars_01J_1_front_three_quarter_00-06.jpg"` when
`?download=1`. Served by redirecting to a presigned MinIO/S3 URL on k3s, streamed in dev.

### `GET /v1/domains/:domainKey/jobs/:jobId/images/:imageId/thumb`
640 px wide JPG for the results grid. Generated at export time.

### `GET /v1/domains/:domainKey/jobs/:jobId/download.zip`
All images + `manifest.json`.

### `DELETE /v1/domains/:domainKey/jobs/:jobId`
User-initiated early deletion. Removes files, sets status `expired`.

### `GET /healthz`
`{ ok: true, ffmpeg: "7.1", db: "ok", storage: "ok" }`. Used as the Kubernetes readiness probe.
`/livez` returns 200 unconditionally for the liveness probe. `/metrics` is Prometheus.

## Internal (not exposed through the frontend's CORS origin)

### `GET /internal/usage?from=&to=&domainKey=`
Token and cost totals per domain per day, from `usage_events`. Behind `X-Internal-Token`.

## Error envelope

Every non-2xx body is `{ "error": { "code": string, "message": string, "details"?: unknown } }`.
`code` is a stable machine string from the `ErrorCode` enum in `internal/httpapi`, listed in the
OpenAPI document as a string enum so the generated TypeScript type is exhaustive; the frontend
maps codes to copy. `message` is a safe English default.

## Versioning

Path-versioned (`/v1`). Additive changes do not bump. Removing or renaming a field bumps to `/v2`
and keeps `/v1` alive for one deployment cycle of all frontends.
