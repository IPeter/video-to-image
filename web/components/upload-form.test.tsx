import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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
  FakeXhr.last = null;
  vi.stubGlobal("XMLHttpRequest", FakeXhr);
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

  it("uploads with progress and opens the job page on 202", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));

    const xhr = FakeXhr.last!;
    expect(xhr.method).toBe("POST");
    expect(xhr.url).toBe("http://api.test:4000/v1/domains/cars/jobs");
    expect(xhr.body?.get("video")).toBeInstanceOf(File);

    act(() => xhr.progress(25 * 1024 * 1024, 50 * 1024 * 1024));
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toHaveAttribute("aria-valuenow", "50");
    expect(screen.getByText("50%")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Choose a different video" })).toBeDisabled();

    act(() => xhr.respond(202, { jobId: JOB_ID, status: "queued" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith(`/jobs/${JOB_ID}`));
    expect(screen.getByText(/Opening your job/)).toBeInTheDocument();
  });

  it("maps a server error code to copy and never shows the raw message", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    act(() => FakeXhr.last!.respond(400, { error: { code: "TOO_LONG", message: "duration 312.4s > 180s" } }));

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
    act(() =>
      FakeXhr.last!.respond(429, { error: { code: "RATE_LIMITED", message: "slow down" } }, { "Retry-After": "360" }),
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
    act(() => FakeXhr.last!.respond(503, { error: { code: "SERVER_BUSY", message: "busy" } }, { "Retry-After": "1" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("We're busy processing other uploads. Please try again in a minute.");
    expect(alert).toHaveTextContent("You can upload again in about 1 second.");
    const retry = screen.getByRole("button", { name: "Try the upload again" });
    expect(retry).toBeDisabled();

    await waitFor(() => expect(retry).toBeEnabled(), { timeout: 3000 });
    expect(alert).toHaveTextContent("You can upload again now.");
    await user.click(retry);
    expect(FakeXhr.last!.method).toBe("POST");
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toBeInTheDocument();
  });

  it("keeps the retry enabled after a 429 without Retry-After", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    act(() => FakeXhr.last!.respond(429, { error: { code: "RATE_LIMITED", message: "x" } }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Please wait a while and try again.");
    expect(screen.queryByTestId("upload-retry-wait")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try the upload again" })).toBeEnabled();
  });

  it("reports network failures distinctly", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    act(() => FakeXhr.last!.networkError());
    expect(await screen.findByRole("alert")).toHaveTextContent("could not reach the server");
  });

  it("cancels an upload", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const xhr = FakeXhr.last!;
    await user.click(screen.getByRole("button", { name: "Cancel upload" }));

    expect(xhr.aborted).toBe(true);
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
    await user.upload(fileInput(), videoFile());
    await user.click(screen.getByRole("button", { name: "Upload and find the best shots" }));
    const xhr = FakeXhr.last!;
    unmount();
    expect(xhr.aborted).toBe(true);
  });
});
