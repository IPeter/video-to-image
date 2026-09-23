import { describe, expect, it } from "vitest";

import { parseRuntimeConfig, toPublicRuntimeConfig } from "./runtime-config-parse";

describe("parseRuntimeConfig", () => {
  it("reads DOMAIN_KEY and API_URL, defaulting the internal URL", () => {
    expect(parseRuntimeConfig({ DOMAIN_KEY: "cars", API_URL: "http://localhost:4000/" })).toEqual({
      ok: true,
      config: { domainKey: "cars", apiUrl: "http://localhost:4000", apiInternalUrl: "http://localhost:4000" },
    });
  });

  it("uses API_INTERNAL_URL for server-side fetches", () => {
    const r = parseRuntimeConfig({
      DOMAIN_KEY: "cars",
      API_URL: "https://api.cars.example.com",
      API_INTERNAL_URL: "http://v2i-api.v2i.svc:4000",
    });
    expect(r).toMatchObject({
      ok: true,
      config: { apiUrl: "https://api.cars.example.com", apiInternalUrl: "http://v2i-api.v2i.svc:4000" },
    });
  });

  it("keeps a path prefix on the API URL", () => {
    expect(parseRuntimeConfig({ DOMAIN_KEY: "cars", API_URL: "https://example.com/api/" })).toMatchObject({
      config: { apiUrl: "https://example.com/api" },
    });
  });

  it("falls back to the NEXT_PUBLIC_ names for local development", () => {
    expect(
      parseRuntimeConfig({ NEXT_PUBLIC_DOMAIN_KEY: "cars", NEXT_PUBLIC_API_URL: "http://localhost:4000" }),
    ).toMatchObject({ ok: true, config: { domainKey: "cars", apiUrl: "http://localhost:4000" } });
  });

  it("prefers the unprefixed names", () => {
    expect(
      parseRuntimeConfig({
        DOMAIN_KEY: "homes",
        NEXT_PUBLIC_DOMAIN_KEY: "cars",
        API_URL: "http://a:1",
        NEXT_PUBLIC_API_URL: "http://b:2",
      }),
    ).toMatchObject({ config: { domainKey: "homes", apiUrl: "http://a:1" } });
  });

  it("reports every missing variable", () => {
    const r = parseRuntimeConfig({});
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.problems).toEqual(["DOMAIN_KEY is not set", "API_URL is not set"]);
  });

  it.each([
    [{ DOMAIN_KEY: "Cars", API_URL: "http://x" }, "DOMAIN_KEY must be lower-case"],
    [{ DOMAIN_KEY: "../etc", API_URL: "http://x" }, "DOMAIN_KEY must be lower-case"],
    [{ DOMAIN_KEY: "cars", API_URL: "localhost:4000" }, "API_URL must be an http(s) URL"],
    [{ DOMAIN_KEY: "cars", API_URL: "ftp://x" }, "API_URL must be an http(s) URL"],
    [{ DOMAIN_KEY: "cars", API_URL: "http://x?y=1" }, "API_URL must be an http(s) URL"],
    [{ DOMAIN_KEY: "cars", API_URL: "http://x", API_INTERNAL_URL: "nope" }, "API_INTERNAL_URL must be"],
  ])("rejects %j", (env, problem) => {
    const r = parseRuntimeConfig(env);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.problems.join("\n")).toContain(problem);
  });

  it("trims whitespace", () => {
    expect(parseRuntimeConfig({ DOMAIN_KEY: " cars ", API_URL: " http://localhost:4000 " })).toMatchObject({ ok: true });
  });

  it("exposes only the public fields", () => {
    expect(
      toPublicRuntimeConfig({ domainKey: "cars", apiUrl: "http://a", apiInternalUrl: "http://internal" }),
    ).toEqual({ domainKey: "cars", apiUrl: "http://a" });
  });
});
