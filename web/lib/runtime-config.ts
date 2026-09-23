import "server-only";

import { parseRuntimeConfig, type RuntimeConfigResult } from "./runtime-config-parse";

export type { PublicRuntimeConfig, RuntimeConfig, RuntimeConfigResult } from "./runtime-config-parse";
export { toPublicRuntimeConfig } from "./runtime-config-parse";

/**
 * Reads the runtime configuration from process.env. Call it while rendering (never at
 * module scope) so the values of the running container are used, not the build's: one
 * image serves every domain (decision 13).
 */
export function getRuntimeConfig(): RuntimeConfigResult {
  return parseRuntimeConfig(process.env);
}
