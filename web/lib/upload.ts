import type { paths } from "./api.d";
import {
  errorCodeFromBody,
  errorCodeFromStatus,
  parseRetryAfter,
  type CreateJobResponse,
  type CreateUploadRequest,
  type ErrorCode,
  type UploadSession,
} from "./api";

// Chunked video upload with XMLHttpRequest, because fetch() still cannot report upload
// progress in every browser. A reverse proxy may cap request bodies (Cloudflare: 100 MB),
// so every file goes through the chunked API, small ones as a single chunk:
//
//   POST   {api}/v1/domains/{key}/uploads                    → 201 UploadSession
//   PUT    {api}/v1/domains/{key}/uploads/{id}/chunks/{i}    → 204 (raw bytes, 2 in flight)
//   POST   {api}/v1/domains/{key}/uploads/{id}/complete      → 202 {jobId}
//   DELETE {api}/v1/domains/{key}/uploads/{id}               → best effort, fetch keepalive
//
// A chunk is retried on network errors, stalls, timeouts, 5xx and 429 (not before its
// Retry-After); any other 4xx answer is final. After 2 network failures in a row a chunk
// probes the session with complete, which answers 404 for a session the server lost.
//
// Complete is retried like a chunk: the server replays the outcome of a repeated complete
// (the same job, or the same rejection) and keeps the chunks after a 500. A 409
// UPLOAD_INCOMPLETE lists missing chunks, which are sent again (2 rounds at most).
//
// The session is DELETEd on cancel, when the page is left (pagehide / beforeunload), and
// when the upload fails before complete was sent (except for UPLOAD_NOT_FOUND). Once
// complete was sent only a final 4xx answer DELETEs it: after a lost answer or a 5xx the
// job may exist, or the server keeps the chunks for a retry. Cancel is then no longer
// possible (UploadProgress.completing).
//
// Cross-origin, every one of these requests is CORS-preflighted (JSON body, PUT/DELETE,
// octet-stream body, upload progress listener), so the API must answer OPTIONS for them
// and allow PUT.

export interface UploadProgress {
  /** Bytes sent so far; never decreases, even when a chunk is re-sent. */
  loaded: number;
  /** 0 when the browser cannot compute the total. */
  total: number;
  /** 0–100, or null when the total is unknown. 100 only once every chunk is stored. */
  percent: number | null;
  /** A chunk failed and waits to be sent again (the connection dropped, the server was busy). */
  retrying: boolean;
  /**
   * Complete has been sent: the server checks the video and creates the job. From here
   * on the upload can no longer be cancelled (abort() stops waiting but keeps the session).
   */
  completing: boolean;
}

export type UploadFailure =
  /**
   * The server answered with a non-2xx status. `code` is null when no known code could be
   * derived. `retryAfterSec` is set when the answer carried a usable Retry-After header
   * (429 RATE_LIMITED, 503 SERVER_BUSY); the API exposes it to CORS callers.
   */
  | { kind: "http"; status: number; code: ErrorCode | null; retryAfterSec?: number }
  /** No response: offline, DNS, CORS refusal, or the connection dropped mid-upload. */
  | { kind: "network" }
  /** A request took longer than `timeoutMs`, or sent nothing for `stallTimeoutMs`. */
  | { kind: "timeout" }
  /** Cancelled by the user (abort() or the AbortSignal). */
  | { kind: "aborted" };

export class UploadError extends Error {
  readonly failure: UploadFailure;

  constructor(failure: UploadFailure) {
    const detail = failure.kind === "http" ? ` ${failure.status}${failure.code ? ` ${failure.code}` : ""}` : "";
    super(`upload: ${failure.kind}${detail}`);
    this.name = "UploadError";
    this.failure = failure;
  }
}

