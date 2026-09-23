// Pure parsing of the web container's environment. lib/runtime-config.ts wraps it with
// the server-only guard; this module has no Next.js dependencies so it is unit-testable.

/** Server-side configuration. `apiInternalUrl` is only for fetches made by the Next server. */
export interface RuntimeConfig {
  /** Domain served by this deployment, e.g. "cars". */
  domainKey: string;
  /** Browser-facing API base URL without a trailing slash, e.g. http://localhost:4000. */
  apiUrl: string;
  /** API base URL for server-side fetches (in-cluster service); defaults to apiUrl. */
  apiInternalUrl: string;
}

/** What the browser may know: returned by /api/config and passed to <Providers>. */
export interface PublicRuntimeConfig {
  domainKey: string;
  apiUrl: string;
}

export type RuntimeConfigResult =
  | { ok: true; config: RuntimeConfig }
  | { ok: false; problems: string[] };

type Env = Record<string, string | undefined>;

// Mirrors the Go validation of a domain key (alphanum, lowercase).
const DOMAIN_KEY_RE = /^[a-z0-9]+$/;

function pick(env: Env, ...names: string[]): { name: string; value: string } | undefined {
  for (const name of names) {
    const value = env[name]?.trim();
    if (value) return { name, value };
  }
  return undefined;
}

function normalizeBaseUrl(value: string): string | null {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return null;
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return null;
  if (url.search || url.hash) return null;
  return `${url.origin}${url.pathname}`.replace(/\/+$/, "");
}

/**
 * Parses DOMAIN_KEY, API_URL and API_INTERNAL_URL. For local development the
 * NEXT_PUBLIC_DOMAIN_KEY / NEXT_PUBLIC_API_URL names are accepted as fallbacks.
 */
export function parseRuntimeConfig(env: Env): RuntimeConfigResult {
  const problems: string[] = [];

  const key = pick(env, "DOMAIN_KEY", "NEXT_PUBLIC_DOMAIN_KEY");
  if (!key) {
    problems.push("DOMAIN_KEY is not set");
  } else if (!DOMAIN_KEY_RE.test(key.value)) {
    problems.push(`${key.name} must be lower-case letters and digits, got "${key.value}"`);
  }

  const api = pick(env, "API_URL", "NEXT_PUBLIC_API_URL");
  let apiUrl: string | null = null;
  if (!api) {
    problems.push("API_URL is not set");
  } else {
    apiUrl = normalizeBaseUrl(api.value);
    if (!apiUrl) problems.push(`${api.name} must be an http(s) URL without query or fragment, got "${api.value}"`);
  }

  const internal = pick(env, "API_INTERNAL_URL");
  let apiInternalUrl: string | null = apiUrl;
  if (internal) {
    apiInternalUrl = normalizeBaseUrl(internal.value);
    if (!apiInternalUrl) {
      problems.push(`API_INTERNAL_URL must be an http(s) URL without query or fragment, got "${internal.value}"`);
    }
  }

  if (problems.length > 0 || !key || !apiUrl || !apiInternalUrl) {
    return { ok: false, problems };
  }
  return { ok: true, config: { domainKey: key.value, apiUrl, apiInternalUrl } };
}

export function toPublicRuntimeConfig(config: RuntimeConfig): PublicRuntimeConfig {
  return { domainKey: config.domainKey, apiUrl: config.apiUrl };
}
