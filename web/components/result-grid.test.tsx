import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { API_BASE, JOB_ID, makeImage, makeResult, makeUsage, NO_USAGE } from "@/test/fixtures";

import { reframeInfo, ResultCard } from "./result-card";
import { ResultGrid } from "./result-grid";

const expiresAt = "2026-09-24T10:00:00Z";

function renderGrid(overrides: Partial<Parameters<typeof ResultGrid>[0]> = {}) {
  return render(<ResultGrid result={makeResult()} expiresAt={expiresAt} apiBaseUrl={API_BASE} locale="en-US" {...overrides} />);
}

describe("<ResultGrid>", () => {
  it("renders the header: summary, count, ZIP link and expiry", () => {
    renderGrid();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Walkaround of a silver sedan, exterior then interior.");
    expect(screen.getByTestId("image-count")).toHaveTextContent("3 images");
    const zip = screen.getByRole("link", { name: /Download all \(ZIP\)/ });
    expect(zip).toHaveAttribute("href", `${API_BASE}/v1/domains/cars/jobs/${JOB_ID}/download.zip`);
    expect(screen.getByText(/Available until/)).toHaveTextContent("Available until Sep 24, 2026");
    expect(screen.getByText(/Available until/).querySelector("time")).toHaveAttribute("datetime", expiresAt);
    expect(screen.getByText(/rear 3\/4 angle was not clearly visible/)).toBeInTheDocument();
  });

  it("keeps the API order and shows label, timestamp and reason per card", () => {
    const images = [
      makeImage(2, { shotLabel: "Side profile", timestampLabel: "00:14", reason: "Full side." }),
      makeImage(1, { shotLabel: "Front 3/4", timestampLabel: "00:06", reason: "Hero shot." }),
      makeImage(3, { shotLabel: "Dashboard", timestampLabel: "01:01", reason: "Interior." }),
    ];
    renderGrid({ result: makeResult({ images }) });
    const grid = screen.getByRole("list", { name: "Selected images" });
    const cards = within(grid).getAllByRole("listitem");
    expect(cards.map((c) => within(c).getByTestId("shot-label").textContent)).toEqual(["Side profile", "Front 3/4", "Dashboard"]);
    expect(cards.map((c) => within(c).getByTestId("timestamp").textContent)).toEqual(["00:14", "00:06", "01:01"]);
    expect(cards[0]).toHaveTextContent("Full side.");
  });

  it("resolves thumbnail and download URLs against the API base", () => {
    const image = makeImage(1);
    renderGrid({ result: makeResult({ images: [image] }) });
    const img = screen.getByRole("img", { name: /Front 3\/4, frame at 00:06/ });
    expect(img).toHaveAttribute("src", `${API_BASE}${image.thumbnailUrl}`);
    const download = screen.getByRole("link", { name: `Download Front 3/4 (${image.fileName})` });
    expect(download).toHaveAttribute("href", `${API_BASE}${image.downloadUrl}`);
    expect(download).toHaveAttribute("download", image.fileName);
  });

  it("leaves absolute (presigned) URLs untouched", () => {
    const image = makeImage(1, { thumbnailUrl: "https://minio.example.com/v2i/thumb.jpg?sig=1" });
    render(<ResultCard image={image} apiBaseUrl={API_BASE} onOpen={() => {}} />);
    expect(screen.getByRole("img")).toHaveAttribute("src", "https://minio.example.com/v2i/thumb.jpg?sig=1");
  });

  it("shows the AI usage line in the header, collapsed", () => {
    renderGrid({ usage: makeUsage() });
    const header = screen.getByRole("banner");
    const panel = within(header).getByTestId("ai-usage");
    expect(panel).toHaveTextContent("AI usage: 9,012 tokens · $0.0041");
    expect(panel).not.toHaveAttribute("open");
  });

  it("shows the AI usage line on an empty result too", () => {
    renderGrid({ usage: makeUsage(), result: makeResult({ images: [], zipUrl: null }) });
    expect(screen.getByTestId("ai-usage")).toHaveTextContent("9,012 tokens");
  });

  it("hides the AI usage line without model calls", () => {
    const { unmount } = renderGrid({ usage: NO_USAGE });
    expect(screen.queryByTestId("ai-usage")).not.toBeInTheDocument();
    unmount();
    renderGrid();
    expect(screen.queryByTestId("ai-usage")).not.toBeInTheDocument();
  });

  it("marks straightened and cropped images next to the timestamp", () => {
    const images = [
      makeImage(1),
      makeImage(2, { rotation: 90 }),
      makeImage(3, { cropped: true }),
      makeImage(4, { rotation: 270, cropped: true }),
    ];
    renderGrid({ result: makeResult({ images }) });
    const cards = within(screen.getByRole("list", { name: "Selected images" })).getAllByRole("listitem");
    expect(within(cards[0]).queryByTestId("reframed")).not.toBeInTheDocument();

    const straightened = within(cards[1]).getByTestId("reframed");
    expect(straightened).toHaveTextContent(/^Straightened/);
    expect(straightened).toHaveAttribute("title", "Reframed from the video frame: rotated 90° clockwise to stand upright.");
    // Same row as the timestamp.
    expect(straightened.parentElement).toContainElement(within(cards[1]).getByTestId("timestamp"));

    const cropped = within(cards[2]).getByTestId("reframed");
    expect(cropped).toHaveTextContent(/^Cropped/);
    expect(cropped).toHaveAttribute("title", "Reframed from the video frame: cropped to frame the subject.");

    const both = within(cards[3]).getByTestId("reframed");
    expect(both).toHaveTextContent(/^Straightened · Cropped/);
    expect(both).toHaveAttribute(
      "title",
      "Reframed from the video frame: rotated 270° clockwise to stand upright and cropped to frame the subject.",
    );
    // Screen readers get the explanation, not just the short label.
    expect(both).toHaveTextContent("rotated 270° clockwise");
  });

  it("describes reframing, ignoring full turns", () => {
    expect(reframeInfo({ rotation: 0, cropped: false })).toBeNull();
    expect(reframeInfo({ rotation: 360, cropped: false })).toBeNull();
    expect(reframeInfo({ rotation: -90, cropped: false })?.description).toContain("rotated 270°");
    expect(reframeInfo({ rotation: 180, cropped: false })?.label).toBe("Straightened");
    expect(reframeInfo({ rotation: 0, cropped: true })?.label).toBe("Cropped");
    expect(reframeInfo({ rotation: 90, cropped: true })?.label).toBe("Straightened · Cropped");
  });

  it("lists missing shots with a re-shoot hint", () => {
    renderGrid({
      result: makeResult({
        missingShots: [
          { id: "rear_three_quarter", label: "Rear 3/4" },
          { id: "interior_dashboard", label: "Dashboard" },
        ],
      }),
    });
    const callout = screen.getByTestId("missing-shots");
    expect(callout).toHaveTextContent("2 shots are missing");
    expect(within(callout).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Rear 3/4", "Dashboard"]);
    expect(callout).toHaveTextContent(/upload the video again/);
  });

  it("hides the missing-shots callout when nothing is missing", () => {
    renderGrid({ result: makeResult({ missingShots: [] }) });
    expect(screen.queryByTestId("missing-shots")).not.toBeInTheDocument();
  });

  it("shows the empty state with the explanation when no images were selected", () => {
    renderGrid({
      result: makeResult({ images: [], zipUrl: null, explanation: "The car was never fully in frame." }),
    });
    const empty = screen.getByTestId("empty-results");
    expect(within(empty).getByRole("heading")).toHaveTextContent("No suitable scenes found");
    expect(empty).toHaveTextContent("The car was never fully in frame.");
    expect(screen.queryByRole("link", { name: /Download all/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Selected images" })).not.toBeInTheDocument();
    expect(screen.getByTestId("image-count")).toHaveTextContent("0 images");
  });

  it("hides the ZIP button when zipUrl is null", () => {
    renderGrid({ result: makeResult({ zipUrl: null }) });
    expect(screen.queryByRole("link", { name: /Download all/ })).not.toBeInTheDocument();
  });

  it("asks for confirmation before deleting", async () => {
    const user = userEvent.setup();
    const onDeleteNow = vi.fn();
    renderGrid({ onDeleteNow });
    await user.click(screen.getByRole("button", { name: "Delete now" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete these images now?" });
    await user.click(within(dialog).getByRole("button", { name: "Keep them" }));
    expect(onDeleteNow).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Delete now" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete now" }));
    expect(onDeleteNow).toHaveBeenCalledTimes(1);
  });

  it("has no delete action without a handler", () => {
    renderGrid();
    expect(screen.queryByRole("button", { name: "Delete now" })).not.toBeInTheDocument();
  });

  it("opens the full-resolution image in a lightbox with keyboard navigation", async () => {
    const user = userEvent.setup();
    const result = makeResult();
    renderGrid({ result });

    await user.click(screen.getByRole("button", { name: "View Side profile at 00:14 in full size" }));
    let dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Side profile");
    expect(within(dialog).queryByTestId("reframed")).not.toBeInTheDocument();
    expect(dialog).toHaveTextContent("2 of 3");
    expect(within(dialog).getByRole("img")).toHaveAttribute("src", `${API_BASE}${result.images[1].url}`);

    await user.keyboard("{ArrowRight}");
    dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("Dashboard");
    expect(dialog).toHaveTextContent("3 of 3");
    expect(within(dialog).getByRole("button", { name: "Next image" })).toBeDisabled();

    await user.keyboard("{ArrowRight}"); // stays on the last image
    expect(screen.getByRole("dialog")).toHaveTextContent("3 of 3");

    await user.keyboard("{ArrowLeft}{ArrowLeft}");
    dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("Front 3/4");
    expect(within(dialog).getByRole("button", { name: "Previous image" })).toBeDisabled();

    await user.click(within(dialog).getByRole("button", { name: "Next image" }));
    expect(screen.getByRole("dialog")).toHaveTextContent("Side profile");

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

describe("<Lightbox> reframe badge", () => {
  it("shows the badge for a cropped image", async () => {
    const user = userEvent.setup();
    renderGrid({ result: makeResult({ images: [makeImage(1, { cropped: true, rotation: 90 })] }) });
    await user.click(screen.getByRole("button", { name: /in full size/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByTestId("reframed")).toHaveTextContent(/^Straightened · Cropped/);
  });
});
