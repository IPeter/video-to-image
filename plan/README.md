# Video-to-Image Platform — Plan

One backend, many frontends. Each frontend lives on its own domain, serves one vertical
(cars, real estate, ...), carries a hard-coded selection prompt, and only accepts videos that
belong to that vertical. The backend validates the video against the domain, finds the relevant
scenes with a vision model, extracts sharp, non-duplicate frames with FFmpeg, and exports them
as JPGs plus a ZIP.

The generic single-prompt product brief lives in `../docs/main-app.md`. This folder turns it into
a multi-domain architecture and an implementation plan. The first domain is a **car dealership**.

## Documents

| File | What it covers |
| --- | --- |
| [01-architecture.md](01-architecture.md) | System overview, components, how domains map to frontends, end-to-end request flow |
| [02-tech-stack.md](02-tech-stack.md) | Go backend stack, Next.js frontend stack, repository layout, how types are shared |
| [03-pipeline.md](03-pipeline.md) | Processing pipeline in detail: validation gate, scene finding, frame extraction, quality scoring, selection, export |
| [04-domain-config.md](04-domain-config.md) | The domain registry format (YAML, mounted as a ConfigMap) and the full car dealership configuration |
| [05-api.md](05-api.md) | HTTP API contract between frontends and backend, generated as OpenAPI from Go |
| [06-data-model.md](06-data-model.md) | Database schema, storage layout, retention and cleanup |
| [07-frontend.md](07-frontend.md) | Frontend app structure, pages, states, how one codebase serves many domains |
| [08-roadmap.md](08-roadmap.md) | Phased milestones, MVP definition, evaluation plan, cost controls |
| [09-decisions.md](09-decisions.md) | The 13 decisions taken on 2026-09-23, with the alternatives that were rejected |
| [10-deployment-k3s.md](10-deployment-k3s.md) | Kubernetes layout for the home k3s cluster: workloads, ingress, storage, secrets, image build |
| [11-implementation-notes.md](11-implementation-notes.md) | What Phase 0 + 1 implemented differently or in addition, and what is still open |

## Decisions made (see `09-decisions.md` for the full list)

- **Backend in Go.** Single binary `v2i` with `serve`, `work`, `migrate` and `eval` subcommands,
  one container image. HTTP via Huma (generates OpenAPI from Go types), jobs via River
  (Postgres-backed, so no Redis), FFmpeg via `os/exec`, Gemini via the official Go SDK.
- **Frontend in Next.js + Tailwind + shadcn/ui**, one codebase deployed once per domain, domain
  key from an environment variable. TypeScript types are generated from the Go OpenAPI spec.
- **Domain = a YAML config file**, not a code branch. In Kubernetes the files are a ConfigMap.
  Adding real estate later means one YAML file and one frontend Deployment.
- **Validation and scene finding happen in one model call** to avoid paying for the video twice.
- **The model finds content, the app makes files.** Model timestamps are hints; FFmpeg samples
  densely around them and the app picks the final frames by sharpness, variety and shot coverage.
- **Gemini 3.5 Flash-Lite** as the default model per the brief, configurable per domain.
- **First deployment target is the home k3s cluster**, used for testing and customer previews.
  Everything is plain Kubernetes manifests so a later move to a hosted cluster is a new overlay.
- **No accounts, 24-hour retention, 8 images per car video, model re-rank off by default,
  English copy first, scoring rows kept indefinitely.**

## How to use this plan

Work through `08-roadmap.md` phase by phase. Phase 1 produces a working end-to-end car
dealership flow with no design polish, runnable locally with Docker Compose. Phase 3 puts it on
k3s. Design is applied in a later phase, on top of a stable API.
