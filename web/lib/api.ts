import createClient from "openapi-fetch";

import type { components, paths } from "./api.d";

// Typed access to the v2i API. Every type here is derived from lib/api.d.ts, which is
// generated from the Go handlers (pnpm gen:api), so a contract change in Go surfaces as
// a TypeScript error here or in the components.

type Schemas = components["schemas"];

export type Job = Schemas["Job"];
export type JobStatus = Job["status"];
export type JobResult = NonNullable<Schemas["JobResult"]>;
export type JobError = NonNullable<Schemas["JobError"]>;
export type Rejection = NonNullable<Schemas["Rejection"]>;
export type Progress = Schemas["Progress"];
export type ProgressStep = Schemas["ProgressStep"];
export type StepName = ProgressStep["name"];
export type StepStatus = ProgressStep["status"];
export type ResultImage = Schemas["ResultImage"];
export type ImageStudio = NonNullable<Schemas["ImageStudio"]>;
export type ImageStudioStatus = ImageStudio["status"];
export type StudioVariant = Schemas["StudioVariant"];
export type StudioStyle = StudioVariant["style"];
export type JobStudio = Schemas["JobStudio"];
export type JobStudioStatus = JobStudio["status"];
export type JobUsage = Schemas["JobUsage"];
export type UsageStep = Schemas["UsageStep"];
export type ShotRef = Schemas["ShotRef"];
export type PublicDomain = Schemas["PublicDomain"];
export type PublicLimits = Schemas["PublicLimits"];
export type CreateJobResponse = Schemas["CreateJobResponse"];
export type CreateUploadRequest = Schemas["CreateUploadRequest"];
export type UploadSession = Schemas["UploadSession"];
export type ErrorDetail = Schemas["ErrorDetail"];
export type ErrorCode = ErrorDetail["code"];

/** Every ErrorCode, at runtime. The type assertion below fails the build if one is missing. */
export const ERROR_CODES = [
  "DOMAIN_NOT_FOUND",
  "ORIGIN_NOT_ALLOWED",
  "UNAUTHORIZED",
  "INVALID_REQUEST",
  "UPLOAD_TOO_LARGE",
  "UNSUPPORTED_FORMAT",
  "CANNOT_DECODE",
  "TOO_LONG",
  "TOO_SHORT",
  "RESOLUTION_TOO_LOW",
  "RESOLUTION_TOO_HIGH",
  "SERVER_BUSY",
  "UPLOAD_NOT_FOUND",
  "UPLOAD_INCOMPLETE",
  "RATE_LIMITED",
  "JOB_NOT_FOUND",
  "IMAGE_NOT_FOUND",
  "NOT_READY",
  "EXPIRED",
  "MODEL_UNAVAILABLE",
  "MODEL_BAD_OUTPUT",
  "PROCESSING_ERROR",
  "NOT_FOUND",
  "INTERNAL_ERROR",
] as const satisfies readonly ErrorCode[];

type MissingErrorCodes = Exclude<ErrorCode, (typeof ERROR_CODES)[number]>;
// If this line errors, a code was added to the Go ErrorCode enum: add it to ERROR_CODES.
const allErrorCodesListed: [MissingErrorCodes] extends [never] ? true : MissingErrorCodes = true;
void allErrorCodesListed;

const errorCodeSet: ReadonlySet<string> = new Set(ERROR_CODES);

/** Narrows an arbitrary string (e.g. from a response body) to a known ErrorCode. */
export function isErrorCode(value: unknown): value is ErrorCode {
  return typeof value === "string" && errorCodeSet.has(value);
}

/** Statuses after which a job never changes again (polling stops). */
export const TERMINAL_STATUSES: readonly JobStatus[] = ["completed", "rejected", "failed", "expired"];

export function isTerminalStatus(status: JobStatus): boolean {
  return TERMINAL_STATUSES.includes(status);
}

/**
 * Pulls the error code out of an error envelope `{error: {code, message}}`. Returns null
 * when the body is not an envelope or carries a code this build does not know. The server
 * message is deliberately ignored: the UI maps codes to its own copy.
 */
export function errorCodeFromBody(body: unknown): ErrorCode | null {
  if (typeof body !== "object" || body === null || !("error" in body)) return null;
  const err = (body as { error: unknown }).error;
  if (typeof err !== "object" || err === null || !("code" in err)) return null;
  const code = (err as { code: unknown }).code;
  return isErrorCode(code) ? code : null;
}

/**
 * Best-effort code for a response without a usable envelope (for example an HTML error
 * page from a reverse proxy in front of the API).
 */
