# 09 — Decisions

Taken on 2026-09-23. Each row is final unless revisited; the plan documents reflect these choices.

| # | Question | Decision | Rejected alternatives | Consequences |
| --- | --- | --- | --- | --- |
| 1 | Backend language | **Go** | TypeScript/Node, Python | OpenAPI generated from Go (Huma) + `openapi-typescript` for the frontend. River instead of BullMQ, so no Redis. Domain configs as YAML, not TS |
| 2 | Frontend split | **One codebase, deployed per domain** | App per domain; single deploy with hostname detection | One Next.js image; per-domain Deployment sets `NEXT_PUBLIC_DOMAIN_KEY` |
| 3 | Gate + scene finding | **One combined model call** | Two calls; combined with a two-stage flag | Video tokens paid once. `03-pipeline.md` step 2. Two-stage mode is not built |
| 4 | Model input | **Low-res preview video** (480p, 2 fps, no audio) | Grid of sampled frames | Uses the Gemini Files API. Keeps temporal context |
| 5 | Model re-rank of candidate frames | **Off by default, measured in Phase 2** | Always on; never | Config flag `selection.modelRerank`, implemented but disabled for cars |
| 6 | Images per car video | **8** | 5, 12 | 4 required shots + 4 optional |
| 7 | Accounts in Phase 1 | **None** | Shared password per domain; full accounts | ULID job URLs are the result handle; rate limit per IP per domain |
| 8 | Retention | **24 hours** | 1 hour, 7 days | `retentionHours: 24` for cars; per-domain override allowed |
| 9 | Direct-to-storage upload | **Phase 3** | From the start; never | Phase 1 streams through the API with a hard size cap |
| 10 | Hosting | **Kubernetes on the home k3s cluster** first, for testing and customer previews | Fly.io/Railway + Vercel + R2; single VPS; AWS | Plain manifests via kustomize, Traefik ingress, MinIO and Postgres in-cluster. `10-deployment-k3s.md`. A hosted cluster later is a new overlay |
| 11 | Scoring rows after file cleanup | **Keep indefinitely** | Delete with files; keep 30 days | `candidate_frames`, `scenes`, `usage_events` never deleted. Files always deleted at expiry. Add a table-size alert |
| 12 | UI language | **English first, localisable per domain** | Hungarian first; both | `copy` block in the domain YAML; a `copy_hu` block later is a config change |
| 13 | How a frontend knows its domain | **Environment variable per deployment** | Hostname detection; both | Explicit; one Deployment per domain |

## Still to confirm against current external docs before Phase 1 step 3

- Gemini Files API size limits and TTL; inline video size limit.
- Whether `videoMetadata.fps` and low media resolution are supported for the chosen model in
  the Go SDK.
- Exact `ResponseSchema` support and any field-type restrictions for structured output.
- Current model IDs and prices (`gemini-3.5-flash-lite`, `gemini-3.8-flash`); the brief dates
  them to 2026-09-23.
