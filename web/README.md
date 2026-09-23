# v2i web

The Next.js frontend of the video-to-image platform (plan: `../plan/07-frontend.md`).
One codebase and **one container image serve every domain**: the domain key and API URL
are read from the environment at request time, and everything else (copy, limits, shot
labels) comes from `GET /v1/domains/{key}`.

Stack: Next.js 16 (App Router, standalone output), React 19, TypeScript (strict),
Tailwind CSS v4, shadcn/ui (Radix), TanStack Query v5, openapi-typescript + openapi-fetch.

## Run it locally

```sh
# 1. Backend on :4000 (repository root), with the free deterministic analyzer
make dev-fake                     # or: go run ./cmd/v2i serve --embed-worker --analyzer=fake

# 2. Frontend on :3000
cd web
npx --yes pnpm@12.5.1 install     # or `pnpm install` if pnpm 12 is installed
DOMAIN_KEY=cars API_URL=http://localhost:4000 npx --yes pnpm@12.5.1 dev
# (`make web-dev` from the root does the same)
```

Open http://localhost:3000. The page origin must be in the domain's `allowedOrigins`
(`configs/domains/cars.yaml` allows `http://localhost:3000`). To try a second domain, run
another instance on :3001 with a different `DOMAIN_KEY` and add `http://localhost:3001` to
that domain's `allowedOrigins`.

## Configuration

Read by the Next server **per request** (`lib/runtime-config.ts`), never baked into the build:

| Variable | Required | Meaning |
| --- | --- | --- |
| `DOMAIN_KEY` | yes | Domain served by this deployment, e.g. `cars` (lower-case letters/digits) |
| `API_URL` | yes | API base URL **as the browser sees it**, e.g. `https://api.cars.example.com`. Upload, polling, image and ZIP URLs use it |
| `API_INTERNAL_URL` | no | API base URL for server-side fetches (the domain config), e.g. the in-cluster Service `http://v2i-api:4000`. Defaults to `API_URL` |

For local development `NEXT_PUBLIC_DOMAIN_KEY` / `NEXT_PUBLIC_API_URL` are accepted as
fallbacks. Copy `.env.example` to `.env.local` if you prefer a file.

How the values reach the browser: the root layout (rendered dynamically) passes
`{domainKey, apiUrl}` and the fetched public domain config into `<Providers>`, so client
components need no extra round trip. `GET /api/config` returns the same `{domainKey, apiUrl}`
(no caching) for anything outside React.

## Health endpoints

Neither endpoint calls the API: an API outage must not restart or unready every web pod
(pages then show "Service temporarily unavailable" and recover by themselves).

| Endpoint | Probe | Answers |
| --- | --- | --- |
| `GET /api/healthz` | **liveness** | always `200 {"ok":true}` while the Node process serves requests |
| `GET /api/readyz` | **readiness** | `200 {"ok":true}` when `DOMAIN_KEY` / `API_URL` / `API_INTERNAL_URL` are valid; `503 {"ok":false,"problems":["DOMAIN_KEY is not set", …]}` otherwise (also logged) |

So a Deployment rolled out with a missing or malformed variable never becomes Ready and the
old pods keep serving. An unknown-but-well-formed `DOMAIN_KEY` (e.g. `car` instead of
`cars`) is only visible in the page logs (`domain: GET …/v1/domains/car returned HTTP 404
DOMAIN_NOT_FOUND`), because checking it would need the API. Kubernetes:

```yaml
livenessProbe:  { httpGet: { path: /api/healthz, port: 3000 } }
readinessProbe: { httpGet: { path: /api/readyz,  port: 3000 } }
```

If the variables are missing/invalid or the API is unreachable, pages render a friendly
"Service temporarily unavailable" state (with the exact problem listed in development) and
the server logs the reason. The domain config is cached by the Next server for 300 s.

## Scripts

| Script | What it does |
| --- | --- |
| `dev` | `next dev -p 3000` |
| `build` / `start` | production build (`output: "standalone"`) / serve it on :3000 |
| `lint` | ESLint (Next core-web-vitals + TypeScript rules) |
| `typecheck` | `next typegen && tsc --noEmit` |
| `test` | Vitest unit and component tests (jsdom + Testing Library) |
| `test:e2e` | Playwright end-to-end tests against `next dev` (see below) |
| `test:e2e:prod` | the same tests against the production standalone server: build, copy assets, `check:dynamic`, Playwright (`scripts/e2e-prod.mjs`) |
| `build:standalone` / `start:standalone` | `next build` + copy `.next/static` and `public/` into `.next/standalone` / run `node .next/standalone/server.js` (reads `PORT`, `HOSTNAME`) |
| `check:dynamic` | fails when `.next/prerender-manifest.json` lists any route besides `/_global-error` and `/icon.svg`; run after `build` |
| `gen:api` | Regenerates `lib/api.d.ts` from `../api/openapi.json` |

