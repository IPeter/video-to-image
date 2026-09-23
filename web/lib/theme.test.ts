import { readFileSync } from "node:fs";
import path from "node:path";

import { describe, expect, it } from "vitest";

import { THEMED_DOMAINS, themeAttributes, themeForDomain } from "./theme";

describe("themeForDomain", () => {
  it.each([
    ["cars", "cars"],
    ["CARS", "cars"],
    ["homes", "default"],
    ["", "default"],
    [null, "default"],
    [undefined, "default"],
  ])("%s → %s", (key, want) => {
    expect(themeForDomain(key)).toBe(want);
    expect(themeAttributes(key)).toEqual({ "data-domain": want });
  });

  it("every themed domain has a CSS variable set in globals.css", () => {
    const css = readFileSync(path.join(__dirname, "..", "app", "globals.css"), "utf8");
    for (const key of THEMED_DOMAINS) {
      expect(css).toContain(`[data-domain="${key}"]`);
    }
  });
});
