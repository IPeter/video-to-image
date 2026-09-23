// Shared settings for playwright.config.ts and the specs.

/** Browser-facing API base URL the web app is started with (and the specs probe). */
export const API_URL = (process.env.API_URL ?? "http://localhost:4000").replace(/\/+$/, "");
/** Domain served by the web app under test. */
export const DOMAIN_KEY = process.env.DOMAIN_KEY ?? "cars";
/** The cars domain allows the http://localhost:3000 origin: keep the host "localhost". */
export const WEB_PORT = Number(process.env.E2E_WEB_PORT ?? 3000);
export const WEB_URL = `http://localhost:${WEB_PORT}`;