export interface UploadOptions {
  apiUrl: string;
  domainKey: string;
  file: Blob;
  /** File name sent to the API (defaults to File.name, else "video"). */
  fileName?: string;
  onProgress?: (progress: UploadProgress) => void;
  /** Aborting the signal cancels the upload, like UploadHandle.abort(). */
  signal?: AbortSignal;
  /** Milliseconds per request; 0 (default) means none: chunk stalls are caught by stallTimeoutMs. */
  timeoutMs?: number;
  /** A chunk request that sends nothing for this long is abandoned and retried (default 60 s; 0 = off). */
  stallTimeoutMs?: number;
  /** Chunks in flight at once (default 2). */
  concurrency?: number;
  /** Retries per chunk or complete after the first attempt (default 4: after 1, 2, 4 and 8 s plus jitter). */
  maxRetries?: number;
  /** Milliseconds per complete request (default 60 s; timeoutMs when that is set). A timeout is retried. */
  completeTimeoutMs?: number;
  /** For tests. */
  createXhr?: () => XMLHttpRequest;
  /** For tests: sends the session DELETE (default: the global fetch, with keepalive). */
  fetch?: typeof fetch;
  /** For tests: where pagehide and beforeunload are listened for (default: window; null = nowhere). */
  pageEvents?: Pick<EventTarget, "addEventListener" | "removeEventListener"> | null;
  /** For tests: the jitter source, [0, 1). */
  random?: () => number;
}

export interface UploadHandle {
  /** Resolves with the 202 body of the complete call; rejects with UploadError. */
  promise: Promise<CreateJobResponse>;
  /**
   * Cancels the upload; the promise rejects with kind "aborted". No-op once settled.
   * Before complete was sent the session is DELETEd (during create: once its answer
   * arrives). Afterwards the job may be created anyway, so the session is kept.
   */
  abort(): void;
}

export const DEFAULT_CONCURRENCY = 2;
export const DEFAULT_MAX_RETRIES = 4;
export const DEFAULT_STALL_TIMEOUT_MS = 60_000;
/** First retry delay; doubles per retry (1, 2, 4, 8 s). */
export const RETRY_BASE_MS = 1000;
/** Up to this fraction of the delay is added at random, so parallel chunks do not retry in lockstep. */
export const RETRY_JITTER = 0.25;
/**
 * A Retry-After longer than this ends the upload instead of stalling it silently: the
 * form then tells the user how long to wait (an hourly rate limit asks for minutes).
 */
export const MAX_RETRY_AFTER_MS = 60_000;
/**
 * Complete answers in about a second; Cloudflare gives up after 100 s (524). A request
 * that hangs longer is abandoned and retried, since Cancel is disabled meanwhile.
 */
export const DEFAULT_COMPLETE_TIMEOUT_MS = 60_000;
/** How often a 409 UPLOAD_INCOMPLETE leads to re-sending the missing chunks. */
export const MAX_RESEND_ROUNDS = 2;
/** Network failures in a row after which a chunk checks that the session still exists. */
export const PROBE_AFTER_NETWORK_FAILURES = 2;
/** CreateUploadRequest.fileName maxLength, in code points (the API counts runes). */
export const MAX_FILE_NAME_LENGTH = 255;
const DISCARD_TIMEOUT_MS = 15_000;

// --- URLs, tied to the generated route table ---------------------------------------

type UploadRoute =
  | "/v1/domains/{domainKey}/uploads"
  | "/v1/domains/{domainKey}/uploads/{uploadId}"
  | "/v1/domains/{domainKey}/uploads/{uploadId}/chunks/{index}"
  | "/v1/domains/{domainKey}/uploads/{uploadId}/complete";
// If this line errors, a chunked-upload route was renamed in Go (lib/api.d.ts is generated).
const uploadRoutesExist: [Exclude<UploadRoute, keyof paths>] extends [never] ? true : never = true;
void uploadRoutesExist;

type CreateUploadResponse =
  paths["/v1/domains/{domainKey}/uploads"]["post"]["responses"][201]["content"]["application/json"];
type CompleteUploadResponse =
  paths["/v1/domains/{domainKey}/uploads/{uploadId}/complete"]["post"]["responses"][202]["content"]["application/json"];

