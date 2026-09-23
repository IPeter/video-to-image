import {
  errorCodeFromBody,
  errorCodeFromStatus,
  parseRetryAfter,
  type CreateJobResponse,
  type ErrorCode,
} from "./api";

// Video upload with XMLHttpRequest, because fetch() still cannot report upload progress
// in every browser. POSTs multipart field "video" to {apiUrl}/v1/domains/{key}/jobs.
//
// Note: registering an upload progress listener makes the browser send a CORS preflight
// (OPTIONS) even for a multipart POST, so the API must answer preflights for this route.

export interface UploadProgress {
  loaded: number;
  /** 0 when the browser cannot compute the total. */
  total: number;
  /** 0–100, or null when the total is unknown. */
  percent: number | null;
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
  /** File name sent in the multipart part (defaults to File.name, else "video"). */
  fileName?: string;
  onProgress?: (progress: UploadProgress) => void;
  /** Aborting the signal cancels the upload, like UploadHandle.abort(). */
  signal?: AbortSignal;
  /** Milliseconds; 0 (default) means no timeout, since large uploads on slow links are normal. */
  timeoutMs?: number;
  /** For tests. */
  createXhr?: () => XMLHttpRequest;
}

export interface UploadHandle {
  /** Resolves with the 202 body; rejects with UploadError. */
  promise: Promise<CreateJobResponse>;
  /** Cancels the upload; the promise rejects with kind "aborted". No-op once settled. */
  abort(): void;
}

export function uploadUrl(apiUrl: string, domainKey: string): string {
  return `${apiUrl.replace(/\/+$/, "")}/v1/domains/${encodeURIComponent(domainKey)}/jobs`;
}

function parseJson(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function isCreateJobResponse(v: unknown): v is CreateJobResponse {
  if (typeof v !== "object" || v === null) return false;
  const jobId = (v as { jobId?: unknown }).jobId;
  return typeof jobId === "string" && jobId.length > 0;
}

/** A response header, or null when it is absent or not exposed by CORS. */
function responseHeader(xhr: XMLHttpRequest, name: string): string | null {
  try {
    return xhr.getResponseHeader(name);
  } catch {
    return null;
  }
}

/** Starts the upload immediately. */
export function uploadVideo(opts: UploadOptions): UploadHandle {
  const xhr = (opts.createXhr ?? (() => new XMLHttpRequest()))();
  let settled = false;
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
    xhr.upload.onprogress = null;
    fn();
  };
  const fail = (failure: UploadFailure) => settle(() => rejectPromise(new UploadError(failure)));

  function abort() {
    if (settled) return;
    // Settle first so the onabort handler (which may fire synchronously) is a no-op.
    fail({ kind: "aborted" });
    try {
      xhr.abort();
    } catch {
      // Nothing to cancel.
    }
  }

  if (opts.signal?.aborted) {
    fail({ kind: "aborted" });
    return { promise, abort };
  }
  opts.signal?.addEventListener("abort", abort, { once: true });

  xhr.upload.onprogress = (e: ProgressEvent) => {
    if (settled || !opts.onProgress) return;
    const total = e.lengthComputable ? e.total : 0;
    opts.onProgress({
      loaded: e.loaded,
      total,
      percent: total > 0 ? Math.min(100, Math.round((e.loaded / total) * 100)) : null,
    });
  };

  xhr.onload = () => {
    const body = parseJson(typeof xhr.responseText === "string" ? xhr.responseText : "");
    if (xhr.status >= 200 && xhr.status < 300) {
      if (isCreateJobResponse(body)) {
        settle(() => resolvePromise(body));
      } else {
        fail({ kind: "http", status: xhr.status, code: null });
      }
      return;
    }
    if (xhr.status === 0) {
      fail({ kind: "network" });
      return;
    }
    const code = errorCodeFromBody(body) ?? errorCodeFromStatus(xhr.status);
    const retryAfterSec = parseRetryAfter(responseHeader(xhr, "Retry-After"));
    fail({ kind: "http", status: xhr.status, code, ...(retryAfterSec !== null && { retryAfterSec }) });
  };
  xhr.onerror = () => fail({ kind: "network" });
  xhr.ontimeout = () => fail({ kind: "timeout" });
  // Only reached when the browser aborts on its own (e.g. navigation); ours settle first.
  xhr.onabort = () => fail({ kind: "network" });

  try {
    const form = new FormData();
    const name = opts.fileName ?? (typeof File !== "undefined" && opts.file instanceof File ? opts.file.name : "video");
    form.append("video", opts.file, name);

    xhr.open("POST", uploadUrl(opts.apiUrl, opts.domainKey));
    xhr.responseType = "text";
    xhr.timeout = opts.timeoutMs ?? 0;
    xhr.setRequestHeader("Accept", "application/json");
    xhr.send(form);
  } catch {
    fail({ kind: "network" });
  }

  return { promise, abort };
}
