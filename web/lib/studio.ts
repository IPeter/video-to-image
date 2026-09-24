import type { ImageStudio, JobStudio, ResultImage, StudioStyle, StudioVariant } from "@/lib/api";

// Studio backgrounds (GET …/jobs/{id} → studio, result.images[].studio): exterior photos
// are cut out and put on a studio backdrop without touching a car pixel. They are made
// after the job completed, so they appear on the results page while it is open. The
// original always stays available; these helpers decide which version is on screen.

/** Badge text of a studio image. */
export const STUDIO_BADGE_TEXT = "Studio background · car unchanged";

/** Tooltip and screen-reader text of the badge. */
export const STUDIO_BADGE_DESCRIPTION =
  "Only the background and the floor shadow are new: every pixel of the car is from the original photo, scratches and dirt included.";

/** Style names as the switch shows them. */
export const STUDIO_STYLE_LABELS: Readonly<Record<StudioStyle, string>> = {
  grey: "Grey",
  light: "Light",
  dark: "Dark",
};

/** Every style, in the API's default order (the job's own order wins; see studioStyleOptions). */
export const STUDIO_STYLES: readonly StudioStyle[] = ["grey", "light", "dark"];

function isStudioStyle(value: unknown): value is StudioStyle {
  return typeof value === "string" && (STUDIO_STYLES as readonly string[]).includes(value);
}

/** Studio versions are still being made after the job completed: keep polling the job. */
export function isStudioInProgress(studio: Pick<JobStudio, "status"> | null | undefined): boolean {
  return studio?.status === "pending" || studio?.status === "running";
}

/** The image's studio outcome when it has ready variants, else null. */
function readyStudio(image: Pick<ResultImage, "studio">): ImageStudio | null {
  const s = image.studio;
  return s && s.status === "ready" && s.variants.length > 0 ? s : null;
}

/** At least one image has a studio version to show (the switch appears then). */
export function hasReadyStudio(images: readonly Pick<ResultImage, "studio">[]): boolean {
  return images.some((img) => readyStudio(img) !== null);
}

/**
 * The styles the switch offers: the job's list (display order, default first); when it
 * is empty (an older API), the styles of the ready variants in first-seen order.
 */
export function studioStyleOptions(
  studio: Pick<JobStudio, "styles"> | null | undefined,
  images: readonly Pick<ResultImage, "studio">[],
): StudioStyle[] {
  const out: StudioStyle[] = [];
  const add = (s: unknown) => {
    if (isStudioStyle(s) && !out.includes(s)) out.push(s);
  };
  (studio?.styles ?? []).forEach(add);
  if (out.length === 0) {
    for (const img of images) readyStudio(img)?.variants.forEach((v) => add(v.style));
  }
  return out;
}

export type BackgroundMode = "original" | "studio";

/** The remembered choice of the switch (per browser, see hooks/use-background-preference). */
export interface BackgroundPreference {
  mode: BackgroundMode;
  /** null → the job's default style. */
  style: StudioStyle | null;
}

/** What the switch shows for this job. */
export interface BackgroundChoice {
  mode: BackgroundMode;
  style: StudioStyle;
}

/** Reads a stored preference; anything malformed reads as none. */
export function parseBackgroundPreference(raw: string | null | undefined): BackgroundPreference | null {
  if (!raw) return null;
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null) return null;
  const { mode, style } = value as { mode?: unknown; style?: unknown };
  if (mode !== "original" && mode !== "studio") return null;
  return { mode, style: isStudioStyle(style) ? style : null };
}

export function serializeBackgroundPreference(pref: BackgroundPreference): string {
  return JSON.stringify({ mode: pref.mode, style: pref.style });
}

/**
 * The switch state for a job: the remembered choice, else the studio version in the
 * job's default style. A remembered style the job does not offer falls back to the
 * default (then the first offered style).
 */
export function resolveBackground(
  pref: BackgroundPreference | null,
  options: readonly StudioStyle[],
  defaultStyle: StudioStyle | "" | null | undefined,
): BackgroundChoice {
  const fallback: StudioStyle =
    defaultStyle && (options.length === 0 || options.includes(defaultStyle)) ? defaultStyle : (options[0] ?? "grey");
  const style = pref?.style && options.includes(pref.style) ? pref.style : fallback;
  return { mode: pref?.mode ?? "studio", style };
}

/**
 * The studio variant an image shows for a style: that style's, else the image's first
 * (the default style). Null when the image has no ready studio version.
 */
export function studioVariantFor(image: Pick<ResultImage, "studio">, style: StudioStyle): StudioVariant | null {
  const s = readyStudio(image);
  if (!s) return null;
  return s.variants.find((v) => v.style === style) ?? s.variants[0];
}

/** The version of an image that a card or the lightbox shows, with its files. */
export interface ShownVersion {
  /** The studio variant on screen; null → the original photo. */
  variant: StudioVariant | null;
  /** The image has a studio version, so the "Show original" toggle applies. */
  hasStudio: boolean;
  url: string;
  thumbnailUrl: string;
  downloadUrl: string;
  fileName: string;
  width: number;
  height: number;
}

/** The original photo's files. */
export function originalVersion(image: ResultImage): ShownVersion {
  return {
    variant: null,
    hasStudio: readyStudio(image) !== null,
    url: image.url,
    thumbnailUrl: image.thumbnailUrl,
    downloadUrl: image.downloadUrl,
    fileName: image.fileName,
    width: image.width,
    height: image.height,
  };
}

/**
 * Which version is on screen: the studio variant of the chosen style when the switch is
 * on Studio, unless `showOriginal` (the card's own "Show original" toggle); the original
 * for images without a studio version.
 */
export function shownVersion(image: ResultImage, choice: BackgroundChoice, showOriginal: boolean): ShownVersion {
  const original = originalVersion(image);
  const variant = studioVariantFor(image, choice.style);
  if (!variant || showOriginal) return original;
  return {
    variant,
    hasStudio: true,
    url: variant.url,
    thumbnailUrl: variant.thumbnailUrl,
    downloadUrl: variant.downloadUrl,
    fileName: variant.fileName,
    width: variant.width,
    height: variant.height,
  };
}

/**
 * The muted note of an image whose studio version was withheld or failed ("Studio version
 * withheld: <reason>"); null for every other state.
 */
export function studioNote(studio: Pick<ImageStudio, "status" | "reason"> | null | undefined): string | null {
  const reason = studio?.reason?.trim() ?? "";
  switch (studio?.status) {
    case "withheld":
      return reason ? `Studio version withheld: ${reason}` : "Studio version withheld.";
    case "failed":
      return reason ? `Studio version unavailable: ${reason}` : "Studio version unavailable.";
    default:
      return null;
  }
}
