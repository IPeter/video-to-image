import { describe, expect, it } from "vitest";

import {
  checkDuration,
  checkFileSize,
  checkFileType,
  checkMaxPixels,
  checkResolution,
  hasBlockingIssue,
  looksLikeVideo,
  precheckFile,
  precheckMetadata,
  type UploadLimits,
} from "./upload-checks";

const limits: UploadLimits = {
  maxUploadBytes: 524_288_000,
  minDurationSec: 5,
  maxDurationSec: 180,
  minShortSidePx: 480,
  maxPixels: 4096 * 2176,
};

describe("file type", () => {
  it.each([
    [{ name: "a.mp4", type: "video/mp4" }, true],
    [{ name: "a.mov", type: "video/quicktime" }, true],
    [{ name: "A.MOV", type: "" }, true],
    [{ name: "clip.webm", type: "" }, true],
    [{ name: "photo.jpg", type: "image/jpeg" }, false],
    [{ name: "notes", type: "" }, false],
    [{ name: "video.mp4", type: "application/pdf" }, false],
  ])("looksLikeVideo(%j) = %s", (file, want) => {
    expect(looksLikeVideo(file)).toBe(want);
    expect(checkFileType(file) === null).toBe(want);
  });

  it("blocks non-videos with UNSUPPORTED_FORMAT", () => {
    expect(checkFileType({ name: "photo.jpg", type: "image/jpeg" })).toMatchObject({
      severity: "error",
      code: "UNSUPPORTED_FORMAT",
    });
  });
});

describe("checkFileSize", () => {
  it("accepts files up to the limit", () => {
    expect(checkFileSize(1, limits)).toBeNull();
    expect(checkFileSize(limits.maxUploadBytes, limits)).toBeNull();
  });

  it("blocks files over the limit", () => {
    const issue = checkFileSize(limits.maxUploadBytes + 1, limits);
    expect(issue).toMatchObject({ severity: "error", code: "UPLOAD_TOO_LARGE" });
    expect(issue?.message).toContain("500 MB");
  });

  it("blocks empty files", () => {
    expect(checkFileSize(0, limits)).toMatchObject({ severity: "error", code: "EMPTY_FILE" });
  });

  it("ignores a zero limit", () => {
    expect(checkFileSize(10 ** 12, { maxUploadBytes: 0 })).toBeNull();
  });
});

describe("checkDuration", () => {
  it.each([
    [5, null],
    [74.2, null],
    [180, null],
    [180.4, "TOO_LONG"],
    [600, "TOO_LONG"],
    [4.9, "TOO_SHORT"],
    [0.5, "TOO_SHORT"],
    [0, null], // unknown
    [Number.NaN, null],
    [Number.POSITIVE_INFINITY, null], // live streams / unknown length
  ])("%s s → %s", (duration, code) => {
    const issue = checkDuration(duration, limits);
    if (code === null) {
      expect(issue).toBeNull();
    } else {
      expect(issue).toMatchObject({ severity: "warning", code });
      expect(issue?.message).toContain("between 5 and 180 seconds");
    }
  });

  it("phrases the range without a minimum", () => {
    expect(checkDuration(200, { minDurationSec: 0, maxDurationSec: 180 })?.message).toContain("at most 180 seconds");
  });
});

describe("checkResolution", () => {
  it.each([
    [1920, 1080, null],
    [1080, 1920, null], // portrait: the short side counts
    [854, 480, null],
    [640, 360, "RESOLUTION_TOO_LOW"],
    [360, 640, "RESOLUTION_TOO_LOW"],
    [0, 0, null], // unknown
  ])("%i×%i → %s", (w, h, code) => {
    const issue = checkResolution(w, h, limits);
    if (code === null) expect(issue).toBeNull();
    else expect(issue).toMatchObject({ severity: "warning", code });
  });

  it("is disabled without a minimum", () => {
    expect(checkResolution(100, 100, { minShortSidePx: 0 })).toBeNull();
  });
});

describe("checkMaxPixels", () => {
  it.each([
    [3840, 2160, null],
    [4096, 2160, null],
    [4096, 2176, null], // exactly the budget
    [2176, 4096, null], // portrait
    [4096, 2304, "RESOLUTION_TOO_HIGH"],
    [7680, 4320, "RESOLUTION_TOO_HIGH"],
    [0, 0, null], // unknown
  ])("%i×%i → %s", (w, h, code) => {
    const issue = checkMaxPixels(w, h, limits);
    if (code === null) expect(issue).toBeNull();
    else expect(issue).toMatchObject({ severity: "warning", code });
  });

  it("names the largest common resolution that fits", () => {
    expect(checkMaxPixels(7680, 4320, limits)?.message).toBe(
      "This video is 7680×4320. We can process up to about 3840×2160 (4K), so it will probably be refused. Export it at 4K or lower first.",
    );
  });

  it("falls back to megapixels for an unusual budget", () => {
    expect(checkMaxPixels(1000, 1000, { maxPixels: 250_000 })?.message).toBe(
      "This video is 1000×1000. We can process up to 0.3 megapixels, so it will probably be refused.",
    );
  });

  it("is disabled when the API sends no pixel budget", () => {
    expect(checkMaxPixels(7680, 4320, {})).toBeNull();
    expect(checkMaxPixels(7680, 4320, { maxPixels: 0 })).toBeNull();
  });
});

describe("precheck", () => {
  it("combines blocking file checks", () => {
    const issues = precheckFile({ name: "x.jpg", type: "image/jpeg", size: limits.maxUploadBytes + 1 }, limits);
    expect(issues.map((i) => i.code)).toEqual(["UNSUPPORTED_FORMAT", "UPLOAD_TOO_LARGE"]);
    expect(hasBlockingIssue(issues)).toBe(true);
  });

  it("passes a good file", () => {
    expect(precheckFile({ name: "car.mp4", type: "video/mp4", size: 50_000_000 }, limits)).toEqual([]);
  });

  it("metadata problems only warn", () => {
    const issues = precheckMetadata({ durationSec: 3, width: 640, height: 360 }, limits);
    expect(issues.map((i) => i.code)).toEqual(["TOO_SHORT", "RESOLUTION_TOO_LOW"]);
    expect(hasBlockingIssue(issues)).toBe(false);

    const tooBig = precheckMetadata({ durationSec: 600, width: 7680, height: 4320 }, limits);
    expect(tooBig.map((i) => i.code)).toEqual(["TOO_LONG", "RESOLUTION_TOO_HIGH"]);
    expect(hasBlockingIssue(tooBig)).toBe(false);
  });
});
