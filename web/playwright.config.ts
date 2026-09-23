import { defineConfig, devices } from "@playwright/test";

import { API_URL, DOMAIN_KEY, WEB_PORT, WEB_URL } from "./e2e/env";

// End-to-end tests. Playwright starts the web app; the backend is NOT started here and
// must already run on API_URL (default http://localhost:4000) with the deterministic fake
// analyzer, from the repository root:
//
//   make deps-up && go run ./cmd/v2i migrate
//   go run ./cmd/v2i serve --embed-worker --analyzer=fake
//
// Locally the specs skip themselves with a clear message when API_URL/healthz is not
// reachable or a fixture video is missing; with CI set they FAIL instead, so a broken
// backend can never produce a green run.
//
// Web server: `next dev` by default. `pnpm test:e2e:prod` (scripts/e2e-prod.mjs) builds and
// runs the production standalone server instead, via E2E_WEB_COMMAND.

export default defineConfig({
  testDir: "./e2e",
  timeout: 150_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: WEB_URL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    // E2E_WEB_COMMAND="node .next/standalone/server.js" serves the production build (see
    // scripts/e2e-prod.mjs); it listens on PORT below.
    command: process.env.E2E_WEB_COMMAND ?? `next dev -p ${WEB_PORT}`,
    // Readiness, not liveness: waits until the server runs with a valid runtime config.
    url: `${WEB_URL}/api/readyz`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    stdout: "pipe",
    stderr: "pipe",
    env: {
      DOMAIN_KEY,
      API_URL,
      PORT: String(WEB_PORT),
      NEXT_TELEMETRY_DISABLED: "1",
    },
  },
});
