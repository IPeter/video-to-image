// GET /api/healthz → 200 {ok: true} while the Next server process answers. LIVENESS probe
// only: it checks neither the configuration (that is /api/readyz) nor the API, so a
// misconfigured pod or an API outage never gets web pods restarted in a loop.
export const dynamic = "force-dynamic";

export function GET() {
  return Response.json({ ok: true }, { headers: { "Cache-Control": "no-store" } });
}