## API types

`lib/api.d.ts` is **generated and committed** (the Docker build does not need the Go spec):

```sh
cd .. && go run ./cmd/v2i openapi -o api/openapi.json   # or: make gen-openapi
cd web && npx --yes pnpm@12.5.1 gen:api                 # or: make gen-web (does both)
```

CI regenerates it and fails when the committed file differs. The generated unions are used
exhaustively: adding an error code (`components/error-message.tsx`), a pipeline step
(`components/job-steps.tsx`) or a job status (`components/job-view.tsx`) in Go breaks the
TypeScript build until the frontend handles it.

## Layout

```
app/
  layout.tsx              per-request config + domain config → <Providers>, <html data-domain>
  page.tsx                upload page
  jobs/[jobId]/page.tsx   progress + results (client <JobView>)
  api/config/route.ts     GET {domainKey, apiUrl}
  api/healthz/route.ts    liveness probe (always 200)
  api/readyz/route.ts     readiness probe (503 when the runtime config is invalid)
  not-found.tsx, error.tsx, globals.css (Tailwind + per-domain CSS variables), icon.svg
components/
  upload-form.tsx, upload-dropzone.tsx, upload-progress.tsx
  job-view.tsx, job-steps.tsx, job-states.tsx
  result-grid.tsx, result-card.tsx, lightbox.tsx, missing-shots.tsx, delete-job-button.tsx
  usage-summary.tsx       "AI usage: 9,012 tokens · $0.0041" + per-step table (results, rejected, failed, processing)
  error-message.tsx       every API error code → plain-language copy
  providers.tsx           QueryClientProvider + runtime/domain context
  ui/                     shadcn/ui components
lib/
  api.d.ts                GENERATED (openapi-typescript)
  api.ts                  typed openapi-fetch client, getJob/deleteJob, resolveApiUrl, error helpers
  upload.ts               chunked XMLHttpRequest upload: session, 2 chunks in flight, retries, complete, cancel
  upload-checks.ts        client pre-checks (size, type, duration, resolution)
  usage.ts                token / cost formatting for the job's model usage
  video-meta.ts           reads duration/resolution and a poster frame with a hidden <video>
  runtime-config*.ts      env parsing (server-only)
  domain.ts               server-side fetch of the public domain config
  theme.ts                domainKey → theme name (CSS variables in app/globals.css)
hooks/
  use-now.ts, use-deadline.ts   ticking clock / one timer at a deadline (expiry, Retry-After)
scripts/
  check-dynamic.mjs       build guard: nothing may be prerendered (decision 13)
  prepare-standalone.mjs  copies static assets next to .next/standalone/server.js
  e2e-prod.mjs            test:e2e:prod
```

Theming: `<html data-domain="cars">` selects the `[data-domain="cars"]` variable set in
`app/globals.css`; unknown domains fall back to the neutral default. Light/dark follow the
operating system.

## Testing

```sh
npx --yes pnpm@12.5.1 test        # unit + component tests
npx --yes pnpm@12.5.1 lint && npx --yes pnpm@12.5.1 typecheck
```

End-to-end (Playwright, Chromium only). Playwright starts the web app (`next dev` on :3000
with `DOMAIN_KEY=cars API_URL=http://localhost:4000`); **it does not start the backend**:

```sh
# repository root
make deps-up && go run ./cmd/v2i migrate
make fixtures                                            # testdata/videos/*.mp4
go run ./cmd/v2i serve --embed-worker --analyzer=fake    # :4000

# web/
npx --yes pnpm@12.5.1 exec playwright install chromium   # once
npx --yes pnpm@12.5.1 test:e2e                           # or `make e2e` from the root
```

Locally the specs **skip** with a clear message when `API_URL/healthz` is not reachable or a
fixture is missing. With `CI` set they **fail** instead, so a backend that no longer starts
cannot produce a green run. Overrides: `API_URL`, `DOMAIN_KEY`, `E2E_VIDEO`,
`E2E_REJECT_VIDEO`, `E2E_WEB_PORT`, and `E2E_WEB_COMMAND` (the command that serves the app
on `E2E_WEB_PORT`; Playwright passes it `PORT`). Playwright waits for `/api/readyz`.
Locally an already running server on the port is reused; stop it (or pick another
`E2E_WEB_PORT` whose origin the domain allows) to test your working tree.