export interface UploadUrls {
  create: string;
  session(uploadId: string): string;
  chunk(uploadId: string, index: number): string;
  complete(uploadId: string): string;
}

function routeUrl(apiUrl: string, route: UploadRoute, params: { domainKey: string; uploadId?: string; index?: number }) {
  const path = route.replace(/\{(\w+)\}/g, (_, name: keyof typeof params) => encodeURIComponent(String(params[name])));
  return `${apiUrl.replace(/\/+$/, "")}${path}`;
}

export function uploadUrls(apiUrl: string, domainKey: string): UploadUrls {
  return {
    create: routeUrl(apiUrl, "/v1/domains/{domainKey}/uploads", { domainKey }),
    session: (uploadId) => routeUrl(apiUrl, "/v1/domains/{domainKey}/uploads/{uploadId}", { domainKey, uploadId }),
    chunk: (uploadId, index) =>
      routeUrl(apiUrl, "/v1/domains/{domainKey}/uploads/{uploadId}/chunks/{index}", { domainKey, uploadId, index }),
    complete: (uploadId) =>
      routeUrl(apiUrl, "/v1/domains/{domainKey}/uploads/{uploadId}/complete", { domainKey, uploadId }),
  };
}

// --- helpers ---------------------------------------------------------------------------

/** Shortens a name to what the API accepts, keeping a short extension. */
export function clampFileName(name: string): string {
  const chars = Array.from(name);
  if (chars.length === 0) return "video";
  if (chars.length <= MAX_FILE_NAME_LENGTH) return name;
  const dot = chars.lastIndexOf(".");
  const ext = dot > 0 && chars.length - dot <= 16 ? chars.slice(dot) : [];
  return [...chars.slice(0, MAX_FILE_NAME_LENGTH - ext.length), ...ext].join("");
}

/** The delay before retry number `retry` (0-based): 1, 2, 4, 8 s … plus up to 25 % jitter. */
export function backoffMs(retry: number, random: () => number = Math.random): number {
  const base = RETRY_BASE_MS * 2 ** retry;
  return Math.round(base * (1 + RETRY_JITTER * random()));
}

/** A Blob.slice plan that matches the server's session exactly, or null when it does not add up. */
export interface ChunkPlan {
  uploadId: string;
  chunkSize: number;
  count: number;
}

export function chunkPlan(session: unknown, sizeBytes: number): ChunkPlan | null {
  if (typeof session !== "object" || session === null) return null;
  const { uploadId, chunkSizeBytes, chunkCount } = session as Partial<Record<keyof UploadSession, unknown>>;
  if (typeof uploadId !== "string" || uploadId === "") return null;
  if (typeof chunkSizeBytes !== "number" || !Number.isSafeInteger(chunkSizeBytes) || chunkSizeBytes <= 0) return null;
  if (typeof chunkCount !== "number" || chunkCount !== Math.max(1, Math.ceil(sizeBytes / chunkSizeBytes))) return null;
  return { uploadId, chunkSize: chunkSizeBytes, count: chunkCount };
}

/** [start, end) of chunk `index`; every chunk but the last is exactly plan.chunkSize. */
export function chunkRange(plan: Pick<ChunkPlan, "chunkSize">, index: number, sizeBytes: number): [number, number] {
  const start = index * plan.chunkSize;
  return [start, Math.min(start + plan.chunkSize, sizeBytes)];
}

