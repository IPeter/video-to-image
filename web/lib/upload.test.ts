import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";

import { FakeXhr } from "@/test/fake-xhr";

import {
  backoffMs,
  chunkPlan,
  chunkRange,
  clampFileName,
  DEFAULT_COMPLETE_TIMEOUT_MS,
  MAX_FILE_NAME_LENGTH,
  missingChunks,
  UploadError,
  uploadUrls,
  uploadVideo,
  type UploadFailure,
  type UploadOptions,
  type UploadProgress,
} from "./upload";

const API = "http://localhost:4000/";
const BASE = "http://localhost:4000/v1/domains/cars/uploads";
const UPLOAD_ID = "upl_01J8ZK3M2Q7K2M9QXA4B5C6D7E";
const JOB = { jobId: "job_01J8ZK3M2Q7K2M9QXA4B5C6D7E", status: "queued" };
const CHUNK_URL = /\/chunks\/\d+$/;

/** A file whose byte i is i % 251, so every slice is recognisable. */
function videoFile(size: number, name = "walkaround.mp4", type = "video/mp4"): File {
  const bytes = new Uint8Array(size);
  for (let i = 0; i < size; i++) bytes[i] = i % 251;
  return new File([bytes], name, { type });
}

function sessionFor(chunkSizeBytes: number, size: number) {
  return {
    uploadId: UPLOAD_ID,
    chunkSizeBytes,
    chunkCount: Math.max(1, Math.ceil(size / chunkSizeBytes)),
    expiresAt: "2026-09-23T12:00:00Z",
  };
}

/** Runs every pending promise continuation (the uploader awaits between requests). */
const flush = () => vi.advanceTimersByTimeAsync(0);
const advance = (ms: number) => vi.advanceTimersByTimeAsync(ms);

/** The session DELETE goes through fetch (keepalive); every test gets a fresh mock. */
let fetchMock: Mock<typeof fetch>;
/** Stands in for window's pagehide / beforeunload in most tests. */
let page: EventTarget;

function start(size = 30, overrides: Partial<UploadOptions> = {}) {
  const onProgress = vi.fn<(p: UploadProgress) => void>();
  const file = videoFile(size);
  const handle = uploadVideo({
    apiUrl: API,
    domainKey: "cars",
    file,
    onProgress,
    createXhr: () => new FakeXhr() as unknown as XMLHttpRequest,
    fetch: fetchMock,
    pageEvents: page,
    random: () => 0,
    ...overrides,
  });
  handle.promise.catch(() => {}); // asserted through failureOf where it matters
  return { handle, onProgress, file };
}

function createRequest(): FakeXhr {
  const [create] = FakeXhr.requests("POST", BASE).filter((x) => x.url === BASE);
  expect(create).toBeDefined();
  return create;
}

/** Starts an upload and answers the create call with a session of `chunkSize` pieces. */
async function started(size: number, chunkSize: number, overrides: Partial<UploadOptions> = {}) {
  const run = start(size, overrides);
  await flush();
  createRequest().respond(201, sessionFor(chunkSize, size));
  await flush();
  return run;
}

function chunkIndex(xhr: FakeXhr): number {
  return Number(xhr.url.slice(xhr.url.lastIndexOf("/") + 1));
}

const pendingChunks = () => FakeXhr.pending("PUT", CHUNK_URL);
const pendingIndexes = () => pendingChunks().map(chunkIndex);
const completeCalls = () => FakeXhr.requests("POST", `${BASE}/${UPLOAD_ID}/complete`);
const pendingCompletes = () => FakeXhr.pending("POST", `${BASE}/${UPLOAD_ID}/complete`);
/** The session DELETEs sent (fetch init objects). */
const deleteCalls = () =>
  fetchMock.mock.calls
    .filter(([url, init]) => String(url) === `${BASE}/${UPLOAD_ID}` && init?.method === "DELETE")
    .map(([, init]) => init!);

const incomplete = (missing: number[], chunkCount: number) => ({
  error: {
    code: "UPLOAD_INCOMPLETE",
    message: `${missing.length} of ${chunkCount} chunks have not been received yet`,
    details: { missing, received: chunkCount - missing.length, chunkCount },
  },
});
const notFound = { error: { code: "UPLOAD_NOT_FOUND", message: "no such upload" } };

