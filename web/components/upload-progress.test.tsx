import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { progressText, UploadProgress } from "./upload-progress";

const MB = 1024 * 1024;

describe("progressText", () => {
  it("shows percent and bytes while uploading", () => {
    expect(progressText(37, 140 * MB, 377 * MB)).toBe("Uploading… 37% (140 MB of 377 MB)");
    expect(progressText(0, 0, 12.5 * MB)).toBe("Uploading… 0% (0 B of 12.5 MB)");
  });

  it("falls back to bytes when the total is unknown", () => {
    expect(progressText(null, 3 * MB, 0)).toBe("Uploading… 3 MB");
  });

  it("switches to the server check once every byte is stored or complete was sent", () => {
    expect(progressText(100, 377 * MB, 377 * MB)).toBe("Checking the video…");
    expect(progressText(99, 376 * MB, 377 * MB, true)).toBe("Checking the video…");
  });
});

describe("<UploadProgress>", () => {
  it("renders the bar, the text and a working cancel button", async () => {
    const onCancel = vi.fn();
    render(<UploadProgress percent={37} loaded={140 * MB} total={377 * MB} onCancel={onCancel} />);
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toHaveAttribute("aria-valuenow", "37");
    expect(screen.getByTestId("upload-progress-text")).toHaveTextContent("Uploading… 37% (140 MB of 377 MB)");
    expect(screen.getByTestId("upload-retrying")).toBeEmptyDOMElement();
    expect(screen.getByText("Upload 25% complete")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Cancel upload" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("disables Cancel while the server checks the video", () => {
    const onCancel = vi.fn();
    render(<UploadProgress percent={100} loaded={50} total={50} completing onCancel={onCancel} />);
    expect(screen.getByTestId("upload-progress-text")).toHaveTextContent("Checking the video…");
    expect(screen.getByRole("button", { name: "Cancel upload" })).toBeDisabled();
    expect(screen.getByText("Upload finished, checking the video")).toBeInTheDocument();
  });

  it("says when a piece is being sent again", () => {
    render(<UploadProgress percent={50} loaded={50} total={100} retrying onCancel={() => {}} />);
    expect(screen.getByTestId("upload-retrying")).toHaveTextContent("Having trouble reaching the server. Retrying…");
    expect(screen.getByText("Having trouble reaching the server, retrying the upload")).toBeInTheDocument();
  });
});
