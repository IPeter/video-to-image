import { afterEach, describe, expect, it, vi } from "vitest";

import { GET as healthz } from "../healthz/route";
import { GET as readyz } from "./route";

function setEnv(env: Record<string, string | undefined>) {
  for (const name of ["DOMAIN_KEY", "NEXT_PUBLIC_DOMAIN_KEY", "API_URL", "NEXT_PUBLIC_API_URL", "API_INTERNAL_URL"]) {
    vi.stubEnv(name, env[name] ?? "");
  }
}

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("GET /api/readyz", () => {
  it("is ready with a valid configuration", async () => {
    setEnv({ DOMAIN_KEY: "cars", API_URL: "http://localhost:4000" });
    const res = readyz();
    expect(res.status).toBe(200);
    expect(res.headers.get("Cache-Control")).toBe("no-store");
    expect(await res.json()).toEqual({ ok: true });
  });

  it.each([
    [{}, ["DOMAIN_KEY is not set", "API_URL is not set"]],
    [{ API_URL: "http://localhost:4000" }, ["DOMAIN_KEY is not set"]],
    [{ DOMAIN_KEY: "cars" }, ["API_URL is not set"]],
    [{ DOMAIN_KEY: "Cars", API_URL: "localhost:4000" }, [expect.stringContaining("DOMAIN_KEY"), expect.stringContaining("API_URL")]],
  ])("is not ready with %j", async (env, problems) => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    setEnv(env);
    const res = readyz();
    expect(res.status).toBe(503);
    expect(res.headers.get("Cache-Control")).toBe("no-store");
    expect(await res.json()).toEqual({ ok: false, problems });
  });

  it("does not call the API", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    setEnv({ DOMAIN_KEY: "cars", API_URL: "http://localhost:1" });
    expect(readyz().status).toBe(200);
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe("GET /api/healthz", () => {
  it("stays live even when the configuration is invalid", async () => {
    setEnv({});
    const res = healthz();
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });
  });
});
