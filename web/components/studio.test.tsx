import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { BACKGROUND_STORAGE_KEY } from "@/hooks/use-background-preference";
import type { JobResult, ResultImage } from "@/lib/api";
import { STUDIO_BADGE_TEXT } from "@/lib/studio";
import { API_BASE, JOB_ID, makeImage, makeJobStudio, makeResult, makeStudioImage, NO_STUDIO } from "@/test/fixtures";

import { ResultCard } from "./result-card";
import { ResultGrid, type ResultGridProps } from "./result-grid";

const expiresAt = "2099-09-24T10:00:00Z";

/** Front 3/4 and side profile with studio versions, a withheld rear 3/4, an interior. */
function studioResult(): JobResult {
  return makeResult({
    images: [
      makeStudioImage(1),
      makeStudioImage(2, { shotId: "side_profile", shotLabel: "Side profile", timestampLabel: "00:14" }),
      makeImage(3, {
        shotId: "rear_three_quarter",
        shotLabel: "Rear 3/4",
        timestampLabel: "00:21",
        studio: { status: "withheld", reason: "The car touches the right edge of the photo, so part of it may be cut off.", variants: [] },
      }),
      makeImage(4, { shotId: "interior_dashboard", shotLabel: "Dashboard", timestampLabel: "01:01" }),
    ],
  });
}

function renderGrid(overrides: Partial<ResultGridProps> = {}) {
  const props: ResultGridProps = {
    result: studioResult(),
    expiresAt,
    apiBaseUrl: API_BASE,
    locale: "en-US",
    studio: makeJobStudio(),
    ...overrides,
  };
  const view = render(<ResultGrid {...props} />);
  return { ...view, rerenderGrid: (next: Partial<ResultGridProps>) => view.rerender(<ResultGrid {...props} {...next} />) };
}

/** The cards' <article>s, in grid order. */
function cards() {
  return within(screen.getByRole("list", { name: "Selected images" }))
    .getAllByRole("listitem")
    .map((li) => within(li).getByRole("article"));
}

function card(label: string) {
  const found = cards().find((c) => within(c).getByTestId("shot-label").textContent === label);
  if (!found) throw new Error(`no card ${label}`);
  return found;
}

function thumbSrc(c: HTMLElement) {
  return within(c).getByRole("img").getAttribute("src");
}

function studioThumb(image: ResultImage, style: string) {
  return `${API_BASE}/v1/domains/cars/jobs/${JOB_ID}/images/${image.id}/studio/${style}/thumb`;
}