function parseJson(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function isJobResponse(v: unknown): v is CompleteUploadResponse {
  if (typeof v !== "object" || v === null) return false;
  const jobId = (v as { jobId?: unknown }).jobId;
  return typeof jobId === "string" && jobId.length > 0;
}

/**
 * The chunks a 409 UPLOAD_INCOMPLETE lists in `error.details.missing` (the server lists
 * the first 20), or null when the body is not such an answer or the list is unusable.
 */
export function missingChunks(body: unknown, chunkCount: number): number[] | null {
  if (errorCodeFromBody(body) !== "UPLOAD_INCOMPLETE") return null;
  const details = (body as { error: { details?: unknown } }).error.details;
  if (typeof details !== "object" || details === null) return null;
  const missing = (details as { missing?: unknown }).missing;
  if (!Array.isArray(missing) || missing.length === 0) return null;
  const valid = missing.every((i) => typeof i === "number" && Number.isSafeInteger(i) && i >= 0 && i < chunkCount);
  return valid ? [...new Set(missing as number[])] : null;
}

/** A response header, or null when it is absent or not exposed by CORS. */
function responseHeader(xhr: XMLHttpRequest, name: string): string | null {
  try {
    return xhr.getResponseHeader(name);
  } catch {
    return null;
  }
}

/** What one request ended with. */
type Outcome =
  | { kind: "answer"; status: number; body: unknown; retryAfterSec: number | null }
  | { kind: "network" }
  | { kind: "timeout" }
  | { kind: "aborted" };

type Answer = Extract<Outcome, { kind: "answer" }>;

function isSuccess(o: Outcome): o is Answer {
  return o.kind === "answer" && o.status >= 200 && o.status < 300;
}

function failureOf(o: Outcome): UploadFailure {
  if (o.kind !== "answer") return o;
  const code = errorCodeFromBody(o.body) ?? errorCodeFromStatus(o.status);
  return { kind: "http", status: o.status, code, ...(o.retryAfterSec !== null && { retryAfterSec: o.retryAfterSec }) };
}

interface RequestSpec {
  method: "POST" | "PUT" | "DELETE";
  url: string;
  body?: Blob | string;
  contentType?: string;
  timeoutMs: number;
  /** Abandons the request (outcome "timeout") when no upload progress was seen for this long. */
  stallTimeoutMs?: number;
  onUploadProgress?: (loaded: number) => void;
  signal: AbortSignal;
}

/** One XHR. Never rejects: every ending is an Outcome. */
function send(createXhr: () => XMLHttpRequest, spec: RequestSpec): Promise<Outcome> {
  return new Promise((resolve) => {
    if (spec.signal.aborted) {
      resolve({ kind: "aborted" });
      return;
    }
    let xhr: XMLHttpRequest;
    try {
      xhr = createXhr();
    } catch {
      resolve({ kind: "network" });
      return;
    }
    let done = false;
    let stallTimer: ReturnType<typeof setTimeout> | undefined;

    const finish = (outcome: Outcome) => {
      if (done) return;
      done = true;
      clearTimeout(stallTimer);
      spec.signal.removeEventListener("abort", onAbort);
      resolve(outcome);
    };
    // Finish first so the onabort handler (which may fire synchronously) is a no-op.
    const stop = (outcome: Outcome) => {
      finish(outcome);
      try {
        xhr.abort();
      } catch {
        // Nothing to cancel.
      }
    };
    const onAbort = () => stop({ kind: "aborted" });
    const armStallTimer = () => {
      if (!spec.stallTimeoutMs || done) return;
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => stop({ kind: "timeout" }), spec.stallTimeoutMs);
    };

    spec.signal.addEventListener("abort", onAbort, { once: true });
    if (spec.onUploadProgress || spec.stallTimeoutMs) {
      xhr.upload.onprogress = (e: ProgressEvent) => {
        if (done) return;
        armStallTimer();
        spec.onUploadProgress?.(e.loaded);
      };
    }
    xhr.onload = () => {
      if (xhr.status === 0) {
        finish({ kind: "network" });
        return;
      }
      finish({
        kind: "answer",
        status: xhr.status,
        body: parseJson(typeof xhr.responseText === "string" ? xhr.responseText : ""),
        retryAfterSec: xhr.status >= 400 ? parseRetryAfter(responseHeader(xhr, "Retry-After")) : null,
      });
    };
    xhr.onerror = () => finish({ kind: "network" });
    xhr.ontimeout = () => finish({ kind: "timeout" });
    // Only reached when the browser aborts on its own (e.g. navigation); ours finish first.
    xhr.onabort = () => finish({ kind: "network" });

    try {
      xhr.open(spec.method, spec.url);
      xhr.responseType = "text";
      xhr.timeout = spec.timeoutMs;
      xhr.setRequestHeader("Accept", "application/json");
      if (spec.contentType) xhr.setRequestHeader("Content-Type", spec.contentType);
      xhr.send(spec.body ?? null);
      armStallTimer();
    } catch {
      finish({ kind: "network" });
    }
  });
}

