// Small, locale-aware formatting helpers shared by the upload and job pages.

const MB = 1024 * 1024;
const GB = 1024 * MB;

function trimNumber(n: number, maxFractionDigits: number): string {
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: maxFractionDigits }).format(n);
}

/** 524288000 → "500 MB"; 1536 → "1.5 KB". Binary units, labelled the way users expect. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "";
  if (bytes >= GB) return `${trimNumber(bytes / GB, 1)} GB`;
  if (bytes >= MB) return `${trimNumber(bytes / MB, bytes >= 100 * MB ? 0 : 1)} MB`;
  if (bytes >= 1024) return `${trimNumber(bytes / 1024, 1)} KB`;
  return `${bytes} B`;
}

/** 74.2 → "1:14"; 3725 → "1:02:05". Rounds down to whole seconds. */
export function formatDuration(totalSeconds: number): string {
  if (!Number.isFinite(totalSeconds) || totalSeconds < 0) return "";
  const s = Math.floor(totalSeconds);
  const hours = Math.floor(s / 3600);
  const minutes = Math.floor((s % 3600) / 60);
  const seconds = s % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`;
}

/** Elapsed time between an ISO timestamp and now, clamped at zero (clock skew). */
export function elapsedSeconds(sinceIso: string, nowMs: number): number {
  const since = Date.parse(sinceIso);
  if (Number.isNaN(since)) return 0;
  return Math.max(0, (nowMs - since) / 1000);
}

/** An ISO timestamp in the user's locale, e.g. "Sep 24, 2026, 10:00 AM". */
export function formatDateTime(iso: string, locale?: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }).format(date);
}

/** "1 image", "8 images". */
export function pluralize(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

/** A common 16:9 video resolution, largest first. */
export interface NamedResolution {
  name: string;
  width: number;
  height: number;
}

const STANDARD_RESOLUTIONS: readonly NamedResolution[] = [
  { name: "8K", width: 7680, height: 4320 },
  { name: "4K", width: 3840, height: 2160 },
  { name: "1440p", width: 2560, height: 1440 },
  { name: "1080p", width: 1920, height: 1080 },
  { name: "720p", width: 1280, height: 720 },
  { name: "480p", width: 854, height: 480 },
];

/**
 * The largest common 16:9 resolution with at most `maxPixels` pixels (width × height),
 * so a pixel budget can be phrased the way people export video: 8912896 → 4K 3840×2160.
 * Null when the budget is unknown or below 480p.
 */
export function largestResolutionWithin(maxPixels: number | undefined): NamedResolution | null {
  if (!maxPixels || !Number.isFinite(maxPixels) || maxPixels <= 0) return null;
  return STANDARD_RESOLUTIONS.find((r) => r.width * r.height <= maxPixels) ?? null;
}

/** A wait in plain language, rounded up: 45 → "about 45 seconds", 301 → "about 6 minutes". */
export function formatWait(seconds: number): string {
  const s = Math.max(1, Math.ceil(seconds));
  if (s < 60) return `about ${pluralize(s, "second")}`;
  return `about ${pluralize(Math.ceil(s / 60), "minute")}`;
}
