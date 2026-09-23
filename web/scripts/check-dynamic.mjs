// Fails when `next build` prerendered anything but Next's own static routes.
//
// Decision 13: one image serves every domain, so every page must render per request
// with the container's DOMAIN_KEY / API_URL. If a refactor drops `dynamic = "force-dynamic"`
// from app/layout.tsx and `connection()` from lib/domain.ts, `next build` prerenders the
// pages with the build machine's env (usually none: the "Service temporarily unavailable"
// page) and bakes them into the image. `next dev` renders everything per request, so the
// e2e cannot notice; this check can. Run it after `pnpm build`:
//
//   node scripts/check-dynamic.mjs [path/to/prerender-manifest.json]
//
// Next itself always prerenders /_global-error, and app/icon.svg is a static file
// route; neither depends on the environment. (/_not-found is not prerendered because the
// root layout is dynamic.)

import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

/** Routes Next may prerender without breaking the runtime configuration. */
export const ALLOWED_PRERENDERED = Object.freeze(["/_global-error", "/icon.svg"]);

/**
 * Returns the prerendered routes (static and ISR) that are not allowed.
 * @param {{routes?: Record<string, unknown>, dynamicRoutes?: Record<string, unknown>}} manifest
 * @returns {string[]}
 */
export function unexpectedPrerenderedRoutes(manifest) {
  const allowed = new Set(ALLOWED_PRERENDERED);
  const routes = [...Object.keys(manifest.routes ?? {}), ...Object.keys(manifest.dynamicRoutes ?? {})];
  return routes.filter((r) => !allowed.has(r)).sort();
}

function main() {
  const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  const file = process.argv[2] ?? path.join(webRoot, ".next", "prerender-manifest.json");
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(file, "utf8"));
  } catch (err) {
    console.error(`check-dynamic: cannot read ${file} (run \`pnpm build\` first): ${err.message}`);
    process.exit(2);
  }
  const bad = unexpectedPrerenderedRoutes(manifest);
  if (bad.length > 0) {
    console.error(
      `check-dynamic: these routes were prerendered at build time: ${bad.join(", ")}.\n` +
        "Pages must render per request so DOMAIN_KEY/API_URL come from the running container " +
        '(keep `export const dynamic = "force-dynamic"` in app/layout.tsx and `await connection()` in lib/domain.ts).',
    );
    process.exit(1);
  }
  console.log(`check-dynamic: ok (only ${ALLOWED_PRERENDERED.join(", ")} prerendered)`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) main();