/** Resolves after `ms`; rejects with an aborted UploadError when the signal fires first. */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new UploadError({ kind: "aborted" }));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(new UploadError({ kind: "aborted" }));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Worth another attempt: no answer, a stall or timeout, a server or proxy error (500,
 * 502/504, Cloudflare 520–524) or rate limiting. Other 4xx answers are final. This holds
 * for complete too: a repeated complete gets the outcome of the first one (the same job
 * or rejection), and after a 500 the server keeps the chunks so complete can run again.
 */
function isTransient(f: UploadFailure): boolean {
  if (f.kind === "network" || f.kind === "timeout") return true;
  return f.kind === "http" && (f.status === 429 || f.status >= 500);
}

/** A chunk request that got no answer at all (the server may have lost the session: D6). */
function isNoAnswer(f: UploadFailure): boolean {
  return f.kind === "network" || f.kind === "timeout";
}

/** What a complete sent as a session probe (after chunk network failures) found out. */
type ProbeVerdict =
  /** The session is alive (409), or the probe did not tell (no answer, 5xx, 429). */
  | { kind: "unknown" }
  /** Every chunk was stored after all: the probe created the job. */
  | { kind: "job"; job: CompleteUploadResponse }
  /** The upload is over: the session is gone (404 UPLOAD_NOT_FOUND), or its video was rejected. */
  | { kind: "final"; failure: UploadFailure };

function probeVerdict(o: Outcome): ProbeVerdict {
  if (o.kind !== "answer") return { kind: "unknown" };
  if (o.status >= 200 && o.status < 300) return isJobResponse(o.body) ? { kind: "job", job: o.body } : { kind: "unknown" };
  if (o.status < 400 || o.status >= 500 || o.status === 408 || o.status === 409 || o.status === 429) {
    return { kind: "unknown" };
  }
  return { kind: "final", failure: failureOf(o) };
}

/** Thrown out of the chunk workers when a probe found the job already created. */
class JobCreatedByProbe extends Error {}


// --- the upload --------------------------------------------------------------------------

function defaultPageEvents(): UploadOptions["pageEvents"] {
  return typeof window !== "undefined" ? window : null;
}

