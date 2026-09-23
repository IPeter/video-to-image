import { describe, expect, it } from "vitest";

import {
  elapsedSeconds,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatWait,
  largestResolutionWithin,
  pluralize,
} from "./format";

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [512, "512 B"],
    [1536, "1.5 KB"],
    [5 * 1024 * 1024, "5 MB"],
    [12.34 * 1024 * 1024, "12.3 MB"],
    [524_288_000, "500 MB"],
    [1.5 * 1024 ** 3, "1.5 GB"],
    [-1, ""],
  ])("%d → %s", (n, want) => {
    expect(formatBytes(n)).toBe(want);
  });
});

describe("formatDuration", () => {
  it.each([
    [0, "0:00"],
    [6.9, "0:06"],
    [74.2, "1:14"],
    [3725, "1:02:05"],
    [Number.NaN, ""],
  ])("%s → %s", (s, want) => {
    expect(formatDuration(s)).toBe(want);
  });
});

describe("elapsedSeconds", () => {
  it("counts from the timestamp and clamps clock skew", () => {
    const t = Date.parse("2026-09-23T10:00:00Z");
    expect(elapsedSeconds("2026-09-23T10:00:00Z", t + 42_000)).toBe(42);
    expect(elapsedSeconds("2026-09-23T10:00:00Z", t - 5_000)).toBe(0);
    expect(elapsedSeconds("garbage", t)).toBe(0);
  });
});

describe("formatDateTime", () => {
  it("formats in the given locale", () => {
    const s = formatDateTime("2026-09-24T10:00:00Z", "en-US");
    expect(s).toMatch(/Sep 24, 2026/);
    expect(formatDateTime("nope")).toBe("");
  });
});

describe("pluralize", () => {
  it("handles one and many", () => {
    expect(pluralize(1, "image")).toBe("1 image");
    expect(pluralize(8, "image")).toBe("8 images");
    expect(pluralize(0, "image")).toBe("0 images");
  });
});

describe("largestResolutionWithin", () => {
  it.each([
    [4096 * 2176, "4K", 3840, 2160],
    [3840 * 2160, "4K", 3840, 2160],
    [3840 * 2160 - 1, "1440p", 2560, 1440],
    [7680 * 4320, "8K", 7680, 4320],
    [1920 * 1080, "1080p", 1920, 1080],
  ])("%d px → %s", (px, name, width, height) => {
    expect(largestResolutionWithin(px)).toEqual({ name, width, height });
  });

  it.each([0, -1, Number.NaN, undefined, 1000])("%s → null", (px) => {
    expect(largestResolutionWithin(px)).toBeNull();
  });
});

describe("formatWait", () => {
  it.each([
    [0, "about 1 second"],
    [1, "about 1 second"],
    [44.2, "about 45 seconds"],
    [59, "about 59 seconds"],
    [60, "about 1 minute"],
    [61, "about 2 minutes"],
    [360, "about 6 minutes"],
  ])("%s s → %s", (sec, want) => {
    expect(formatWait(sec)).toBe(want);
  });
});