Against the **production build** (the standalone server the container runs; `next dev`
renders every request dynamically, so only this catches a page that got prerendered):

```sh
npx --yes pnpm@12.5.1 test:e2e:prod               # build + prepare + check:dynamic + Playwright
npx --yes pnpm@12.5.1 test:e2e:prod --skip-build  # reuse an existing .next build
```

which is equivalent to:

```sh
npx --yes pnpm@12.5.1 build
node scripts/prepare-standalone.mjs   # .next/static and public/ into .next/standalone
node scripts/check-dynamic.mjs        # only /_global-error and /icon.svg may be prerendered
E2E_WEB_COMMAND="node .next/standalone/server.js" HOSTNAME=127.0.0.1 \
  npx --yes pnpm@12.5.1 exec playwright test
```

Every run uploads two videos, and the API allows 10 uploads per hour per IP
(`ratePerIpPerHour`); after that uploads answer 429 until the bucket refills.

## Container

```sh
# from the repository root; the build context is web/ (same as `make image-web`)
docker build -f deploy/docker/Dockerfile.web -t v2i-web web
docker run --rm -p 3000:3000 -e DOMAIN_KEY=cars -e API_URL=http://localhost:4000 \
  -e API_INTERNAL_URL=http://host.docker.internal:4000 v2i-web
```

Multi-stage build on `node:24-alpine`; the build stage also runs `check:dynamic`. The
standalone server runs as the non-root `node` user, declared numerically as
`USER 1000:1000` so Kubernetes can verify `runAsNonRoot: true`, on :3000
(`CMD ["node", "server.js"]`). The Docker `HEALTHCHECK` uses `/api/readyz`. The server
writes its fetch cache under `/app/.next/cache`; with a read-only root filesystem mount an
`emptyDir` there.

## Notes for the API

- Every upload is chunked, small files included (Cloudflare rejects request bodies over
  100 MB): `POST …/uploads` (JSON) → `PUT …/uploads/{id}/chunks/{i}`
  (`application/octet-stream`, sized as the session says, 2 in flight) →
  `POST …/uploads/{id}/complete` → the job page. Cross-origin every one of these requests is
  preflighted (`OPTIONS`), so `Access-Control-Allow-Methods` must include `PUT` and
  `DELETE`; job `DELETE` is preflighted too.
- A chunk is retried up to 4 times (1, 2, 4, 8 s plus up to 25 % jitter) on network
  errors, a 60 s stall, 5xx and 429; a `Retry-After` is waited out, unless it is over a
  minute, which ends the upload with the wait shown. Other 4xx answers are final. After
  2 failures in a row without an answer, the chunk probes the session with `complete`:
  `404 UPLOAD_NOT_FOUND` ends the upload with the "upload expired" copy, `409` means it
  is alive, and a `202` (every chunk had arrived, only the answers were lost) is the job.
- `complete` is retried the same way (network errors, a 60 s timeout, 5xx including
  502/504/520–524, 429/503): the API replays the outcome of a repeated complete and keeps
  the chunks after a 500. A `409 UPLOAD_INCOMPLETE` re-sends `details.missing` and
  completes again, at most 2 rounds. Once complete is sent, Cancel is disabled and the
  progress box says "Checking the video…".
- The session is DELETEd (`fetch` with `keepalive`, best effort) on Cancel, on
  `pagehide` / `beforeunload`, and when the upload fails before complete was sent, but not
  after `UPLOAD_NOT_FOUND`. Cancel during the create call lets it finish and DELETEs the
  session it returns. After complete was sent only a final 4xx answer DELETEs it; after a
  network error or 5xx the job may exist, or the server keeps the chunks for a retry.
- `<img>` thumbnails and download links are plain GETs without an `Origin` header.
- Upload `429 RATE_LIMITED` / `503 SERVER_BUSY`: the page reads `Retry-After` (it must stay
  in `Access-Control-Expose-Headers`), says "You can upload again in about N minutes" and
  keeps the upload button disabled until then.
- A completed job is re-fetched on window focus and at `expiresAt`; a `410`/`404` switches
  the page to the expired / not-found state (the ZIP and image links would otherwise open
  the API's JSON error body).
- Image/ZIP URLs in the job payload may be relative to the API base (resolved with
  `API_URL`) or absolute (presigned storage URLs are used as-is).