/** Starts the upload immediately. */
export function uploadVideo(opts: UploadOptions): UploadHandle {
  const createXhr = opts.createXhr ?? (() => new XMLHttpRequest());
  const concurrency = Math.max(1, Math.floor(opts.concurrency ?? DEFAULT_CONCURRENCY));
  const maxRetries = Math.max(0, Math.floor(opts.maxRetries ?? DEFAULT_MAX_RETRIES));
  const stallTimeoutMs = opts.stallTimeoutMs ?? DEFAULT_STALL_TIMEOUT_MS;
  const timeoutMs = opts.timeoutMs ?? 0;
  const completeTimeoutMs = opts.completeTimeoutMs ?? (timeoutMs > 0 ? timeoutMs : DEFAULT_COMPLETE_TIMEOUT_MS);
  const random = opts.random ?? Math.random;
  const fetchImpl = opts.fetch ?? (typeof fetch === "function" ? fetch : null);
  const pageEvents = opts.pageEvents === undefined ? defaultPageEvents() : opts.pageEvents;
  const urls = uploadUrls(opts.apiUrl, opts.domainKey);
  const file = opts.file;
  const total = file.size;

  /** Aborted when the upload settles: cancels every request and backoff wait. */
  const lifetime = new AbortController();
  const signal = lifetime.signal;
  let settled = false;
  /** The session the server holds chunks for, once the create call answered. */
  let sessionId: string | null = null;
  /** The first complete went out: the job may exist from now on, even if no answer comes. */
  let completeSent = false;

  let resolvePromise!: (value: CreateJobResponse) => void;
  let rejectPromise!: (reason: UploadError) => void;
  const promise = new Promise<CreateJobResponse>((resolve, reject) => {
    resolvePromise = resolve;
    rejectPromise = reject;
  });

  const settle = (fn: () => void) => {
    if (settled) return;
    settled = true;
    opts.signal?.removeEventListener("abort", abort);
    pageEvents?.removeEventListener("pagehide", leavePage);
    pageEvents?.removeEventListener("beforeunload", leavePage);
    lifetime.abort();
    fn();
  };

  /**
   * Best-effort DELETE of the session. keepalive lets it outlive the page (pagehide, or a
   * tab closed right after Cancel); sendBeacon cannot send DELETE. Nobody reads the answer.
   */
  const discardSession = (uploadId: string) => {
    const url = urls.session(uploadId);
    if (fetchImpl) {
      try {
        const init: RequestInit = { method: "DELETE", keepalive: true, headers: { Accept: "application/json" } };
        void Promise.resolve(fetchImpl.call(globalThis, url, init)).catch(() => {});
        return;
      } catch {
        // fetch refused the request outright: fall back to XHR.
      }
    }
    void send(createXhr, { method: "DELETE", url, timeoutMs: DISCARD_TIMEOUT_MS, signal: new AbortController().signal });
  };

  /** Whether a failed upload's session is DELETEd (see the header comment). */
  const shouldDiscard = (f: UploadFailure): boolean => {
    if (f.kind === "http" && f.code === "UPLOAD_NOT_FOUND") return false; // already gone
    if (!completeSent) return true;
    // Only a definitive answer (a rejection, 409 after the resend rounds, 429 for too long)
    // says the server holds nothing worth keeping. Not after a lost answer or a 5xx.
    return f.kind === "http" && f.status >= 400 && f.status < 500;
  };

  const fail = (failure: UploadFailure) => {
    if (settled) return;
    settle(() => rejectPromise(new UploadError(failure)));
    if (sessionId && shouldDiscard(failure)) discardSession(sessionId);
  };

  function abort() {
    fail({ kind: "aborted" });
  }

  /**
   * pagehide (beforeunload as a fallback): the page is going away, so free the session's
   * upload slot at once instead of leaving it to the server's idle expiry. Once complete
   * was sent the server finishes the job on its own.
   */
  function leavePage() {
    if (!completeSent) abort();
  }

  // --- progress: stored chunks + what the in-flight ones have sent so far ---
  const inFlight = new Map<number, number>();
  let storedBytes = 0;
  let storedChunks = 0;
  let chunkCount = 0;
  let waitingRetries = 0;
  let completing = false;
  let reported: UploadProgress | null = null;

  const report = () => {
    if (settled || !opts.onProgress) return;
    let sent = storedBytes;
    for (const bytes of inFlight.values()) sent += bytes;
    const loaded = Math.min(total, Math.max(reported?.loaded ?? 0, sent));
    const allStored = chunkCount > 0 && storedChunks === chunkCount;
    const percent = total > 0 ? (allStored ? 100 : Math.min(99, Math.floor((loaded / total) * 100))) : null;
    const retrying = waitingRetries > 0;
    if (
      reported &&
      reported.loaded === loaded &&
      reported.percent === percent &&
      reported.retrying === retrying &&
      reported.completing === completing
    ) {
      return;
    }
    reported = { loaded, total, percent, retrying, completing };
    opts.onProgress(reported);
  };

  /** Runs `attempt` until it succeeds, retrying transient failures with backoff until `stop` fires. */
  async function withRetries<T>(
    attempt: () => Promise<{ value: T } | { failure: UploadFailure }>,
    stop: AbortSignal,
  ): Promise<T> {
    for (let retry = 0; ; retry++) {
      const result = await attempt();
      if ("value" in result) return result.value;
      const f = result.failure;
      if (f.kind === "aborted" || retry >= maxRetries || !isTransient(f)) throw new UploadError(f);
      let delay = backoffMs(retry, random);
      if (f.kind === "http" && f.retryAfterSec) {
        const asked = f.retryAfterSec * 1000;
        if (asked > MAX_RETRY_AFTER_MS) throw new UploadError(f);
        delay = Math.max(delay, asked);
      }
      waitingRetries++;
      report();
      try {
        await sleep(delay, stop);
      } finally {
        waitingRetries--;
      }
      report();
    }
  }

  // --- session probe: a complete after chunk network failures (see PROBE_AFTER_NETWORK_FAILURES) ---
  /** The probe in flight, shared by the chunk workers. */
  let probe: Promise<ProbeVerdict> | null = null;
  /** The job a probe created: every chunk had been stored, only the answers were lost. */
  let probedJob: CompleteUploadResponse | null = null;

  function probeSession(uploadId: string): Promise<ProbeVerdict> {
    probe ??= send(createXhr, { method: "POST", url: urls.complete(uploadId), timeoutMs: completeTimeoutMs, signal })
      .then((outcome) => {
        const verdict = probeVerdict(outcome);
        if (verdict.kind === "job") probedJob = verdict.job;
        return verdict;
      })
      .finally(() => {
        probe = null;
      });
    return probe;
  }

  /** Sends chunk `index` until it is stored. `recount` = false: it was counted as stored already. */
  async function sendChunk(plan: ChunkPlan, index: number, stop: AbortSignal, recount: boolean): Promise<void> {
    const [start, end] = chunkRange(plan, index, total);
    const body = file.slice(start, end);
    const size = end - start;
    let noAnswers = 0;
    await withRetries<void>(async () => {
      // A probe that finds every chunk stored starts the ingest, which refuses chunk PUTs.
      while (probe) await probe;
      if (recount) inFlight.set(index, 0);
      const outcome = await send(createXhr, {
        method: "PUT",
        url: urls.chunk(plan.uploadId, index),
        body,
        contentType: "application/octet-stream",
        timeoutMs,
        stallTimeoutMs,
        signal: stop,
        onUploadProgress: recount
          ? (loaded) => {
              inFlight.set(index, Math.min(Math.max(0, loaded), size));
              report();
            }
          : undefined,
      });
      inFlight.delete(index);
      if (isSuccess(outcome)) {
        if (recount) {
          storedBytes += size;
          storedChunks++;
          report();
        }
        return { value: undefined };
      }
      const failure = failureOf(outcome);
      noAnswers = isNoAnswer(failure) ? noAnswers + 1 : 0;
      // A server that refuses a chunk early (e.g. its session is gone) may reset the
      // connection before the browser reads the 404: ask the session directly.
      if (noAnswers >= PROBE_AFTER_NETWORK_FAILURES && !stop.aborted) {
        const verdict = await probeSession(plan.uploadId);
        if (verdict.kind === "job") throw new JobCreatedByProbe();
        if (verdict.kind === "final") return { failure: verdict.failure };
      }
      return { failure };
    }, stop);
  }

  /**
   * sendChunks, but a probe that created the job meanwhile wins: then its job is returned
   * (null: every chunk was stored). A chunk PUT refused while a probe runs the ingest
   * (400 "being completed", 404) waits for the probe's answer first.
   */
  async function sendChunksUnlessProbed(
    plan: ChunkPlan,
    indexes: readonly number[],
    recount: boolean,
  ): Promise<CompleteUploadResponse | null> {
    try {
      await sendChunks(plan, indexes, recount);
      return null;
    } catch (err) {
      while (probe) await probe;
      if (probedJob) return probedJob;
      throw err;
    }
  }

  /** Sends the given chunks, `concurrency` at a time. The first final failure stops the others. */
  async function sendChunks(plan: ChunkPlan, indexes: readonly number[], recount: boolean): Promise<void> {
    if (signal.aborted) throw new UploadError({ kind: "aborted" });
    const stopper = new AbortController();
    const stopAll = () => stopper.abort();
    signal.addEventListener("abort", stopAll, { once: true });
    let next = 0;
    const worker = async () => {
      try {
        while (next < indexes.length && !stopper.signal.aborted) {
          await sendChunk(plan, indexes[next++], stopper.signal, recount);
        }
      } catch (err) {
        stopper.abort(); // stop the other chunks right away
        throw err;
      }
    };
    try {
      await Promise.all(Array.from({ length: Math.min(concurrency, indexes.length) }, worker));
    } finally {
      signal.removeEventListener("abort", stopAll);
    }
  }

  /**
   * Complete, retried on transient failures (the server replays a repeated complete). A
   * 409 UPLOAD_INCOMPLETE re-sends the chunks it lists, at most MAX_RESEND_ROUNDS times.
   */
  async function complete(plan: ChunkPlan): Promise<CompleteUploadResponse> {
    completeSent = true;
    completing = true;
    report();
    for (let round = 0; ; round++) {
      const conflict: { missing: number[] | null } = { missing: null };
      try {
        return await withRetries<CompleteUploadResponse>(async () => {
          const outcome = await send(createXhr, {
            method: "POST",
            url: urls.complete(plan.uploadId),
            timeoutMs: completeTimeoutMs,
            signal,
          });
          if (outcome.kind !== "answer") return { failure: outcome };
          if (outcome.status >= 200 && outcome.status < 300) {
            if (isJobResponse(outcome.body)) return { value: outcome.body };
            return { failure: { kind: "http", status: outcome.status, code: null } };
          }
          conflict.missing = missingChunks(outcome.body, plan.count);
          return { failure: failureOf(outcome) };
        }, signal);
      } catch (err) {
        const missing = conflict.missing;
        if (!missing || round >= MAX_RESEND_ROUNDS) throw err;
        // The server does not count these as stored (an older PUT of one may still be
        // open there, or its answer got lost): give it a moment, then send them again.
        await sleep(backoffMs(round, random), signal);
        // A probe while re-sending (network failures) may find every chunk stored and
        // create the job: that job is the answer.
        const job = await sendChunksUnlessProbed(plan, missing, false);
        if (job) return job;
      }
    }
  }

  async function run(): Promise<CreateJobResponse> {
    const fileName = clampFileName(
      opts.fileName ?? (typeof File !== "undefined" && file instanceof File ? file.name : "video"),
    );
    const request: CreateUploadRequest = { fileName, sizeBytes: total, ...(file.type ? { contentType: file.type } : {}) };
    // Cancel does not abort the create call: once the server made a session, only its
    // answer names the session to DELETE.
    const created = await send(createXhr, {
      method: "POST",
      url: urls.create,
      body: JSON.stringify(request),
      contentType: "application/json",
      timeoutMs,
      signal: new AbortController().signal,
    });
    const session = isSuccess(created) ? (created.body as CreateUploadResponse | null) : null;
    if (typeof session?.uploadId === "string" && session.uploadId) {
      sessionId = session.uploadId;
      if (settled) discardSession(sessionId); // cancelled, or the page was left, while create ran
    }
    if (settled) throw new UploadError({ kind: "aborted" });
    if (!isSuccess(created)) throw new UploadError(failureOf(created));
    const plan = chunkPlan(session, total);
    if (!plan) throw new UploadError({ kind: "http", status: created.status, code: null });
    chunkCount = plan.count;
    report();

    const job = await sendChunksUnlessProbed(
      plan,
      Array.from({ length: plan.count }, (_, i) => i),
      true,
    );
    return job ?? complete(plan);
  }

  if (opts.signal?.aborted) {
    fail({ kind: "aborted" });
    return { promise, abort };
  }
  opts.signal?.addEventListener("abort", abort, { once: true });
  pageEvents?.addEventListener("pagehide", leavePage);
  pageEvents?.addEventListener("beforeunload", leavePage);

  run().then(
    (job) => settle(() => resolvePromise(job)),
    (err: unknown) => fail(err instanceof UploadError ? err.failure : { kind: "network" }),
  );

  return { promise, abort };
}
