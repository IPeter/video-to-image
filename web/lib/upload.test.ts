import { describe, expect, it, vi } from "vitest";

import { FakeXhr } from "@/test/fake-xhr";

import { UploadError, uploadUrl, uploadVideo, type UploadFailure } from "./upload";

function start(overrides: Partial<Parameters<typeof uploadVideo>[0]> = {}) {
  const onProgress = vi.fn();
  const file = new File([new Uint8Array(1024)], "walkaround.mp4", { type: "video/mp4" });
  const handle = uploadVideo({
    apiUrl: "http://localhost:4000/",
    domainKey: "cars",
    file,
    onProgress,
    createXhr: () => new FakeXhr() as unknown as XMLHttpRequest,
    ...overrides,
  });
  const xhr = FakeXhr.last!;
  return { handle, xhr, onProgress, file };
}

async function failureOf(promise: Promise<unknown>): Promise<UploadFailure> {
  const err = await promise.then(
    () => {
      throw new Error("expected the upload to fail");
    },
    (e: unknown) => e,
  );
  expect(err).toBeInstanceOf(UploadError);
  return (err as UploadError).failure;
}

describe("uploadUrl", () => {
  it("joins base URL and domain", () => {
    expect(uploadUrl("http://localhost:4000/", "cars")).toBe("http://localhost:4000/v1/domains/cars/jobs");
    expect(uploadUrl("https://api.example.com/base", "homes")).toBe("https://api.example.com/base/v1/domains/homes/jobs");
  });
});

describe("uploadVideo", () => {
  it("POSTs the file as multipart field 'video'", () => {
    const { xhr, file } = start();
    expect(xhr.method).toBe("POST");
    expect(xhr.url).toBe("http://localhost:4000/v1/domains/cars/jobs");
    expect(xhr.headers.Accept).toBe("application/json");
    const sent = xhr.body?.get("video");
    expect(sent).toBeInstanceOf(File);
    expect((sent as File).name).toBe(file.name);
    expect((sent as File).size).toBe(file.size);
  });

  it("reports progress", () => {
    const { xhr, onProgress } = start();
    xhr.progress(256, 1024);
    xhr.progress(1024, 1024);
    xhr.progress(100, 0, false);
    expect(onProgress.mock.calls.map((c) => c[0])).toEqual([
      { loaded: 256, total: 1024, percent: 25 },
      { loaded: 1024, total: 1024, percent: 100 },
      { loaded: 100, total: 0, percent: null },
    ]);
  });

  it("resolves with the 202 body", async () => {
    const { xhr, handle } = start();
    xhr.respond(202, { jobId: "job_01J8ZK3M2Q7K2M9QXA4B5C6D7E", status: "queued" });
    await expect(handle.promise).resolves.toEqual({ jobId: "job_01J8ZK3M2Q7K2M9QXA4B5C6D7E", status: "queued" });
  });

  it.each([
    [400, { error: { code: "TOO_LONG", message: "Video is longer than 180 s" } }, "TOO_LONG"],
    [400, { error: { code: "RESOLUTION_TOO_LOW", message: "x" } }, "RESOLUTION_TOO_LOW"],
    [413, { error: { code: "UPLOAD_TOO_LARGE", message: "x" } }, "UPLOAD_TOO_LARGE"],
    [429, { error: { code: "RATE_LIMITED", message: "x" } }, "RATE_LIMITED"],
    [403, { error: { code: "ORIGIN_NOT_ALLOWED", message: "x" } }, "ORIGIN_NOT_ALLOWED"],
    [413, "<html><body>413 Request Entity Too Large</body></html>", "UPLOAD_TOO_LARGE"],
    [400, { error: { code: "BRAND_NEW_CODE", message: "x" } }, null],
    [500, "oops", "INTERNAL_ERROR"],
  ])("maps HTTP %i %j to code %s", async (status, body, code) => {
    const { xhr, handle } = start();
    xhr.respond(status, body);
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status, code });
  });

  it("passes a Retry-After on 429 through as seconds", async () => {
    const { xhr, handle } = start();
    xhr.respond(429, { error: { code: "RATE_LIMITED", message: "x" } }, { "Retry-After": "360" });
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 429, code: "RATE_LIMITED", retryAfterSec: 360 });
  });

  it("passes a Retry-After on 503 SERVER_BUSY through as seconds", async () => {
    const { xhr, handle } = start();
    xhr.respond(503, { error: { code: "SERVER_BUSY", message: "x" } }, { "retry-after": "30" });
    expect(await failureOf(handle.promise)).toStrictEqual({ kind: "http", status: 503, code: "SERVER_BUSY", retryAfterSec: 30 });
  });

  it("omits retryAfterSec when the header is missing or malformed", async () => {
    const cases: Record<string, string>[] = [{}, { "Retry-After": "soon" }];
    for (const headers of cases) {
      const { xhr, handle } = start();
      xhr.respond(429, { error: { code: "RATE_LIMITED", message: "x" } }, headers);
      expect(await failureOf(handle.promise)).toStrictEqual({ kind: "http", status: 429, code: "RATE_LIMITED" });
    }
  });

  it("rejects a 2xx without a job id", async () => {
    const { xhr, handle } = start();
    xhr.respond(202, "not json");
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 202, code: null });
  });

  it("treats a network error distinctly", async () => {
    const { xhr, handle } = start();
    xhr.networkError();
    expect(await failureOf(handle.promise)).toEqual({ kind: "network" });
  });

  it("treats a timeout distinctly", async () => {
    const { xhr, handle } = start({ timeoutMs: 5000 });
    expect(xhr.timeout).toBe(5000);
    xhr.ontimeout?.();
    expect(await failureOf(handle.promise)).toEqual({ kind: "timeout" });
  });

  it("abort() cancels the request and rejects with kind aborted", async () => {
    const { xhr, handle, onProgress } = start();
    handle.abort();
    expect(xhr.aborted).toBe(true);
    expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
    // Late events after the abort are ignored.
    xhr.progress(10, 100);
    xhr.respond(202, { jobId: "job_x", status: "queued" });
    expect(onProgress).not.toHaveBeenCalled();
  });

  it("an AbortSignal cancels the upload", async () => {
    const controller = new AbortController();
    const { xhr, handle } = start({ signal: controller.signal });
    controller.abort();
    expect(xhr.aborted).toBe(true);
    expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
  });

  it("does not send when the signal is already aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    const { xhr, handle } = start({ signal: controller.signal });
    expect(xhr.body).toBeNull();
    expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
  });

  it("a browser-initiated abort counts as a network failure", async () => {
    const { xhr, handle } = start();
    xhr.onabort?.();
    expect(await failureOf(handle.promise)).toEqual({ kind: "network" });
  });

  it("abort() after completion is a no-op", async () => {
    const { xhr, handle } = start();
    xhr.respond(202, { jobId: "job_1", status: "queued" });
    handle.abort();
    await expect(handle.promise).resolves.toMatchObject({ jobId: "job_1" });
    expect(xhr.aborted).toBe(false);
  });
});
