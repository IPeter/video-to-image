import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";

import { carsDomain, JOB_ID } from "@/test/fixtures";
import { FakeXhr } from "@/test/fake-xhr";
import { renderWithProviders } from "@/test/render";

import { UploadForm } from "./upload-form";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

const probe = vi.fn();
vi.mock("@/lib/video-meta", () => ({ probeVideoFile: (...args: unknown[]) => probe(...args) }));

function videoFile(name = "walkaround.mp4", size = 50 * 1024 * 1024, type = "video/mp4"): File {
  const file = new File([new Uint8Array(16)], name, { type });
  Object.defineProperty(file, "size", { value: size });
  return file;
}

const MiB = 1024 * 1024;
const UPLOADS = "http://api.test:4000/v1/domains/cars/uploads";
const UPLOAD_ID = "upl_01J8ZK3M2Q7K2M9QXA4B5C6D7E";

function session(chunkSizeBytes: number, sizeBytes: number) {
  return {
    uploadId: UPLOAD_ID,
    chunkSizeBytes,
    chunkCount: Math.max(1, Math.ceil(sizeBytes / chunkSizeBytes)),
    expiresAt: "2026-09-23T12:00:00Z",
  };
}

/** The pending create call (POST .../uploads). */
function createCall(): Promise<FakeXhr> {
  return waitFor(() => {
    const [create] = FakeXhr.pending("POST", UPLOADS).filter((x) => x.url === UPLOADS);
    expect(create).toBeDefined();
    return create;
  });
}

function chunksInFlight(count: number): Promise<FakeXhr[]> {
  return waitFor(() => {
    const pending = FakeXhr.pending("PUT", "/chunks/");
    expect(pending).toHaveLength(count);
    return pending;
  });
}

function completeCall(): Promise<FakeXhr> {
  return waitFor(() => {
    const [complete] = FakeXhr.pending("POST", `${UPLOADS}/${UPLOAD_ID}/complete`);
    expect(complete).toBeDefined();
    return complete;
  });
}

/** Picks a 50 MB video, starts the upload and answers the create call. */
async function startUpload(user: ReturnType<typeof userEvent.setup>, chunkSizeBytes = 64 * MiB) {
  await user.upload(fileInput(), videoFile());
  await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
  const create = await createCall();
  await act(async () => create.respond(201, session(chunkSizeBytes, 50 * MiB)));
}

/** Uploads the (single-chunk) video and returns the pending complete call. */
async function uploadUntilComplete(user: ReturnType<typeof userEvent.setup>): Promise<FakeXhr> {
  await startUpload(user);
  const [chunk] = await chunksInFlight(1);
  await act(async () => chunk.respond(204));
  return completeCall();
}

/** The session DELETE goes out through fetch with keepalive. */
let fetchMock: Mock<typeof fetch>;
const sessionDeletes = () =>
  fetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE").map(([url, init]) => [String(url), init?.keepalive]);

function renderForm() {
  return renderWithProviders(<UploadForm limits={carsDomain.limits} />);
}

function fileInput(): HTMLInputElement {
  return screen.getByLabelText("Choose a video file") as HTMLInputElement;
}

