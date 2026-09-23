import { afterEach, describe, expect, it, vi } from "vitest";

import { carsDomain } from "@/test/fixtures";

import { DOMAIN_REVALIDATE_SECONDS, fetchDomainConfig } from "./domain";

const config = { domainKey: "cars", apiUrl: "https://api.example.com", apiInternalUrl: "http://v2i-api:4000" };

afterEach(() => {
  vi.restoreAllMocks();
});

function quiet() {
  vi.spyOn(console, "error").mockImplementation(() => {});
}

describe("fetchDomainConfig", () => {
  it("fetches the public config from the internal URL with revalidation", async () => {
    const fetchImpl = vi.fn(async () => Response.json(carsDomain));
    const r = await fetchDomainConfig(config, fetchImpl);
    expect(r).toEqual({ ok: true, config, domain: carsDomain });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit & { next?: { revalidate?: number } }];
    expect(url).toBe("http://v2i-api:4000/v1/domains/cars");
    expect(init.next).toEqual({ revalidate: DOMAIN_REVALIDATE_SECONDS });
    expect(init.signal).toBeInstanceOf(AbortSignal);
  });

  it("reports an unreachable API as unavailable", async () => {
    quiet();
    const r = await fetchDomainConfig(config, async () => {
      throw new TypeError("fetch failed");
    });
    expect(r).toMatchObject({ ok: false, reason: "unavailable", domainKey: "cars", status: null, code: null });
  });

  it("reports an unknown domain with its code", async () => {
    quiet();
    const r = await fetchDomainConfig(config, async () =>
      Response.json({ error: { code: "DOMAIN_NOT_FOUND", message: "no" } }, { status: 404 }),
    );
    expect(r).toMatchObject({ ok: false, reason: "unavailable", status: 404, code: "DOMAIN_NOT_FOUND" });
  });

  it("rejects a malformed body", async () => {
    quiet();
    const r = await fetchDomainConfig(config, async () => new Response("<html>", { status: 200 }));
    expect(r).toMatchObject({ ok: false, reason: "unavailable", status: 200 });
    const r2 = await fetchDomainConfig(config, async () => Response.json({ key: "cars" }));
    expect(r2).toMatchObject({ ok: false, reason: "unavailable" });
  });
});
