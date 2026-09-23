// pnpm test:e2e:prod: the Playwright e2e against the production standalone server (what
// deploy/docker/Dockerfile.web runs) instead of `next dev`:
//
//   1. next build                     (skip with --skip-build or E2E_SKIP_BUILD=1)
//   2. scripts/prepare-standalone.mjs (.next/static and public/ into .next/standalone)
//   3. scripts/check-dynamic.mjs      (no page may be prerendered, decision 13)
//   4. playwright test with E2E_WEB_COMMAND="node .next/standalone/server.js"
//
// The standalone server reads PORT (playwright.config.ts sets it to E2E_WEB_PORT) and
// HOSTNAME (default here 127.0.0.1; set E2E_WEB_HOSTNAME to change it). Other arguments
// are passed to Playwright, e.g. `pnpm test:e2e:prod --skip-build --headed`.

import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(import.meta.url);

const args = process.argv.slice(2);
const skipBuild = args.includes("--skip-build") || process.env.E2E_SKIP_BUILD === "1";
const playwrightArgs = args.filter((a) => a !== "--skip-build");

function run(label, file, argv, env = process.env) {
  console.log(`\n[e2e:prod] ${label}`);
  const res = spawnSync(process.execPath, [file, ...argv], { cwd: webRoot, stdio: "inherit", env });
  if (res.error) throw res.error;
  if (res.status !== 0) process.exit(res.status ?? 1);
}

if (!skipBuild) {
  run("next build", require.resolve("next/dist/bin/next"), ["build"], { ...process.env, NEXT_TELEMETRY_DISABLED: "1" });
}
run("prepare standalone", path.join(webRoot, "scripts", "prepare-standalone.mjs"), []);
run("check that nothing was prerendered", path.join(webRoot, "scripts", "check-dynamic.mjs"), []);
run("playwright test (standalone server)", require.resolve("@playwright/test/cli"), ["test", ...playwrightArgs], {
  ...process.env,
  E2E_WEB_COMMAND: "node .next/standalone/server.js",
  HOSTNAME: process.env.E2E_WEB_HOSTNAME ?? "127.0.0.1",
});