export function errorCodeFromStatus(status: number): ErrorCode | null {
  switch (status) {
    case 401:
      return "UNAUTHORIZED";
    case 403:
      return "ORIGIN_NOT_ALLOWED";
    case 410:
      return "EXPIRED";
    case 413:
      return "UPLOAD_TOO_LARGE";
    case 429:
      return "RATE_LIMITED";
    default:
      return status >= 500 ? "INTERNAL_ERROR" : null;
  }
}

/**
 * Parses a Retry-After header into whole seconds from now: either delta-seconds ("120")
 * or an HTTP date. Returns null when the header is missing, malformed or not in the
 * future, so callers can fall back to generic "try again later" copy.
 */
export function parseRetryAfter(value: string | null | undefined, nowMs: number = Date.now()): number | null {
  const v = value?.trim();
  if (!v) return null;
  if (/^\d+$/.test(v)) {
    const sec = Number(v);
    return Number.isSafeInteger(sec) && sec > 0 ? sec : null;
  }
  if (!/GMT$/i.test(v)) return null; // HTTP dates are always in GMT
  const at = Date.parse(v);
  if (Number.isNaN(at)) return null;
  const sec = Math.ceil((at - nowMs) / 1000);
  return sec > 0 ? sec : null;
}

export type ApiErrorKind = "http" | "network";

/** A failed API call. `kind: "network"` means no HTTP response arrived at all. */
export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status: number;
  readonly code: ErrorCode | null;

  constructor(kind: ApiErrorKind, status: number, code: ErrorCode | null, options?: { cause?: unknown }) {
    super(kind === "network" ? "api: network error" : `api: HTTP ${status}${code ? ` ${code}` : ""}`, options);
    this.name = "ApiError";
    this.kind = kind;
    this.status = status;
    this.code = code;
  }

  static fromResponse(status: number, body: unknown): ApiError {
    return new ApiError("http", status, errorCodeFromBody(body) ?? errorCodeFromStatus(status));
  }

  static network(cause: unknown): ApiError {
    return new ApiError("network", 0, null, { cause });
  }
}

/**
 * Transient failures are worth retrying: no response at all, rate limiting, or a server
 * error. 4xx answers such as 404 JOB_NOT_FOUND or 410 EXPIRED are final.
 */
export function isTransientError(error: unknown): boolean {
  if (!(error instanceof ApiError)) return true;
  if (error.kind === "network") return true;
  return error.status === 408 || error.status === 429 || error.status >= 500;
}

/**
 * Resolves a URL returned by the API (relative to the API base, e.g. `/v1/domains/…`)
 * against the browser-facing API base URL. Absolute URLs (presigned storage links) are
 * returned unchanged.
 */
export function resolveApiUrl(apiBaseUrl: string, path: string): string {
  if (/^[a-z][a-z\d+.-]*:/i.test(path) || path.startsWith("//")) return path;
  const base = apiBaseUrl.replace(/\/+$/, "");
  return `${base}${path.startsWith("/") ? "" : "/"}${path}`;
}

export type ApiClient = ReturnType<typeof createClient<paths>>;

export interface Api {
  /** The underlying typed openapi-fetch client. */
  readonly client: ApiClient;
  /** Browser-facing API base URL. */
  readonly baseUrl: string;
  getJob(domainKey: string, jobId: string, signal?: AbortSignal): Promise<Job>;
  deleteJob(domainKey: string, jobId: string): Promise<void>;
  resolveApiUrl(path: string): string;
}

/** Creates the API helpers for one base URL. `fetchImpl` is for tests. */
export function createApi(baseUrl: string, fetchImpl?: typeof fetch): Api {
  const client = createClient<paths>({
    baseUrl,
    // Resolve globalThis.fetch per call so test doubles installed later are honoured.
    fetch: fetchImpl ?? ((input) => globalThis.fetch(input)),
  });

  return {
    client,
    baseUrl,
    async getJob(domainKey, jobId, signal) {
      let res;
      try {
        res = await client.GET("/v1/domains/{domainKey}/jobs/{jobId}", {
          params: { path: { domainKey, jobId } },
          cache: "no-store",
          signal,
        });
      } catch (err) {
        if (signal?.aborted) throw err;
        throw ApiError.network(err);
      }
      if (res.data === undefined) throw ApiError.fromResponse(res.response.status, res.error);
      return res.data;
    },
    async deleteJob(domainKey, jobId) {
      let res;
      try {
        res = await client.DELETE("/v1/domains/{domainKey}/jobs/{jobId}", {
          params: { path: { domainKey, jobId } },
        });
      } catch (err) {
        throw ApiError.network(err);
      }
      if (!res.response.ok) throw ApiError.fromResponse(res.response.status, res.error);
    },
    resolveApiUrl(path) {
      return resolveApiUrl(baseUrl, path);
    },
  };
}
