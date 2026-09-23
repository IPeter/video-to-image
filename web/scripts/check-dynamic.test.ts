import { describe, expect, it } from "vitest";

import { ALLOWED_PRERENDERED, unexpectedPrerenderedRoutes } from "./check-dynamic.mjs";

describe("check-dynamic", () => {
  it("accepts a build that prerendered only Next's own static routes", () => {
    expect(unexpectedPrerenderedRoutes({ routes: { "/_global-error": {}, "/icon.svg": {} }, dynamicRoutes: {} })).toEqual([]);
    expect(ALLOWED_PRERENDERED).toEqual(["/_global-error", "/icon.svg"]);
  });

  it("flags prerendered pages (the layout lost force-dynamic / connection())", () => {
    const manifest = {
      routes: { "/_global-error": {}, "/icon.svg": {}, "/": {}, "/_not-found": {} },
      dynamicRoutes: { "/jobs/[jobId]": {} },
    };
    expect(unexpectedPrerenderedRoutes(manifest)).toEqual(["/", "/_not-found", "/jobs/[jobId]"]);
  });

  it("copes with a manifest without route maps", () => {
    expect(unexpectedPrerenderedRoutes({})).toEqual([]);
  });
});