describe("studio backgrounds on the results page", () => {
  it("shows no switch and only originals without studio versions", () => {
    renderGrid({ result: makeResult(), studio: NO_STUDIO });
    expect(screen.queryByTestId("background-switch")).not.toBeInTheDocument();
    expect(screen.queryByTestId("studio-bar")).not.toBeInTheDocument();
    expect(screen.queryByTestId("studio-line")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /studio photos/ })).not.toBeInTheDocument();
    for (const c of cards()) expect(c).toHaveAttribute("data-version", "original");
  });

  it("shows the studio version in the default style by default", () => {
    const result = studioResult();
    renderGrid({ result, studio: makeJobStudio({ styles: ["light", "grey", "dark"], defaultStyle: "light" }) });

    const mode = screen.getByRole("group", { name: "Background" });
    expect(within(mode).getByRole("radio", { name: "Studio" })).toBeChecked();
    expect(within(mode).getByRole("radio", { name: "Original" })).not.toBeChecked();
    const style = screen.getByRole("group", { name: "Studio style" });
    expect(within(style).getAllByRole("radio").map((r) => r.closest("label")?.textContent)).toEqual(["Light", "Grey", "Dark"]);
    expect(within(style).getByRole("radio", { name: "Light" })).toBeChecked();

    const front = card("Front 3/4");
    expect(front).toHaveAttribute("data-version", "studio");
    expect(thumbSrc(front)).toBe(studioThumb(result.images[0], "light"));
    expect(within(front).getByTestId("studio-badge")).toHaveTextContent(STUDIO_BADGE_TEXT);
    expect(within(front).getByRole("img")).toHaveAccessibleName(
      /^Front 3\/4, frame at 00:06 on a studio background \(the car itself is unchanged\)\./,
    );
    expect(within(front).getByRole("button", { name: /Front 3\/4 at 00:06 \(studio background\) in full size/ })).toBeInTheDocument();
    const download = within(front).getByRole("link", { name: /Download Front 3\/4 with studio background/ });
    expect(download).toHaveAttribute("href", `${API_BASE}${result.images[0].studio!.variants[1].downloadUrl}`);
    expect(download).toHaveAttribute("download", "cars_QXA4B5C6_1_front_three_quarter_00-06_studio-light.jpg");
    expect(within(front).getByRole("button", { name: "Show original" })).toHaveAttribute("aria-pressed", "false");

    // The interior has no studio version: original, no studio line.
    const dash = card("Dashboard");
    expect(dash).toHaveAttribute("data-version", "original");
    expect(within(dash).queryByTestId("studio-line")).not.toBeInTheDocument();
    expect(within(dash).queryByRole("button", { name: "Show original" })).not.toBeInTheDocument();
  });

  it("shows the original of a withheld image with a short note", () => {
    const result = studioResult();
    renderGrid({ result });
    const rear = card("Rear 3/4");
    expect(rear).toHaveAttribute("data-version", "original");
    expect(thumbSrc(rear)).toBe(`${API_BASE}${result.images[2].thumbnailUrl}`);
    expect(within(rear).getByTestId("studio-note")).toHaveTextContent(
      "Studio version withheld: The car touches the right edge of the photo, so part of it may be cut off.",
    );
    expect(within(rear).queryByTestId("studio-badge")).not.toBeInTheDocument();
    expect(within(rear).queryByRole("button", { name: "Show original" })).not.toBeInTheDocument();
  });

  it("notes a failed studio version and shows the original", () => {
    const failed = makeImage(1, { studio: { status: "failed", reason: "Something went wrong while making the studio background.", variants: [] } });
    renderGrid({ result: makeResult({ images: [failed, makeStudioImage(2)] }) });
    const c = cards()[0];
    expect(c).toHaveAttribute("data-version", "original");
    expect(within(c).getByTestId("studio-note")).toHaveTextContent(
      "Studio version unavailable: Something went wrong while making the studio background.",
    );
  });

  it("says the studio versions are being prepared, without a switch, while none is ready", () => {
    const pending = { status: "pending" as const, reason: "", variants: [] };
    renderGrid({
      result: makeResult({ images: [makeImage(1, { studio: pending }), makeImage(2)] }),
      studio: makeJobStudio({ status: "running", zipUrl: null }),
    });
    expect(screen.getByTestId("studio-preparing")).toHaveTextContent("Preparing studio backgrounds");
    expect(screen.queryByTestId("background-switch")).not.toBeInTheDocument();
    const c = cards()[0];
    expect(within(c).getByTestId("studio-pending")).toHaveTextContent("Preparing studio version…");
    expect(c).toHaveAttribute("data-version", "original");
    // The 4:3 frame is there from the start, so nothing moves when the version arrives.
    expect(within(c).getByRole("img").closest("div")).toHaveStyle({ aspectRatio: "4 / 3" });
  });

  it("swaps in studio versions as they arrive and announces it once", () => {
    const pending = { status: "pending" as const, reason: "", variants: [] };
    const first = makeImage(1, { studio: pending });
    const { rerenderGrid } = renderGrid({
      result: makeResult({ images: [first, makeImage(2, { shotLabel: "Side profile", studio: pending })] }),
      studio: makeJobStudio({ status: "running", zipUrl: null }),
    });
    expect(screen.getByTestId("studio-announcer")).toBeEmptyDOMElement();

    const ready = makeStudioImage(1);
    rerenderGrid({
      result: makeResult({ images: [ready, makeImage(2, { shotLabel: "Side profile", studio: pending })] }),
      studio: makeJobStudio({ status: "running", zipUrl: null }),
    });
    expect(screen.getByTestId("background-switch")).toBeInTheDocument();
    expect(screen.queryByTestId("studio-preparing")).not.toBeInTheDocument();
    expect(thumbSrc(card("Front 3/4"))).toBe(studioThumb(ready, "grey"));
    expect(within(card("Side profile")).getByTestId("studio-pending")).toBeInTheDocument();
    expect(screen.getByTestId("studio-announcer")).toHaveTextContent(
      "Studio backgrounds are ready. Showing the grey studio background; the originals stay available.",
    );
    expect(screen.queryByRole("link", { name: "Download studio photos (ZIP)" })).not.toBeInTheDocument();
  });

  it("flips one card with Show original", async () => {
    const user = userEvent.setup();
    const result = studioResult();
    renderGrid({ result });
    const front = card("Front 3/4");
    const toggle = within(front).getByRole("button", { name: "Show original" });

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    expect(front).toHaveAttribute("data-version", "original");
    expect(thumbSrc(front)).toBe(`${API_BASE}${result.images[0].thumbnailUrl}`);
    expect(within(front).getByTestId("studio-original")).toHaveTextContent("Original photo");
    expect(within(front).getByRole("link", { name: /^Download Front 3\/4 \(/ })).toHaveAttribute("download", result.images[0].fileName);
    // Only that card.
    expect(card("Side profile")).toHaveAttribute("data-version", "studio");

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(front).toHaveAttribute("data-version", "studio");
  });

  it("switches every card to the original and back, and remembers the choice", async () => {
    const user = userEvent.setup();
    const result = studioResult();
    const { unmount } = renderGrid({ result });

    await user.click(within(card("Side profile")).getByRole("button", { name: "Show original" }));
    await user.click(screen.getByRole("radio", { name: "Original" }));
    expect(screen.queryByRole("group", { name: "Studio style" })).not.toBeInTheDocument();
    for (const c of cards()) expect(c).toHaveAttribute("data-version", "original");
    // A studio image can still be flipped on its own; the toggle shows it pressed.
    const toggle = within(card("Front 3/4")).getByRole("button", { name: "Show original" });
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    expect(JSON.parse(window.localStorage.getItem(BACKGROUND_STORAGE_KEY)!)).toEqual({ mode: "original", style: "grey" });

    unmount();
    renderGrid({ result });
    expect(screen.getByRole("radio", { name: "Original" })).toBeChecked();
    expect(card("Front 3/4")).toHaveAttribute("data-version", "original");

    await user.click(screen.getByRole("radio", { name: "Studio" }));
    expect(card("Front 3/4")).toHaveAttribute("data-version", "studio");
    expect(card("Side profile")).toHaveAttribute("data-version", "studio");
  });

  it("changes the style from the picker, by mouse and keyboard", async () => {
    const user = userEvent.setup();
    const result = studioResult();
    renderGrid({ result });
    const front = card("Front 3/4");

    await user.click(screen.getByRole("radio", { name: "Dark" }));
    expect(thumbSrc(front)).toBe(studioThumb(result.images[0], "dark"));
    expect(JSON.parse(window.localStorage.getItem(BACKGROUND_STORAGE_KEY)!)).toEqual({ mode: "studio", style: "dark" });

    // Arrow keys move within the radio group (one tab stop).
    screen.getByRole("radio", { name: "Dark" }).focus();
    await user.keyboard("{ArrowLeft}");
    expect(screen.getByRole("radio", { name: "Light" })).toBeChecked();
    expect(thumbSrc(front)).toBe(studioThumb(result.images[0], "light"));
  });

  it("resets the per-card flips when the switch changes", async () => {
    const user = userEvent.setup();
    renderGrid();
    await user.click(within(card("Front 3/4")).getByRole("button", { name: "Show original" }));
    expect(card("Front 3/4")).toHaveAttribute("data-version", "original");
    await user.click(screen.getByRole("radio", { name: "Light" }));
    expect(card("Front 3/4")).toHaveAttribute("data-version", "studio");
  });

  it("uses a remembered style only when the job offers it", () => {
    window.localStorage.setItem(BACKGROUND_STORAGE_KEY, JSON.stringify({ mode: "studio", style: "dark" }));
    const result = makeResult({ images: [makeStudioImage(1, {}, ["grey", "light"])] });
    renderGrid({ result, studio: makeJobStudio({ styles: ["grey", "light"] }) });
    expect(screen.getByRole("radio", { name: "Grey" })).toBeChecked();
    expect(screen.queryByRole("radio", { name: "Dark" })).not.toBeInTheDocument();
    expect(thumbSrc(cards()[0])).toBe(studioThumb(result.images[0], "grey"));
  });

  it("hides the style picker when there is only one style", () => {
    renderGrid({ result: makeResult({ images: [makeStudioImage(1, {}, ["grey"])] }), studio: makeJobStudio({ styles: ["grey"] }) });
    expect(screen.getByRole("group", { name: "Background" })).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Studio style" })).not.toBeInTheDocument();
  });

  it("works when the browser refuses storage", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    const user = userEvent.setup();
    renderGrid();
    expect(card("Front 3/4")).toHaveAttribute("data-version", "studio");
    await user.click(screen.getByRole("radio", { name: "Original" }));
    expect(card("Front 3/4")).toHaveAttribute("data-version", "original");
  });

  it("offers both ZIPs once the studio photos are done", () => {
    renderGrid();
    expect(screen.getByRole("link", { name: /Download all \(ZIP\)/ })).toHaveAttribute(
      "href",
      `${API_BASE}/v1/domains/cars/jobs/${JOB_ID}/download.zip`,
    );
    expect(screen.getByRole("link", { name: "Download studio photos (ZIP)" })).toHaveAttribute(
      "href",
      `${API_BASE}/v1/domains/cars/jobs/${JOB_ID}/studio.zip`,
    );
  });

  it("the lightbox shows the card's version, shares its toggle and downloads what it shows", async () => {
    const user = userEvent.setup();
    const result = studioResult();
    renderGrid({ result });

    await user.click(within(card("Front 3/4")).getByRole("button", { name: /in full size/ }));
    let dialog = await screen.findByRole("dialog");
    const img = within(dialog).getByRole("img");
    expect(img).toHaveAttribute("src", `${API_BASE}${result.images[0].studio!.variants[0].url}`);
    expect(img).toHaveAccessibleName(/on a studio background \(the car itself is unchanged\)/);
    expect(within(dialog).getByTestId("studio-badge")).toHaveTextContent(STUDIO_BADGE_TEXT);
    const download = within(dialog).getByRole("link", { name: /Download studio photo/ });
    expect(download).toHaveAttribute("href", `${API_BASE}${result.images[0].studio!.variants[0].downloadUrl}`);
    expect(download).toHaveAttribute("download", result.images[0].studio!.variants[0].fileName);

    await user.click(within(dialog).getByRole("button", { name: "Show original" }));
    dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("img")).toHaveAttribute("src", `${API_BASE}${result.images[0].url}`);
    expect(within(dialog).getByRole("button", { name: "Show original" })).toHaveAttribute("aria-pressed", "true");
    expect(within(dialog).getByRole("link", { name: /^Download/ })).toHaveAttribute("download", result.images[0].fileName);

    // The withheld image shows its note in the lightbox too.
    await user.keyboard("{ArrowRight}{ArrowRight}");
    dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("Rear 3/4");
    expect(within(dialog).getByTestId("studio-note")).toHaveTextContent(/^Studio version withheld:/);
    expect(within(dialog).queryByRole("button", { name: "Show original" })).not.toBeInTheDocument();

    await user.keyboard("{Escape}");
    // The flip made in the lightbox holds on the card.
    expect(card("Front 3/4")).toHaveAttribute("data-version", "original");
  });
});

describe("<ResultCard> without studio props", () => {
  it("shows the original even for an image with a studio version", () => {
    const image = makeStudioImage(1);
    render(<ResultCard image={image} apiBaseUrl={API_BASE} onOpen={() => {}} />);
    expect(screen.getByRole("img")).toHaveAttribute("src", `${API_BASE}${image.thumbnailUrl}`);
    expect(screen.getByTestId("studio-original")).toHaveTextContent("Original photo");
    expect(screen.queryByRole("button", { name: "Show original" })).not.toBeInTheDocument();
  });
});
