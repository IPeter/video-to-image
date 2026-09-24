import { describe, expect, it } from "vitest";

import { makeImage, makeJobStudio, makeStudioImage, NO_STUDIO, readyStudio } from "@/test/fixtures";

import {
  hasReadyStudio,
  isStudioInProgress,
  originalVersion,
  parseBackgroundPreference,
  resolveBackground,
  serializeBackgroundPreference,
  shownVersion,
  studioNote,
  studioStyleOptions,
  studioVariantFor,
} from "./studio";

describe("isStudioInProgress", () => {
  it("is true only while the studio run is pending or running", () => {
    expect(isStudioInProgress(makeJobStudio({ status: "pending" }))).toBe(true);
    expect(isStudioInProgress(makeJobStudio({ status: "running" }))).toBe(true);
    for (const status of ["none", "done", "failed", "skipped"] as const) {
      expect(isStudioInProgress(makeJobStudio({ status }))).toBe(false);
    }
    expect(isStudioInProgress(null)).toBe(false);
    expect(isStudioInProgress(undefined)).toBe(false);
  });
});

describe("hasReadyStudio", () => {
  it("needs one image with a ready variant", () => {
    expect(hasReadyStudio([])).toBe(false);
    expect(hasReadyStudio([makeImage(1)])).toBe(false);
    expect(hasReadyStudio([makeImage(1, { studio: { status: "pending", reason: "", variants: [] } })])).toBe(false);
    expect(hasReadyStudio([makeImage(1, { studio: { status: "withheld", reason: "r", variants: [] } })])).toBe(false);
    expect(hasReadyStudio([makeImage(1, { studio: { status: "ready", reason: "", variants: [] } })])).toBe(false);
    expect(hasReadyStudio([makeImage(1), makeStudioImage(2)])).toBe(true);
  });
});

describe("studioStyleOptions", () => {
  it("keeps the job's order and drops unknown or repeated styles", () => {
    expect(studioStyleOptions(makeJobStudio({ styles: ["light", "grey", "dark"] }), [])).toEqual(["light", "grey", "dark"]);
    const odd = makeJobStudio({ styles: ["dark", "dark", "neon" as "grey"] });
    expect(studioStyleOptions(odd, [])).toEqual(["dark"]);
  });

  it("falls back to the ready variants' styles when the job lists none", () => {
    const images = [makeImage(1), makeStudioImage(2, {}, ["light", "dark"]), makeStudioImage(3, {}, ["grey", "light"])];
    expect(studioStyleOptions(NO_STUDIO, images)).toEqual(["light", "dark", "grey"]);
    expect(studioStyleOptions(null, [])).toEqual([]);
  });
});

describe("background preference", () => {
  it("round-trips", () => {
    const pref = { mode: "original", style: "dark" } as const;
    expect(parseBackgroundPreference(serializeBackgroundPreference(pref))).toEqual(pref);
    expect(parseBackgroundPreference(serializeBackgroundPreference({ mode: "studio", style: null }))).toEqual({
      mode: "studio",
      style: null,
    });
  });

  it.each([null, undefined, "", "not json", "null", "42", '"studio"', "{}", '{"mode":"sepia"}'])(
    "reads %j as no preference",
    (raw) => {
      expect(parseBackgroundPreference(raw)).toBeNull();
    },
  );

  it("drops an unknown style but keeps the mode", () => {
    expect(parseBackgroundPreference('{"mode":"studio","style":"neon"}')).toEqual({ mode: "studio", style: null });
  });
});

describe("resolveBackground", () => {
  const options = ["light", "grey", "dark"] as const;

  it("defaults to the studio version in the job's default style", () => {
    expect(resolveBackground(null, options, "light")).toEqual({ mode: "studio", style: "light" });
  });

  it("uses the remembered mode and style", () => {
    expect(resolveBackground({ mode: "original", style: "dark" }, options, "light")).toEqual({ mode: "original", style: "dark" });
    expect(resolveBackground({ mode: "studio", style: null }, options, "grey")).toEqual({ mode: "studio", style: "grey" });
  });

  it("falls back when the remembered or default style is not offered", () => {
    expect(resolveBackground({ mode: "studio", style: "dark" }, ["grey", "light"], "grey")).toEqual({ mode: "studio", style: "grey" });
    expect(resolveBackground(null, ["light"], "grey")).toEqual({ mode: "studio", style: "light" });
    expect(resolveBackground(null, ["dark", "grey"], "")).toEqual({ mode: "studio", style: "dark" });
    expect(resolveBackground(null, [], "")).toEqual({ mode: "studio", style: "grey" });
    expect(resolveBackground(null, [], "dark")).toEqual({ mode: "studio", style: "dark" });
  });
});

describe("shownVersion", () => {
  const image = makeStudioImage(1, {}, ["grey", "light"]);
  const [grey, light] = image.studio!.variants;

  it("shows the chosen style's variant with its files", () => {
    const shown = shownVersion(image, { mode: "studio", style: "light" }, false);
    expect(shown).toEqual({
      variant: light,
      hasStudio: true,
      url: light.url,
      thumbnailUrl: light.thumbnailUrl,
      downloadUrl: light.downloadUrl,
      fileName: light.fileName,
      width: light.width,
      height: light.height,
    });
    expect(shown.fileName).toBe("cars_QXA4B5C6_1_front_three_quarter_00-06_studio-light.jpg");
  });

  it("falls back to the image's default variant for a style it lacks", () => {
    expect(studioVariantFor(image, "dark")).toBe(grey);
    expect(shownVersion(image, { mode: "studio", style: "dark" }, false).variant).toBe(grey);
  });

  it("shows the original when asked, or when there is no studio version", () => {
    const original = shownVersion(image, { mode: "studio", style: "grey" }, true);
    expect(original).toEqual({ ...originalVersion(image), hasStudio: true });
    expect(original.url).toBe(image.url);
    expect(original.fileName).toBe(image.fileName);

    const plain = makeImage(2);
    expect(shownVersion(plain, { mode: "studio", style: "grey" }, false)).toEqual({ ...originalVersion(plain), hasStudio: false });
    const withheld = makeImage(3, { studio: { status: "withheld", reason: "r", variants: [] } });
    expect(shownVersion(withheld, { mode: "studio", style: "grey" }, false).variant).toBeNull();
  });

  it("the switch's mode alone does not pick the version (the caller combines it with the card's flip)", () => {
    // ResultGrid passes showOriginal = (mode === "original") XOR flipped.
    expect(shownVersion(image, { mode: "original", style: "grey" }, false).variant).toBe(grey);
  });
});

describe("studioNote", () => {
  it("explains withheld and failed images", () => {
    expect(studioNote({ status: "withheld", reason: " The car touches the right edge of the photo. " })).toBe(
      "Studio version withheld: The car touches the right edge of the photo.",
    );
    expect(studioNote({ status: "withheld", reason: "" })).toBe("Studio version withheld.");
    expect(studioNote({ status: "failed", reason: "Something went wrong." })).toBe("Studio version unavailable: Something went wrong.");
    expect(studioNote({ status: "failed", reason: "" })).toBe("Studio version unavailable.");
  });

  it("has nothing to say otherwise", () => {
    expect(studioNote({ status: "pending", reason: "" })).toBeNull();
    expect(studioNote(readyStudio(makeImage(1)))).toBeNull();
    expect(studioNote(null)).toBeNull();
  });
});
