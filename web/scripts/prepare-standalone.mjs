// Copies .next/static and public/ next to .next/standalone/server.js: the standalone
// server only contains the traced runtime files and serves the static assets from there
// (deploy/docker/Dockerfile.web does the same with COPY). Run it after `next build`.

import { cpSync, existsSync, rmSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const standalone = path.join(webRoot, ".next", "standalone");

if (!existsSync(path.join(standalone, "server.js"))) {
  console.error(`prepare-standalone: ${standalone}/server.js is missing: run \`pnpm build\` first (output: "standalone").`);
  process.exit(1);
}

for (const [from, to] of [
  [path.join(webRoot, ".next", "static"), path.join(standalone, ".next", "static")],
  [path.join(webRoot, "public"), path.join(standalone, "public")],
]) {
  rmSync(to, { recursive: true, force: true });
  if (existsSync(from)) cpSync(from, to, { recursive: true });
}
console.log(`prepare-standalone: copied .next/static and public/ into ${path.relative(webRoot, standalone)}`);
