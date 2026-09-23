import { describe, expect, it, vi } from "vitest";

import { API_BASE, JOB_ID, makeJob } from "@/test/fixtures";

import {
  ApiError,
  createApi,
  ERROR_CODES,
  errorCodeFromBody,
  errorCodeFromStatus,
  isErrorCode,
  isTerminalStatus,
  isTransientError,
  parseRetryAfter,
  resolveApiUrl,
} from "./api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function recordingFetch(respond: (req: Request) => Response | Promise<Response>) {
  const requests: Request[] = [];
  const fn = vi.fn(async (input: Request) => {
    requests.push(input);
    return respond(input);
  });
  return { fn: fn as unknown as typeof fetch, requests };
}

describe("parseRetryAfter", () => {
  const now = Date.parse("2026-09-23T10:00:00Z");
  it.each([
    ["120", 120],
    [" 7 ", 7],
    ["Wed, 23 Sep 2026 10:05:00 GMT", 300],
    ["Wed, 23 Sep 2026 09:59:00 GMT", null], // in the past
    ["0", null],
    ["-5", null],
    ["1.5", null],
    ["soon", null],
    ["10 minutes", null],
    ["", null],
    [null, null],
  ])("%j → %s", (value, want) => {
    expect(parseRetryAfter(value, now)).toBe(want);
  });
});

describe("resolveApiUrl", () => {
  it.each([
    ["http://localhost:4000", "/v1/domains/cars/jobs/j/download.zip", "http://localhost:4000/v1/domains/cars/jobs/j/download.zip"],
    ["http://localhost:4000/", "/v1/x", "http://localhost:4000/v1/x"],
    ["https://api.example.com/base", "/v1/x", "https://api.example.com/base/v1/x"],
    ["https://api.example.com/base/", "v1/x", "https://api.example.com/base/v1/x"],
    ["http://localhost:4000", "https://minio.example.com/v2i/x.jpg?X-Amz-Signature=1", "https://minio.example.com/v2i/x.jpg?X-Amz-Signature=1"],
    ["http://localhost:4000", "//cdn.example.com/x.jpg", "//cdn.example.com/x.jpg"],
    ["", "/v1/x", "/v1/x"],
  ])("resolves %s + %s", (base, path, want) => {
    expect(resolveApiUrl(base, path)).toBe(want);
  });
});

describe("error codes", () => {
  it("lists every code once", () => {
    expect(new Set(ERROR_CODES).size).toBe(ERROR_CODES.length);
    for (const code of ERROR_CODES) expect(isErrorCode(code)).toBe(true);
    expect(isErrorCode("NOT_A_CODE")).toBe(false);
    expect(isErrorCode(42)).toBe(false);
  });

  it.each([
    [{ error: { code: "TOO_LONG", message: "x" } }, "TOO_LONG"],
    [{ error: { code: "SOMETHING_NEW", message: "x" } }, null],
    [{ error: "nope" }, null],
    [{ message: "x" }, null],
    ["<html>", null],
    [null, null],
  ])("errorCodeFromBody(%j) = %s", (body, want) => {
    expect(errorCodeFromBody(body)).toBe(want);
  });

  it.each([
    [413, "UPLOAD_TOO_LARGE"],
    [429, "RATE_LIMITED"],
    [403, "ORIGIN_NOT_ALLOWED"],
    [410, "EXPIRED"],
    [502, "INTERNAL_ERROR"],
    [404, null],
    [400, null],
  ])("errorCodeFromStatus(%i) = %s", (status, want) => {
    expect(errorCodeFromStatus(status)).toBe(want);
  });
});