beforeEach(() => {
  push.mockReset();
  probe.mockReset();
  probe.mockResolvedValue({
    metadata: { durationSec: 74.2, width: 1920, height: 1080 },
    posterDataUrl: "data:image/jpeg;base64,AAAA",
  });
  FakeXhr.reset();
  vi.stubGlobal("XMLHttpRequest", FakeXhr);
  fetchMock = vi.fn<typeof fetch>(() => Promise.resolve({ ok: true, status: 204 } as Response));
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("<UploadForm>", () => {
  it("offers an accessible video-only file input", () => {
    renderForm();
    const input = fileInput();
    expect(input).toHaveAttribute("type", "file");
    expect(input).toHaveAttribute("accept", "video/*");
    expect(input).toHaveAccessibleDescription("MP4 or MOV, up to 500 MB.");
  });

  it("previews a picked video with name, size, length and poster", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());

    const selected = screen.getByRole("region", { name: "Selected video" });
    expect(selected).toHaveTextContent("walkaround.mp4");
    expect(selected).toHaveTextContent("50 MB");
    expect(await screen.findByText("1:14")).toBeInTheDocument();
    expect(selected).toHaveTextContent("1920×1080");
    expect(screen.getByRole("img", { name: "First frames of walkaround.mp4" })).toHaveAttribute(
      "src",
      "data:image/jpeg;base64,AAAA",
    );
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeEnabled();
    expect(probe).toHaveBeenCalledTimes(1);
  });

  it("blocks files over the size limit before uploading", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile("huge.mov", carsDomain.limits.maxUploadBytes + 1, "video/quicktime"));
    expect(screen.getByRole("alert")).toHaveTextContent("larger than the 500 MB limit");
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeDisabled();
    expect(probe).not.toHaveBeenCalled();
  });

  it("accepts a 377 MB phone video: the limit is the domain's, not one request's", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile("IMG_1972.MOV", 377 * 1000 * 1000, "video/quicktime"));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeEnabled();
  });

  it("blocks files that are not videos", async () => {
    const user = userEvent.setup({ applyAccept: false });
    renderForm();
    await user.upload(fileInput(), new File(["x"], "car.jpg", { type: "image/jpeg" }));
    expect(screen.getByRole("alert")).toHaveTextContent("does not look like a video");
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeDisabled();
  });

  it("warns, but does not block, on duration and resolution", async () => {
    probe.mockResolvedValue({ metadata: { durationSec: 3, width: 640, height: 360 }, posterDataUrl: null });
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    const checks = await screen.findByRole("list", { name: "Checks on the selected video" });
    await waitFor(() => expect(checks).toHaveTextContent("between 5 and 180 seconds"));
    expect(checks).toHaveTextContent("at least 480 pixels");
    expect(screen.getByText("No preview available")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeEnabled();
  });

  it("warns, but does not block, when the resolution is above the pixel budget", async () => {
    probe.mockResolvedValue({ metadata: { durationSec: 60, width: 7680, height: 4320 }, posterDataUrl: null });
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    const checks = await screen.findByRole("list", { name: "Checks on the selected video" });
    await waitFor(() => expect(checks).toHaveTextContent("This video is 7680×4320."));
    expect(checks).toHaveTextContent("up to about 3840×2160 (4K)");
    expect(checks.querySelector('[data-issue="RESOLUTION_TOO_HIGH"]')).not.toBeNull();
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeEnabled();
  });

  it("uploads in chunks with progress and opens the job page on 202", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));

    const create = await createCall();
    expect(create.json).toEqual({ fileName: "walkaround.mp4", sizeBytes: 50 * MiB, contentType: "video/mp4" });
    await act(async () => create.respond(201, session(16 * MiB, 50 * MiB)));

    const [c0, c1] = await chunksInFlight(2);
    expect(c0.url).toBe(`${UPLOADS}/${UPLOAD_ID}/chunks/0`);
    expect(c1.url).toBe(`${UPLOADS}/${UPLOAD_ID}/chunks/1`);
    act(() => {
      c0.progress(8 * MiB, 16 * MiB);
      c1.progress(4.5 * MiB, 16 * MiB);
    });
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toHaveAttribute("aria-valuenow", "25");
    expect(screen.getByTestId("upload-progress-text")).toHaveTextContent("Uploading… 25% (12.5 MB of 50 MB)");
    expect(screen.getByRole("button", { name: "Choose a different video" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel upload" })).toBeEnabled();

    await act(async () => {
      c0.respond(204);
      c1.respond(204);
    });
    const rest = await chunksInFlight(2);
    await act(async () => rest.forEach((x) => x.respond(204)));

    const complete = await completeCall();
    expect(screen.getByTestId("upload-progress-text")).toHaveTextContent("Checking the video…");
    await act(async () => complete.respond(202, { jobId: JOB_ID, status: "queued" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith(`/jobs/${JOB_ID}`));
    expect(screen.getByText(/Opening your job/)).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("disables Cancel once complete was sent, and keeps it so while complete is retried", async () => {
    const user = userEvent.setup();
    renderForm();
    await startUpload(user);
    const [chunk] = await chunksInFlight(1);
    expect(screen.getByRole("button", { name: "Cancel upload" })).toBeEnabled();
    await act(async () => chunk.respond(204));
    const complete = await completeCall();
    expect(screen.getByTestId("upload-progress-text")).toHaveTextContent("Checking the video…");
    expect(screen.getByRole("button", { name: "Cancel upload" })).toBeDisabled();

    // A lost answer: complete is sent again (the server replays its outcome).
    await act(async () => complete.respond(502, "<html>Bad gateway</html>"));
    expect(screen.getByRole("button", { name: "Cancel upload" })).toBeDisabled();
    const again = await waitFor(
      () => {
        const pending = FakeXhr.pending("POST", `${UPLOADS}/${UPLOAD_ID}/complete`);
        expect(pending).toHaveLength(1);
        return pending[0];
      },
      { timeout: 3000 },
    );
    expect(again).not.toBe(complete);
    await act(async () => again.respond(202, { jobId: JOB_ID, status: "queued" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith(`/jobs/${JOB_ID}`));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("says so while a chunk is being sent again", async () => {
    const user = userEvent.setup();
    renderForm();
    await startUpload(user);
    const [chunk] = await chunksInFlight(1);
    await act(async () => chunk.respond(503, { error: { code: "SERVER_BUSY", message: "x" } }));
    expect(screen.getByTestId("upload-retrying")).toHaveTextContent("Having trouble reaching the server. Retrying…");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    // The retry follows after about a second.
    const [again] = await waitFor(
      () => {
        const pending = FakeXhr.pending("PUT", "/chunks/0");
        expect(pending).toHaveLength(1);
        return pending;
      },
      { timeout: 3000 },
    );
    expect(again).not.toBe(chunk);
    expect(screen.getByTestId("upload-retrying")).toBeEmptyDOMElement();
  });

  it("explains an expired upload session", async () => {
    const user = userEvent.setup();
    renderForm();
    await startUpload(user);
    const [chunk] = await chunksInFlight(1);
    await act(async () => chunk.respond(404, { error: { code: "UPLOAD_NOT_FOUND", message: "no such upload" } }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The upload expired or was interrupted. Please upload the video again.");
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeEnabled();
  });

  it("maps a server error code to copy and never shows the raw message", async () => {
    const user = userEvent.setup();
    renderForm();
    const complete = await uploadUntilComplete(user);
    await act(async () => complete.respond(400, { error: { code: "TOO_LONG", message: "duration 312.4s > 180s" } }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The video is too long. Videos must be between 5 and 180 seconds long.");
    expect(alert).not.toHaveTextContent("312.4");
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeEnabled();
    expect(push).not.toHaveBeenCalled();
  });

  it("says how long to wait after 429 and disables the retry until then", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const create = await createCall();
    await act(async () =>
      create.respond(429, { error: { code: "RATE_LIMITED", message: "slow down" } }, { "Retry-After": "360" }),
    );

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("You have sent a lot of requests");
    expect(alert).toHaveTextContent("You can upload again in about 6 minutes.");
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeDisabled();

    // Picking another video does not get around the wait.
    await user.click(screen.getByRole("button", { name: "Choose a different video" }));
    await user.upload(fileInput(), videoFile("other.mp4"));
    expect(screen.getByRole("button", { name: "Upload and find the best shots" })).toBeDisabled();
    expect(screen.getByTestId("upload-retry-wait")).toHaveTextContent("You can upload again in about 6 minutes.");
  });

  it("re-enables the retry when the Retry-After wait is over", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const create = await createCall();
    await act(async () => create.respond(503, { error: { code: "SERVER_BUSY", message: "busy" } }, { "Retry-After": "1" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("We're busy processing other uploads. Please try again in a minute.");
    expect(alert).toHaveTextContent("You can upload again in about 1 second.");
    const retry = screen.getByRole("button", { name: "Try the upload again" });
    expect(retry).toBeDisabled();

    await waitFor(() => expect(retry).toBeEnabled(), { timeout: 3000 });
    expect(alert).toHaveTextContent("You can upload again now.");
    await user.click(retry);
    expect(await createCall()).not.toBe(create);
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toBeInTheDocument();
  });

  it("keeps the retry enabled after a 429 without Retry-After", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const create = await createCall();
    await act(async () => create.respond(429, { error: { code: "RATE_LIMITED", message: "x" } }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Please wait a while and try again.");
    expect(screen.queryByTestId("upload-retry-wait")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeEnabled();
  });

  it("reports network failures distinctly", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const create = await createCall();
    await act(async () => create.networkError());
    expect(await screen.findByRole("alert")).toHaveTextContent("could not reach the server");
  });

  it("cancels an upload: aborts the chunks in flight and discards the session", async () => {
    const user = userEvent.setup();
    renderForm();
    await startUpload(user, 16 * MiB);
    const inFlight = await chunksInFlight(2);
    await user.click(screen.getByRole("button", { name: "Cancel upload" }));

    expect(inFlight.every((x) => x.aborted)).toBe(true);
    expect(sessionDeletes()).toEqual([[`${UPLOADS}/${UPLOAD_ID}`, true]]);
    expect(await screen.findByText("Upload cancelled.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeEnabled();
  });

  it("accepts a dropped file and can reset the selection", async () => {
    const user = userEvent.setup();
    renderForm();
    const zone = fileInput().closest("label")!;
    const file = videoFile("dropped.mov", 10 * 1024 * 1024, "video/quicktime");
    fireEvent.dragEnter(zone, { dataTransfer: { types: ["Files"], files: [file] } });
    expect(zone).toHaveAttribute("data-dragging");
    fireEvent.drop(zone, { dataTransfer: { types: ["Files"], files: [file] } });

    expect(await screen.findByRole("region", { name: "Selected video" })).toHaveTextContent("dropped.mov");
    await user.click(screen.getByRole("button", { name: "Choose a different video" }));
    expect(fileInput()).toBeInTheDocument();
  });

  it("aborts an in-flight upload on unmount", async () => {
    const user = userEvent.setup();
    const { unmount } = renderForm();
    await startUpload(user);
    const [chunk] = await chunksInFlight(1);
    unmount();
    expect(chunk.aborted).toBe(true);
    expect(sessionDeletes()).toEqual([[`${UPLOADS}/${UPLOAD_ID}`, true]]);
  });

  it("discards the session when the page is left mid-upload", async () => {
    const user = userEvent.setup();
    renderForm();
    await startUpload(user);
    const [chunk] = await chunksInFlight(1);
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(chunk.aborted).toBe(true);
    expect(sessionDeletes()).toEqual([[`${UPLOADS}/${UPLOAD_ID}`, true]]);
    expect(await screen.findByText("Upload cancelled.")).toBeInTheDocument();
  });

  it("cancelled while the session is being created: DELETEs it once the answer arrives", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const create = await createCall();
    await user.click(screen.getByRole("button", { name: "Cancel upload" }));
    expect(await screen.findByText("Upload cancelled.")).toBeInTheDocument();
    expect(create.aborted).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();

    await act(async () => create.respond(201, session(64 * MiB, 50 * MiB)));
    await waitFor(() => expect(sessionDeletes()).toEqual([[`${UPLOADS}/${UPLOAD_ID}`, true]]));
    expect(FakeXhr.requests("PUT")).toHaveLength(0);
  });
});
