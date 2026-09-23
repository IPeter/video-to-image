import "server-only";

import { connection } from "next/server";
import { cache } from "react";

import { errorCodeFromBody, type ErrorCode, type PublicDomain } from "./api";
import { getRuntimeConfig, type RuntimeConfig } from "./runtime-config";

/** How long the Next server caches the public domain config (matches the API's max-age). */
export const DOMAIN_REVALIDATE_SECONDS = 300;
const FETCH_TIMEOUT_MS = 5000;

export type DomainLoadResult =
  | { ok: true; config: RuntimeConfig; domain: PublicDomain }
  | { ok: false; reason: "config"; problems: string[] }
  | {
      ok: false;
      reason: "unavailable";
      domainKey: string;
      status: number | null;
      code: ErrorCode | null;
      /** Operator-facing description, shown only in development. */
      detail: string;
    };

type FetchLike = (input: string, init?: RequestInit & { next?: { revalidate?: number } }) => Promise<Response>;

/**
 * Fetches GET {apiInternalUrl}/v1/domains/{key}. Never throws: an unreachable API, an
 * unknown key or a malformed body all become `{ok: false, reason: "unavailable"}` so the
 * page can render a friendly error instead of crashing.
 */
export async function fetchDomainConfig(
  config: RuntimeConfig,
  fetchImpl: FetchLike = fetch,
): Promise<DomainLoadResult> {
  const url = `${config.apiInternalUrl}/v1/domains/${encodeURIComponent(config.domainKey)}`;
  let res: Response;
  try {
    res = await fetchImpl(url, {
      headers: { Accept: "application/json" },
      next: { revalidate: DOMAIN_REVALIDATE_SECONDS },
      signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
  } catch (err) {
    const detail = `GET ${url} failed: ${err instanceof Error ? err.message : String(err)}`;
    console.error(`domain: ${detail}`);
    return { ok: false, reason: "unavailable", domainKey: config.domainKey, status: null, code: null, detail };
  }

  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    // Non-JSON body: handled below.
  }

  if (!res.ok) {
    const code = errorCodeFromBody(body);
    const detail = `GET ${url} returned HTTP ${res.status}${code ? ` ${code}` : ""}`;
    console.error(`domain: ${detail}`);
    return { ok: false, reason: "unavailable", domainKey: config.domainKey, status: res.status, code, detail };
  }
  if (!isPublicDomain(body)) {
    const detail = `GET ${url} returned an unexpected body`;
    console.error(`domain: ${detail}`);
    return { ok: false, reason: "unavailable", domainKey: config.domainKey, status: res.status, code: null, detail };
  }
  return { ok: true, config, domain: body };
}

function isPublicDomain(v: unknown): v is PublicDomain {
  if (typeof v !== "object" || v === null) return false;
  const d = v as Partial<PublicDomain>;
  return (
    typeof d.key === "string" &&
    typeof d.name === "string" &&
    typeof d.limits === "object" &&
    d.limits !== null &&
    typeof d.limits.maxUploadBytes === "number" &&
    Array.isArray(d.shots) &&
    typeof d.copy === "object" &&
    d.copy !== null &&
    typeof d.copy.uploadTitle === "string"
  );
}

/**
 * Runtime config + public domain config for the current request. Memoised per request
 * (layout, metadata and pages share one fetch). Opting into dynamic rendering here means
 * no page is prerendered at build time, so the build needs neither the API nor env vars.
 */
export const loadDomain = cache(async (): Promise<DomainLoadResult> => {
  await connection();
  const rc = getRuntimeConfig();
  if (!rc.ok) {
    console.error(`config: ${rc.problems.join("; ")}`);
    return { ok: false, reason: "config", problems: rc.problems };
  }
  return fetchDomainConfig(rc.config);
});