describe("isTransientError / isTerminalStatus", () => {
  it.each([
    [ApiError.network(new TypeError("Failed to fetch")), true],
    [new ApiError("http", 429, "RATE_LIMITED"), true],
    [new ApiError("http", 500, "INTERNAL_ERROR"), true],
    [new ApiError("http", 503, null), true],
    [new ApiError("http", 404, "JOB_NOT_FOUND"), false],
    [new ApiError("http", 410, "EXPIRED"), false],
    [new ApiError("http", 403, "ORIGIN_NOT_ALLOWED"), false],
    [new TypeError("boom"), true],
  ])("%s → %s", (err, want) => {
    expect(isTransientError(err)).toBe(want);
  });

  it("knows the terminal statuses", () => {
    expect(isTerminalStatus("queued")).toBe(false);
    expect(isTerminalStatus("processing")).toBe(false);
    for (const s of ["completed", "rejected", "failed", "expired"] as const) expect(isTerminalStatus(s)).toBe(true);
  });
});

describe("createApi", () => {
  it("getJob requests the job URL and returns the body", async () => {
    const job = makeJob();
    const f = recordingFetch(() => jsonResponse(200, job));
    const api = createApi(API_BASE, f.fn);

    await expect(api.getJob("cars", JOB_ID)).resolves.toEqual(job);
    expect(f.requests).toHaveLength(1);
    expect(f.requests[0].method).toBe("GET");
    expect(f.requests[0].url).toBe(`${API_BASE}/v1/domains/cars/jobs/${JOB_ID}`);
    expect(f.requests[0].cache).toBe("no-store");
  });

  it("getJob maps the error envelope to ApiError", async () => {
    const api = createApi(API_BASE, recordingFetch(() => jsonResponse(404, { error: { code: "JOB_NOT_FOUND", message: "no" } })).fn);
    const err = await api.getJob("cars", JOB_ID).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ kind: "http", status: 404, code: "JOB_NOT_FOUND" });
  });

  it("getJob maps 410 EXPIRED", async () => {
    const api = createApi(API_BASE, recordingFetch(() => jsonResponse(410, { error: { code: "EXPIRED", message: "gone" } })).fn);
    await expect(api.getJob("cars", JOB_ID)).rejects.toMatchObject({ status: 410, code: "EXPIRED" });
  });

  it("getJob falls back to the status when the body is not an envelope", async () => {
    const api = createApi(API_BASE, recordingFetch(() => new Response("<html>Bad gateway</html>", { status: 502 })).fn);
    await expect(api.getJob("cars", JOB_ID)).rejects.toMatchObject({ kind: "http", status: 502, code: "INTERNAL_ERROR" });
  });

  it("getJob turns a rejected fetch into a network ApiError", async () => {
    const api = createApi(API_BASE, (async () => {
      throw new TypeError("Failed to fetch");
    }) as unknown as typeof fetch);
    await expect(api.getJob("cars", JOB_ID)).rejects.toMatchObject({ kind: "network", status: 0, code: null });
  });

  it("encodes path parameters", async () => {
    const f = recordingFetch(() => jsonResponse(200, makeJob()));
    await createApi(API_BASE, f.fn).getJob("cars", "job_a/../b");
    expect(f.requests[0].url).toBe(`${API_BASE}/v1/domains/cars/jobs/job_a%2F..%2Fb`);
  });

  it("deleteJob sends DELETE and accepts 204", async () => {
    const f = recordingFetch(() => new Response(null, { status: 204 }));
    await expect(createApi(API_BASE, f.fn).deleteJob("cars", JOB_ID)).resolves.toBeUndefined();
    expect(f.requests[0].method).toBe("DELETE");
    expect(f.requests[0].url).toBe(`${API_BASE}/v1/domains/cars/jobs/${JOB_ID}`);
  });

  it("deleteJob throws ApiError on failure", async () => {
    const f = recordingFetch(() => jsonResponse(404, { error: { code: "JOB_NOT_FOUND", message: "no" } }));
    await expect(createApi(API_BASE, f.fn).deleteJob("cars", JOB_ID)).rejects.toMatchObject({ status: 404, code: "JOB_NOT_FOUND" });
  });

  it("resolveApiUrl uses the client's base URL", () => {
    expect(createApi(`${API_BASE}/`).resolveApiUrl("/v1/x")).toBe(`${API_BASE}/v1/x`);
  });
});
