import type { ErrorCode, PublicLimits } from "./api";
import { formatBytes, formatDuration, largestResolutionWithin } from "./format";

// Client-side pre-checks before an upload. They exist for fast feedback only; the server
// re-checks everything with ffprobe. Size and file type block the upload (the server would
// refuse it after the user waited for the transfer); duration and resolution only warn,
// because browsers cannot read every container/codec the server accepts.

export type CheckSeverity = "error" | "warning";

export interface CheckIssue {
  severity: CheckSeverity;
  /** The API error code this issue anticipates, for consistent copy and tests. */
  code: ErrorCode | "EMPTY_FILE";
  message: string;
}

export type UploadLimits = Pick<
  PublicLimits,
  "maxUploadBytes" | "minDurationSec" | "maxDurationSec" | "minShortSidePx" | "maxPixels"
>;

/** Metadata read in the browser from a hidden <video> element. */
export interface VideoMetadata {
  durationSec: number;
  width: number;
  height: number;
}

const VIDEO_EXTENSIONS = ["mp4", "mov", "m4v", "webm", "mkv", "avi", "3gp", "mpg", "mpeg", "mts", "m2ts"];

function extension(name: string): string {
  const dot = name.lastIndexOf(".");
  return dot >= 0 ? name.slice(dot + 1).toLowerCase() : "";
}

/** True when the file claims to be a video (MIME type), or has no type but a video extension. */
export function looksLikeVideo(file: { name: string; type: string }): boolean {
  if (file.type) return file.type.toLowerCase().startsWith("video/");
  return VIDEO_EXTENSIONS.includes(extension(file.name));
}

export function checkFileType(file: { name: string; type: string }): CheckIssue | null {
  if (looksLikeVideo(file)) return null;
  return {
    severity: "error",
    code: "UNSUPPORTED_FORMAT",
    message: "This file does not look like a video. Please choose an MP4 or MOV video file.",
  };
}

export function checkFileSize(sizeBytes: number, limits: Pick<UploadLimits, "maxUploadBytes">): CheckIssue | null {
  if (sizeBytes <= 0) {
    return { severity: "error", code: "EMPTY_FILE", message: "This file is empty. Please choose another video." };
  }
  if (limits.maxUploadBytes > 0 && sizeBytes > limits.maxUploadBytes) {
    return {
      severity: "error",
      code: "UPLOAD_TOO_LARGE",
      message: `The file is ${formatBytes(sizeBytes)}, larger than the ${formatBytes(limits.maxUploadBytes)} limit. Try a shorter clip or export it at a lower resolution.`,
    };
  }
  return null;
}

export function checkDuration(
  durationSec: number,
  limits: Pick<UploadLimits, "minDurationSec" | "maxDurationSec">,
): CheckIssue | null {
  if (!Number.isFinite(durationSec) || durationSec <= 0) return null; // unknown: let the server decide
  const range =
    limits.minDurationSec > 0
      ? `between ${limits.minDurationSec} and ${limits.maxDurationSec} seconds`
      : `at most ${limits.maxDurationSec} seconds`;
  if (limits.maxDurationSec > 0 && durationSec > limits.maxDurationSec) {
    return {
      severity: "warning",
      code: "TOO_LONG",
      message: `This video is ${formatDuration(durationSec)} long. Videos must be ${range} long, so it will probably be refused.`,
    };
  }
  if (limits.minDurationSec > 0 && durationSec < limits.minDurationSec) {
    return {
      severity: "warning",
      code: "TOO_SHORT",
      message: `This video is only ${durationSec < 1 ? "under a second" : `${Math.floor(durationSec)} seconds`} long. Videos must be ${range} long, so it will probably be refused.`,
    };
  }
  return null;
}

export function checkResolution(
  width: number,
  height: number,
  limits: Pick<UploadLimits, "minShortSidePx">,
): CheckIssue | null {
  if (!(width > 0) || !(height > 0) || !(limits.minShortSidePx > 0)) return null;
  const shortSide = Math.min(width, height);
  if (shortSide >= limits.minShortSidePx) return null;
  return {
    severity: "warning",
    code: "RESOLUTION_TOO_LOW",
    message: `This video is ${width}×${height}. Videos need at least ${limits.minShortSidePx} pixels on the shorter side, so it will probably be refused.`,
  };
}

/**
 * Warns when width × height exceeds the domain's pixel budget (the server answers
 * RESOLUTION_TOO_HIGH). Only a warning: the browser may report a different size than
 * ffprobe (rotation, anamorphic pixels). Disabled when the API sends no maxPixels.
 */
export function checkMaxPixels(
  width: number,
  height: number,
  limits: Partial<Pick<UploadLimits, "maxPixels">>,
): CheckIssue | null {
  const maxPixels = limits.maxPixels ?? 0;
  if (!(width > 0) || !(height > 0) || !(maxPixels > 0)) return null;
  if (width * height <= maxPixels) return null;
  const within = largestResolutionWithin(maxPixels);
  const limit = within
    ? `We can process up to about ${within.width}×${within.height} (${within.name})`
    : `We can process up to ${trimMegapixels(maxPixels)} megapixels`;
  return {
    severity: "warning",
    code: "RESOLUTION_TOO_HIGH",
    message: `This video is ${width}×${height}. ${limit}, so it will probably be refused.${within ? ` Export it at ${within.name} or lower first.` : ""}`,
  };
}

function trimMegapixels(pixels: number): string {
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 }).format(pixels / 1_000_000);
}

/** Blocking checks that only need the File (run immediately on pick). */
export function precheckFile(file: { name: string; type: string; size: number }, limits: UploadLimits): CheckIssue[] {
  return [checkFileType(file), checkFileSize(file.size, limits)].filter((i): i is CheckIssue => i !== null);
}

/** Advisory checks on metadata the browser managed to read. */
export function precheckMetadata(meta: VideoMetadata, limits: UploadLimits): CheckIssue[] {
  return [
    checkDuration(meta.durationSec, limits),
    checkResolution(meta.width, meta.height, limits),
    checkMaxPixels(meta.width, meta.height, limits),
  ].filter((i): i is CheckIssue => i !== null);
}

export function hasBlockingIssue(issues: readonly CheckIssue[]): boolean {
  return issues.some((i) => i.severity === "error");
}
