import { getRuntimeConfig } from "@/lib/runtime-config";

// GET /api/readyz → READINESS probe. 200 {ok: true} when the runtime configuration
// (DOMAIN_KEY, API_URL, optional API_INTERNAL_URL) is valid; 503 {ok: false, problems}
// otherwise, so a Deployment rolled out with a missing or malformed env var never becomes
// Ready and the old pods keep serving. It deliberately does not call the API: an API
// outage must not take every web pod out of the Service (pages show a friendly
// "temporarily unavailable" state instead). An unknown DOMAIN_KEY is therefore only
// visible in the page logs ("domain: GET … returned HTTP 404 DOMAIN_NOT_FOUND").
export const dynamic = "force-dynamic";

const noStore = { "Cache-Control": "no-store" };

export function GET() {
  const rc = getRuntimeConfig();
  if (!rc.ok) {
    console.error(`readyz: not ready: ${rc.problems.join("; ")}`);
    return Response.json({ ok: false, problems: rc.problems }, { status: 503, headers: noStore });
  }
  return Response.json({ ok: true }, { headers: noStore });
}