/** Answers pending chunk PUTs with 204 until none are left. */
async function storeAllChunks() {
  for (let guard = 0; pendingChunks().length > 0; guard++) {
    if (guard > 1000) throw new Error("chunks never ran out");
    pendingChunks()[0].respond(204);
    await flush();
  }
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

beforeEach(() => {
  vi.useFakeTimers();
  FakeXhr.reset();
  fetchMock = vi.fn<typeof fetch>(() => Promise.resolve({ ok: true, status: 204 } as Response));
  page = new EventTarget();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("uploadUrls", () => {
  it("builds every chunked-upload URL", () => {
    const urls = uploadUrls("http://localhost:4000/", "cars");
    expect(urls.create).toBe(BASE);
    expect(urls.session(UPLOAD_ID)).toBe(`${BASE}/${UPLOAD_ID}`);
    expect(urls.chunk(UPLOAD_ID, 3)).toBe(`${BASE}/${UPLOAD_ID}/chunks/3`);
    expect(urls.complete(UPLOAD_ID)).toBe(`${BASE}/${UPLOAD_ID}/complete`);
    expect(uploadUrls("https://api.example.com/base", "homes").create).toBe(
      "https://api.example.com/base/v1/domains/homes/uploads",
    );
    expect(uploadUrls("http://x", "a b").session("u/1")).toBe("http://x/v1/domains/a%20b/uploads/u%2F1");
  });
});

describe("chunk plan", () => {
  it("accepts a session that covers the file exactly", () => {
    expect(chunkPlan(sessionFor(10, 30), 30)).toEqual({ uploadId: UPLOAD_ID, chunkSize: 10, count: 3 });
    expect(chunkPlan(sessionFor(10, 25), 25)).toEqual({ uploadId: UPLOAD_ID, chunkSize: 10, count: 3 });
    expect(chunkPlan(sessionFor(10, 7), 7)).toEqual({ uploadId: UPLOAD_ID, chunkSize: 10, count: 1 });
  });

  it.each([
    ["a wrong chunk count", { ...sessionFor(10, 30), chunkCount: 4 }],
    ["a zero chunk size", { ...sessionFor(10, 30), chunkSizeBytes: 0 }],
    ["a fractional chunk size", { ...sessionFor(10, 30), chunkSizeBytes: 7.5 }],
    ["no upload id", { ...sessionFor(10, 30), uploadId: "" }],
    ["no object", "nope"],
  ])("rejects %s", (_, session) => {
    expect(chunkPlan(session, 30)).toBeNull();
  });

  it("slices exact multiples, a remainder and a single chunk", () => {
    const plan = { chunkSize: 10 };
    expect([0, 1, 2].map((i) => chunkRange(plan, i, 30))).toEqual([[0, 10], [10, 20], [20, 30]]);
    expect([0, 1, 2].map((i) => chunkRange(plan, i, 25))).toEqual([[0, 10], [10, 20], [20, 25]]);
    expect(chunkRange(plan, 0, 7)).toEqual([0, 7]);
  });
});

describe("missingChunks", () => {
  it("reads the missing chunks of a 409 UPLOAD_INCOMPLETE", () => {
    expect(missingChunks(incomplete([1, 3], 4), 4)).toEqual([1, 3]);
    expect(missingChunks(incomplete([2, 2], 4), 4)).toEqual([2]);
  });

  it.each([
    ["another code", { error: { code: "INVALID_REQUEST", message: "x", details: { missing: [1] } } }],
    ["no details", { error: { code: "UPLOAD_INCOMPLETE", message: "x" } }],
    ["an empty list", incomplete([], 4)],
    ["an index out of range", incomplete([4], 4)],
    ["a negative index", incomplete([-1], 4)],
    ["a fractional index", incomplete([1.5], 4)],
    ["a string index", { error: { code: "UPLOAD_INCOMPLETE", message: "x", details: { missing: ["1"] } } }],
    ["not an envelope", "<html>409</html>"],
  ])("ignores %s", (_, body) => {
    expect(missingChunks(body, 4)).toBeNull();
  });
});

describe("backoffMs", () => {
  it("doubles from one second, plus up to 25 % jitter", () => {
    expect([0, 1, 2, 3].map((r) => backoffMs(r, () => 0))).toEqual([1000, 2000, 4000, 8000]);
    expect([0, 1, 2, 3].map((r) => backoffMs(r, () => 0.5))).toEqual([1125, 2250, 4500, 9000]);
    expect(backoffMs(3, () => 0.9999)).toBeLessThanOrEqual(10_000);
  });
});

describe("clampFileName", () => {
  it("keeps normal names and falls back for empty ones", () => {
    expect(clampFileName("IMG_1972.MOV")).toBe("IMG_1972.MOV");
    expect(clampFileName("")).toBe("video");
  });

  it("shortens long names to the API limit, keeping the extension", () => {
    const long = `${"á".repeat(300)}.mov`;
    const clamped = clampFileName(long);
    expect(Array.from(clamped)).toHaveLength(MAX_FILE_NAME_LENGTH);
    expect(clamped.endsWith(".mov")).toBe(true);
    const emoji = "🚗".repeat(300); // surrogate pairs are one character each
    expect(Array.from(clampFileName(emoji))).toHaveLength(MAX_FILE_NAME_LENGTH);
  });
});

describe("uploadVideo", () => {
  it("starts a session with the file's name, size and type", async () => {
    start(30);
    await flush();
    const create = createRequest();
    expect(create.headers).toMatchObject({ "Content-Type": "application/json", Accept: "application/json" });
    expect(create.json).toEqual({ fileName: "walkaround.mp4", sizeBytes: 30, contentType: "video/mp4" });
  });

  it("leaves out an unknown content type and uses fileName when given", async () => {
    const file = new File([new Uint8Array(4)], "clip", { type: "" });
    const handle = uploadVideo({
      apiUrl: API,
      domainKey: "cars",
      file,
      fileName: "renamed.mov",
      createXhr: () => new FakeXhr() as unknown as XMLHttpRequest,
    });
    handle.promise.catch(() => {});
    await flush();
    expect(createRequest().json).toEqual({ fileName: "renamed.mov", sizeBytes: 4 });
  });

  it.each([
    ["an exact multiple", 30, 10, [10, 10, 10]],
    ["a remainder", 25, 10, [10, 10, 5]],
    ["a single chunk", 7, 10, [7]],
  ])("sends %s as raw octet-stream chunks", async (_, size, chunkSize, sizes) => {
    const { file, handle } = await started(size, chunkSize, { concurrency: 1 });
    const bodies: Blob[] = [];
    for (let i = 0; i < sizes.length; i++) {
      const [put] = pendingChunks();
      expect(put.method).toBe("PUT");
      expect(put.url).toBe(`${BASE}/${UPLOAD_ID}/chunks/${i}`);
      expect(put.headers["Content-Type"]).toBe("application/octet-stream");
      bodies.push(put.blob);
      put.respond(204);
      await flush();
    }
    expect(bodies.map((b) => b.size)).toEqual(sizes);
    const sent = new Uint8Array(await new Blob(bodies).arrayBuffer());
    expect(sent).toEqual(new Uint8Array(await file.arrayBuffer()));

    const [complete] = completeCalls();
    expect(complete.body).toBeNull();
    complete.respond(202, JOB, { Location: `/v1/domains/cars/jobs/${JOB.jobId}` });
    await expect(handle.promise).resolves.toEqual(JOB);
    expect(deleteCalls()).toHaveLength(0);
  });

  it("keeps at most 2 chunks in flight and completes only after the last one", async () => {
    await started(50, 10);
    expect(pendingIndexes()).toEqual([0, 1]);

    pendingChunks()[1].respond(204);
    await flush();
    expect(pendingIndexes()).toEqual([0, 2]);

    pendingChunks()[0].respond(204);
    await flush();
    expect(pendingIndexes()).toEqual([2, 3]);

    let maxInFlight = 0;
    while (pendingChunks().length > 0) {
      maxInFlight = Math.max(maxInFlight, pendingChunks().length);
      expect(completeCalls()).toHaveLength(0);
      pendingChunks()[0].respond(204);
      await flush();
    }
    expect(maxInFlight).toBe(2);
    expect(FakeXhr.requests("PUT")).toHaveLength(5);
    expect(completeCalls()).toHaveLength(1);
  });

  it("honours a custom concurrency", async () => {
    await started(50, 10, { concurrency: 3 });
    expect(pendingIndexes()).toEqual([0, 1, 2]);
  });

  it("retries a chunk after network errors, stalls, timeouts and 5xx with 1, 2, 4, 8 s backoff", async () => {
    const { handle, onProgress } = await started(10, 10);
    // Alternating, so no two failures without an answer come in a row (that would probe).
    const failures: ((x: FakeXhr) => void)[] = [
      (x) => x.networkError(),
      (x) => x.respond(503, { error: { code: "SERVER_BUSY", message: "x" } }),
      (x) => x.timeoutNow(),
      (x) => x.respond(502, "<html>Bad gateway</html>"),
    ];
    const delays = [1000, 2000, 4000, 8000];
    for (let i = 0; i < failures.length; i++) {
      failures[i](pendingChunks()[0]);
      await flush();
      expect(pendingChunks()).toHaveLength(0);
      expect(onProgress.mock.lastCall?.[0].retrying).toBe(true);
      await advance(delays[i] - 1);
      expect(pendingChunks()).toHaveLength(0);
      await advance(1);
      expect(pendingChunks()).toHaveLength(1);
      expect(onProgress.mock.lastCall?.[0].retrying).toBe(false);
    }
    expect(FakeXhr.requests("PUT")).toHaveLength(5);
    expect(new Set(FakeXhr.requests("PUT").map(chunkIndex))).toEqual(new Set([0]));
    expect(completeCalls()).toHaveLength(0);

    pendingChunks()[0].respond(204);
    await flush();
    completeCalls()[0].respond(202, JOB);
    await expect(handle.promise).resolves.toEqual(JOB);
  });

  it("adds jitter to the backoff", async () => {
    await started(10, 10, { random: () => 0.5 });
    pendingChunks()[0].networkError();
    await flush();
    await advance(1124);
    expect(pendingChunks()).toHaveLength(0);
    await advance(1);
    expect(pendingChunks()).toHaveLength(1);
  });

  it("abandons a stalled chunk and sends it again", async () => {
    await started(10, 10, { stallTimeoutMs: 5000 });
    const first = pendingChunks()[0];
    await advance(4000);
    first.progress(4, 10); // progress re-arms the stall timer
    await advance(4999);
    expect(first.aborted).toBe(false);
    await advance(1);
    expect(first.aborted).toBe(true);
    await advance(1000);
    expect(pendingChunks()).toHaveLength(1);
    expect(pendingChunks()[0]).not.toBe(first);
  });

  it("gives up after 4 retries, stops the other chunks and discards the session", async () => {
    const { handle } = await started(20, 10);
    const other = pendingChunks()[1];
    for (let attempt = 0; attempt < 5; attempt++) {
      const chunk0 = pendingChunks().find((x) => chunkIndex(x) === 0)!;
      chunk0.respond(503, "<html>Service unavailable</html>");
      await advance(10_000);
    }
    expect(FakeXhr.requests("PUT").filter((x) => chunkIndex(x) === 0)).toHaveLength(5);
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 503, code: "INTERNAL_ERROR" });
    expect(other.aborted).toBe(true);
    expect(pendingChunks()).toHaveLength(0);
    expect(deleteCalls()).toEqual([{ method: "DELETE", keepalive: true, headers: { Accept: "application/json" } }]);
    expect(completeCalls()).toHaveLength(0);
  });

  it("waits for Retry-After on 429 before sending the chunk again", async () => {
    const { handle } = await started(10, 10);
    pendingChunks()[0].respond(429, { error: { code: "RATE_LIMITED", message: "x" } }, { "Retry-After": "5" });
    await flush();
    await advance(4999); // the 1 s backoff alone would have retried long ago
    expect(pendingChunks()).toHaveLength(0);
    await advance(1);
    expect(pendingChunks()).toHaveLength(1);
    pendingChunks()[0].respond(204);
    await flush();
    completeCalls()[0].respond(202, JOB);
    await expect(handle.promise).resolves.toEqual(JOB);
  });

  it("gives up at once when Retry-After asks for more than a minute", async () => {
    const { handle } = await started(10, 10);
    pendingChunks()[0].respond(429, { error: { code: "RATE_LIMITED", message: "x" } }, { "Retry-After": "360" });
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 429, code: "RATE_LIMITED", retryAfterSec: 360 });
    expect(FakeXhr.requests("PUT")).toHaveLength(1);
    expect(deleteCalls()).toHaveLength(1);
  });

  it.each([
    [400, { error: { code: "INVALID_REQUEST", message: "x" } }, "INVALID_REQUEST"],
    [403, { error: { code: "ORIGIN_NOT_ALLOWED", message: "x" } }, "ORIGIN_NOT_ALLOWED"],
    [408, "", null],
    [413, "<html>413 Request Entity Too Large</html>", "UPLOAD_TOO_LARGE"],
  ])("does not retry a chunk answered %i", async (status, body, code) => {
    const { handle } = await started(20, 10);
    const other = pendingChunks()[1];
    pendingChunks()[0].respond(status, body);
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status, code });
    await advance(20_000);
    expect(FakeXhr.requests("PUT")).toHaveLength(2);
    expect(other.aborted).toBe(true);
    expect(deleteCalls()).toHaveLength(1);
  });

  it("does not DELETE a session the server says is gone", async () => {
    const { handle } = await started(10, 10);
    pendingChunks()[0].respond(404, notFound);
    expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 404, code: "UPLOAD_NOT_FOUND" });
    expect(deleteCalls()).toHaveLength(0);
  });

  it("aggregates progress over the chunks in flight and never goes backwards", async () => {
    const { onProgress } = await started(40, 10);
    const put = (i: number) => pendingChunks().find((x) => chunkIndex(x) === i)!;

    put(0).progress(5, 10);
    put(1).progress(5, 10);
    expect(onProgress.mock.lastCall?.[0]).toEqual({ loaded: 10, total: 40, percent: 25, retrying: false, completing: false });

    put(0).progress(10, 10);
    put(0).respond(204);
    await flush();
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 15, percent: 37 });

    // Chunk 1 fails after 5 bytes: its bytes are lost, but the bar does not move back.
    put(1).networkError();
    await flush();
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 15, retrying: true });
    put(2).progress(3, 10);
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 15 });
    put(2).progress(8, 10);
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 18 });

    await advance(1000);
    put(1).progress(10, 10);
    put(2).progress(10, 10);
    // Every byte is sent but not yet stored: still below 100 %.
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 30, percent: 75 });
    put(1).respond(204);
    put(2).respond(204);
    await flush();
    put(3).progress(10, 10);
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 40, percent: 99 });
    put(3).respond(204);
    await flush();
    const events = onProgress.mock.calls.map((c) => c[0]);
    // Every chunk stored, then complete goes out.
    expect(events.slice(-2)).toEqual([
      { loaded: 40, total: 40, percent: 100, retrying: false, completing: false },
      { loaded: 40, total: 40, percent: 100, retrying: false, completing: true },
    ]);
    expect(completeCalls()).toHaveLength(1);

    for (let i = 1; i < events.length; i++) {
      expect(events[i].loaded).toBeGreaterThanOrEqual(events[i - 1].loaded);
      expect(events[i].percent!).toBeGreaterThanOrEqual(events[i - 1].percent!);
      expect(events[i].total).toBe(40);
    }
    expect(events.filter((e) => e.percent === 100)).toHaveLength(2);
    expect(events.filter((e) => e.completing)).toHaveLength(1);
  });

  it("clamps chunk progress to the chunk size", async () => {
    const { onProgress } = await started(10, 10);
    pendingChunks()[0].progress(4096, 4096); // e.g. a proxy reporting framing overhead
    expect(onProgress.mock.lastCall?.[0]).toMatchObject({ loaded: 10, percent: 99 });
  });

  describe("cancel", () => {
    it("aborts every chunk in flight and DELETEs the session with keepalive", async () => {
      const { handle, onProgress } = await started(50, 10);
      const inFlight = pendingChunks();
      expect(inFlight).toHaveLength(2);
      handle.abort();
      expect(inFlight.every((x) => x.aborted)).toBe(true);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(deleteCalls()).toEqual([{ method: "DELETE", keepalive: true, headers: { Accept: "application/json" } }]);
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Nothing else happens afterwards.
      const calls = onProgress.mock.calls.length;
      inFlight[0].progress(5, 10);
      await advance(60_000);
      expect(onProgress.mock.calls.length).toBe(calls);
      expect(FakeXhr.requests("PUT")).toHaveLength(2);
      expect(completeCalls()).toHaveLength(0);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it("works through an AbortSignal", async () => {
      const controller = new AbortController();
      const { handle } = await started(20, 10, { signal: controller.signal });
      controller.abort();
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(FakeXhr.requests("PUT").every((x) => x.aborted)).toBe(true);
      expect(deleteCalls()).toHaveLength(1);
    });

    it("sends nothing when the signal is already aborted", async () => {
      const controller = new AbortController();
      controller.abort();
      const { handle } = start(10, { signal: controller.signal });
      await flush();
      expect(FakeXhr.all).toHaveLength(0);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("during the create call lets it finish and DELETEs the session it returns", async () => {
      const { handle } = start(10);
      await flush();
      const create = createRequest();
      handle.abort();
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(create.aborted).toBe(false);
      expect(fetchMock).not.toHaveBeenCalled(); // no session id yet

      create.respond(201, sessionFor(10, 10));
      await flush();
      expect(deleteCalls()).toHaveLength(1);
      expect(FakeXhr.requests("PUT")).toHaveLength(0);
      expect(completeCalls()).toHaveLength(0);
    });

    it("during the create call DELETEs nothing when the create failed", async () => {
      const { handle } = start(10);
      await flush();
      handle.abort();
      createRequest().respond(503, { error: { code: "SERVER_BUSY", message: "x" } });
      await flush();
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(fetchMock).not.toHaveBeenCalled();
      expect(FakeXhr.all).toHaveLength(1);
    });

    it("during a backoff wait sends no retry", async () => {
      const { handle } = await started(10, 10);
      pendingChunks()[0].networkError();
      await flush();
      handle.abort();
      await advance(20_000);
      expect(FakeXhr.requests("PUT")).toHaveLength(1);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(deleteCalls()).toHaveLength(1);
    });

    it("after complete was sent stops waiting but keeps the session (the job may be created)", async () => {
      const { handle, onProgress } = await started(10, 10);
      await storeAllChunks();
      expect(onProgress.mock.lastCall?.[0].completing).toBe(true);
      const [complete] = completeCalls();
      handle.abort();
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(complete.aborted).toBe(true);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("after completion is a no-op", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      completeCalls()[0].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      handle.abort();
      expect(FakeXhr.all.some((x) => x.aborted)).toBe(false);
      expect(fetchMock).not.toHaveBeenCalled();
    });
  });

  describe("leaving the page", () => {
    it.each(["pagehide", "beforeunload"])("DELETEs the open session with keepalive on %s", async (event) => {
      const { handle } = await started(50, 10);
      const inFlight = pendingChunks();
      page.dispatchEvent(new Event(event));
      expect(deleteCalls()).toEqual([{ method: "DELETE", keepalive: true, headers: { Accept: "application/json" } }]);
      expect(inFlight.every((x) => x.aborted)).toBe(true);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });

      // beforeunload then pagehide: one DELETE.
      page.dispatchEvent(new Event("pagehide"));
      page.dispatchEvent(new Event("beforeunload"));
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it("listens on window by default", async () => {
      const { handle } = await started(20, 10, { pageEvents: undefined });
      window.dispatchEvent(new Event("pagehide"));
      expect(deleteCalls()).toHaveLength(1);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
    });

    it("sends no DELETE once complete was sent, and the upload carries on", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      page.dispatchEvent(new Event("pagehide"));
      page.dispatchEvent(new Event("beforeunload"));
      expect(fetchMock).not.toHaveBeenCalled();
      const [complete] = completeCalls();
      expect(complete.aborted).toBe(false);
      complete.respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
    });

    it("during the create call DELETEs the session once its answer arrives", async () => {
      const { handle } = start(10);
      await flush();
      page.dispatchEvent(new Event("pagehide"));
      expect(fetchMock).not.toHaveBeenCalled();
      createRequest().respond(201, sessionFor(10, 10));
      await flush();
      expect(deleteCalls()).toEqual([{ method: "DELETE", keepalive: true, headers: { Accept: "application/json" } }]);
      expect(await failureOf(handle.promise)).toEqual({ kind: "aborted" });
      expect(FakeXhr.requests("PUT")).toHaveLength(0);
    });

    it("stops listening once the upload is over", async () => {
      const target = { addEventListener: vi.fn(), removeEventListener: vi.fn() };
      const { handle } = await started(10, 10, { pageEvents: target });
      expect(target.addEventListener.mock.calls.map((c) => c[0])).toEqual(["pagehide", "beforeunload"]);
      await storeAllChunks();
      completeCalls()[0].respond(202, JOB);
      await handle.promise;
      expect(target.removeEventListener.mock.calls).toEqual(target.addEventListener.mock.calls.map((c) => [c[0], c[1]]));
    });

    it("falls back to an XHR DELETE without fetch", async () => {
      const { handle } = await started(10, 10, {
        fetch: (() => {
          throw new TypeError("fetch is not available");
        }) as unknown as typeof fetch,
      });
      handle.abort();
      await flush();
      expect(FakeXhr.requests("DELETE", `${BASE}/${UPLOAD_ID}`)).toHaveLength(1);
    });
  });

  describe("complete", () => {
    it.each([
      [400, "TOO_SHORT"],
      [400, "TOO_LONG"],
      [400, "CANNOT_DECODE"],
      [400, "RESOLUTION_TOO_HIGH"],
      [400, "UNSUPPORTED_FORMAT"],
      [409, "UPLOAD_INCOMPLETE"], // without a usable list of missing chunks
    ] as const)("maps a final %i %s and DELETEs the session", async (status, code) => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      completeCalls()[0].respond(status, { error: { code, message: "server detail" } });
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status, code });
      await advance(20_000);
      expect(completeCalls()).toHaveLength(1);
      expect(deleteCalls()).toHaveLength(1);
    });

    it("maps UPLOAD_NOT_FOUND and does not DELETE", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      completeCalls()[0].respond(404, notFound);
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 404, code: "UPLOAD_NOT_FOUND" });
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("retries network errors, timeouts and 5xx with the chunk backoff: the server replays the outcome", async () => {
      const { handle, onProgress } = await started(20, 10, { maxRetries: 8 });
      await storeAllChunks();
      const failures: ((x: FakeXhr) => void)[] = [
        (x) => x.networkError(),
        (x) => x.timeoutNow(),
        (x) => x.respond(500, { error: { code: "INTERNAL_ERROR", message: "x" } }),
        (x) => x.respond(502, "<html>Bad gateway</html>"),
        (x) => x.respond(504, "<html>Gateway timeout</html>"),
        (x) => x.respond(520, "<html>Cloudflare</html>"),
        (x) => x.respond(522, "<html>Cloudflare</html>"),
        (x) => x.respond(524, "<html>A timeout occurred</html>"),
      ];
      const delays = [1000, 2000, 4000, 8000, 16_000, 32_000, 64_000, 128_000];
      for (let i = 0; i < failures.length; i++) {
        const [pending] = pendingCompletes();
        failures[i](pending);
        await flush();
        expect(pendingCompletes()).toHaveLength(0);
        expect(onProgress.mock.lastCall?.[0]).toMatchObject({ retrying: true, completing: true });
        await advance(delays[i] - 1);
        expect(pendingCompletes()).toHaveLength(0);
        await advance(1);
        expect(pendingCompletes()).toHaveLength(1);
      }
      pendingCompletes()[0].respond(202, JOB); // the replay of the first complete's job
      await expect(handle.promise).resolves.toEqual(JOB);
      expect(completeCalls()).toHaveLength(9);
      expect(FakeXhr.requests("PUT")).toHaveLength(2); // chunks are not sent again
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("gives each complete a 60 s timeout by default", async () => {
      await started(10, 10);
      await storeAllChunks();
      expect(completeCalls()[0].timeout).toBe(DEFAULT_COMPLETE_TIMEOUT_MS);
      expect(DEFAULT_COMPLETE_TIMEOUT_MS).toBe(60_000);
    });

    it("gets the replayed rejection after a lost answer, and then DELETEs", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      completeCalls()[0].networkError();
      await advance(1000);
      completeCalls()[1].respond(400, { error: { code: "TOO_SHORT", message: "x" } });
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 400, code: "TOO_SHORT" });
      expect(deleteCalls()).toHaveLength(1);
    });

    it.each([
      [503, { "Retry-After": "3" }, 3000],
      [429, { "Retry-After": "3" }, 3000],
      [429, {}, 1000],
    ] as const)("retries %i after its Retry-After without re-sending chunks", async (status, headers, wait) => {
      const { handle } = await started(20, 10);
      await storeAllChunks();
      const code = status === 429 ? "RATE_LIMITED" : "SERVER_BUSY";
      completeCalls()[0].respond(status, { error: { code, message: "x" } }, headers);
      await flush();
      await advance(wait - 1);
      expect(completeCalls()).toHaveLength(1);
      await advance(1);
      expect(completeCalls()).toHaveLength(2);
      completeCalls()[1].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      expect(FakeXhr.requests("PUT")).toHaveLength(2);
    });

    it.each([
      ["network errors", (x: FakeXhr) => x.networkError(), { kind: "network" }],
      ["timeouts", (x: FakeXhr) => x.timeoutNow(), { kind: "timeout" }],
      ["500s", (x: FakeXhr) => x.respond(500, "oops"), { kind: "http", status: 500, code: "INTERNAL_ERROR" }],
      ["502s", (x: FakeXhr) => x.respond(502, "<html>"), { kind: "http", status: 502, code: "INTERNAL_ERROR" }],
      [
        "503s",
        (x: FakeXhr) => x.respond(503, { error: { code: "SERVER_BUSY", message: "x" } }),
        { kind: "http", status: 503, code: "SERVER_BUSY" },
      ],
    ])("after 4 retries of %s fails without DELETE: the job may exist, or the chunks are kept", async (_, answer, failure) => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      for (let attempt = 0; attempt < 5; attempt++) {
        answer(pendingCompletes()[0]);
        await advance(10_000);
      }
      expect(await failureOf(handle.promise)).toEqual(failure);
      expect(completeCalls()).toHaveLength(5);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("after 4 retries of 429 fails and DELETEs (the server did nothing)", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      for (let attempt = 0; attempt < 5; attempt++) {
        pendingCompletes()[0].respond(429, { error: { code: "RATE_LIMITED", message: "x" } });
        await advance(10_000);
      }
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 429, code: "RATE_LIMITED" });
      expect(deleteCalls()).toHaveLength(1);
    });

    it("does not retry a 2xx without a job id, and keeps the session", async () => {
      const { handle } = await started(10, 10);
      await storeAllChunks();
      completeCalls()[0].respond(202, "not json");
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 202, code: null });
      await advance(20_000);
      expect(completeCalls()).toHaveLength(1);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("re-sends the chunks a 409 lists, then completes again", async () => {
      const { handle, file, onProgress } = await started(30, 10);
      await storeAllChunks();
      completeCalls()[0].respond(409, incomplete([1], 3));
      await flush();
      expect(pendingChunks()).toHaveLength(0); // a short pause first
      await advance(1000);
      const [again] = pendingChunks();
      expect(again.url).toBe(`${BASE}/${UPLOAD_ID}/chunks/1`);
      const sent = new Uint8Array(await again.blob.arrayBuffer());
      expect(sent).toEqual(new Uint8Array(await file.slice(10, 20).arrayBuffer()));
      again.progress(10, 10);
      again.respond(204);
      await flush();
      expect(completeCalls()).toHaveLength(2);
      completeCalls()[1].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      expect(FakeXhr.requests("PUT")).toHaveLength(4);
      expect(fetchMock).not.toHaveBeenCalled();
      // Still "checking" throughout: the bar does not jump back for the re-sent chunk.
      const afterComplete = onProgress.mock.calls.map((c) => c[0]).slice(-1)[0];
      expect(afterComplete).toEqual({ loaded: 30, total: 30, percent: 100, retrying: false, completing: true });
    });

    it("re-sends missing chunks at most twice, then fails with UPLOAD_INCOMPLETE and DELETEs", async () => {
      const { handle } = await started(30, 10);
      await storeAllChunks();
      for (let round = 0; round < 2; round++) {
        completeCalls()[round].respond(409, incomplete([0, 2], 3));
        await advance(10_000);
        expect(pendingIndexes()).toEqual([0, 2]);
        await storeAllChunks();
      }
      completeCalls()[2].respond(409, incomplete([2], 3));
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 409, code: "UPLOAD_INCOMPLETE" });
      await advance(20_000);
      expect(FakeXhr.requests("PUT")).toHaveLength(7);
      expect(completeCalls()).toHaveLength(3);
      expect(deleteCalls()).toHaveLength(1);
    });

    it("keeps Cancel impossible while missing chunks are re-sent", async () => {
      const { onProgress } = await started(20, 10);
      await storeAllChunks();
      completeCalls()[0].respond(409, incomplete([0], 2));
      await advance(1000);
      expect(pendingIndexes()).toEqual([0]);
      expect(onProgress.mock.lastCall?.[0].completing).toBe(true);
    });
  });

  describe("session probe", () => {
    it("after 2 network failures of a chunk finds a lost session and stops with UPLOAD_NOT_FOUND", async () => {
      const { handle } = await started(20, 10);
      const chunk0 = () => pendingChunks().find((x) => chunkIndex(x) === 0)!;
      chunk0().networkError();
      await advance(1000);
      expect(completeCalls()).toHaveLength(0);
      chunk0().networkError();
      await flush();
      const [probe] = completeCalls();
      expect(probe.body).toBeNull();
      probe.respond(404, notFound);
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 404, code: "UPLOAD_NOT_FOUND" });
      await advance(20_000);
      expect(FakeXhr.requests("PUT").filter((x) => chunkIndex(x) === 0)).toHaveLength(2);
      expect(pendingChunks()).toHaveLength(0);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("keeps retrying when the session is alive (409), and holds other chunks back while it asks", async () => {
      const { handle } = await started(30, 10);
      const put = (i: number) => pendingChunks().find((x) => chunkIndex(x) === i);
      put(0)!.networkError();
      await advance(1000);
      put(0)!.timeoutNow(); // a stall counts too
      await flush();
      expect(pendingCompletes()).toHaveLength(1);
      put(1)!.respond(204);
      await flush();
      expect(put(2)).toBeUndefined(); // not while the probe could start the ingest

      pendingCompletes()[0].respond(409, incomplete([0, 2], 3));
      await flush();
      expect(put(2)).toBeDefined();
      await advance(2000); // chunk 0's second backoff
      put(0)!.respond(204);
      put(2)!.respond(204);
      await flush();
      expect(completeCalls()).toHaveLength(2);
      completeCalls()[1].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
    });

    it("probes again after further network failures and reports the last one when the session stays alive", async () => {
      const { handle } = await started(10, 10);
      for (let attempt = 0; attempt < 5; attempt++) {
        pendingChunks()[0].networkError();
        await flush();
        const probe = pendingCompletes()[0];
        if (attempt >= 1) probe.respond(409, incomplete([0], 1));
        else expect(probe).toBeUndefined();
        await advance(10_000);
      }
      expect(await failureOf(handle.promise)).toEqual({ kind: "network" });
      expect(completeCalls()).toHaveLength(4);
      expect(deleteCalls()).toHaveLength(1);
    });

    it("takes the job when the probe finds every chunk stored (only the answers were lost)", async () => {
      const { handle } = await started(10, 10);
      pendingChunks()[0].networkError();
      await advance(1000);
      pendingChunks()[0].networkError();
      await flush();
      pendingCompletes()[0].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      expect(completeCalls()).toHaveLength(1);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("takes the job a probe creates while chunks a 409 listed are re-sent", async () => {
      const { handle } = await started(20, 10);
      await storeAllChunks();
      completeCalls()[0].respond(409, incomplete([1], 2));
      await advance(1000);
      expect(pendingIndexes()).toEqual([1]);
      pendingChunks()[0].networkError();
      await advance(1000);
      pendingChunks()[0].networkError();
      await flush();
      expect(completeCalls()).toHaveLength(2); // the probe
      pendingCompletes()[0].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      await advance(20_000);
      expect(completeCalls()).toHaveLength(2);
      expect(pendingChunks()).toHaveLength(0);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("stops re-sending with UPLOAD_NOT_FOUND when a probe during a 409 round finds the session gone", async () => {
      const { handle } = await started(20, 10);
      await storeAllChunks();
      completeCalls()[0].respond(409, incomplete([1], 2));
      await advance(1000);
      pendingChunks()[0].networkError();
      await advance(1000);
      pendingChunks()[0].networkError();
      await flush();
      pendingCompletes()[0].respond(404, notFound);
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 404, code: "UPLOAD_NOT_FOUND" });
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("lets a probe that created the job win over a chunk refused meanwhile", async () => {
      const { handle } = await started(20, 10);
      const put = (i: number) => pendingChunks().find((x) => chunkIndex(x) === i)!;
      put(0).networkError();
      await advance(1000);
      put(0).networkError();
      await flush();
      // Chunk 1 was being re-sent when the probe started the ingest.
      put(1).respond(400, { error: { code: "INVALID_REQUEST", message: "the upload is being completed" } });
      await flush();
      pendingCompletes()[0].respond(202, JOB);
      await expect(handle.promise).resolves.toEqual(JOB);
      expect(fetchMock).not.toHaveBeenCalled();
    });
  });

  describe("create", () => {
    it.each([
      [413, { error: { code: "UPLOAD_TOO_LARGE", message: "x" } }, {}, { code: "UPLOAD_TOO_LARGE" }],
      [403, { error: { code: "ORIGIN_NOT_ALLOWED", message: "x" } }, {}, { code: "ORIGIN_NOT_ALLOWED" }],
      [422, { error: { code: "INVALID_REQUEST", message: "x" } }, {}, { code: "INVALID_REQUEST" }],
      [400, { error: { code: "BRAND_NEW_CODE", message: "x" } }, {}, { code: null }],
      [429, { error: { code: "RATE_LIMITED", message: "x" } }, { "Retry-After": "360" }, { code: "RATE_LIMITED", retryAfterSec: 360 }],
      [503, { error: { code: "SERVER_BUSY", message: "x" } }, { "retry-after": "30" }, { code: "SERVER_BUSY", retryAfterSec: 30 }],
      [429, { error: { code: "RATE_LIMITED", message: "x" } }, { "Retry-After": "soon" }, { code: "RATE_LIMITED" }],
    ])("reports %i %j without retrying", async (status, body, headers, expected) => {
      const { handle } = start(10);
      await flush();
      createRequest().respond(status, body, headers);
      expect(await failureOf(handle.promise)).toStrictEqual({ kind: "http", status, ...expected });
      await advance(120_000);
      expect(FakeXhr.all).toHaveLength(1);
    });

    it("treats a network error, a timeout and a browser abort distinctly", async () => {
      const cases: [(x: FakeXhr) => void, UploadFailure][] = [
        [(x) => x.networkError(), { kind: "network" }],
        [(x) => x.timeoutNow(), { kind: "timeout" }],
        [(x) => x.onabort?.(), { kind: "network" }],
      ];
      for (const [end, failure] of cases) {
        FakeXhr.reset();
        const { handle } = start(10, { timeoutMs: 5000 });
        await flush();
        expect(createRequest().timeout).toBe(5000);
        end(createRequest());
        expect(await failureOf(handle.promise)).toEqual(failure);
      }
    });

    it("rejects a session that does not match the file, and discards it", async () => {
      const { handle } = start(30);
      await flush();
      createRequest().respond(201, { ...sessionFor(10, 30), chunkCount: 2 });
      expect(await failureOf(handle.promise)).toEqual({ kind: "http", status: 201, code: null });
      expect(FakeXhr.requests("PUT")).toHaveLength(0);
      expect(deleteCalls()).toHaveLength(1);
    });
  });
});
