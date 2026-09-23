import { getRuntimeConfig, toPublicRuntimeConfig } from "@/lib/runtime-config";

// GET /api/config → {domainKey, apiUrl} from the container's environment at request
// time, so one image serves every domain (plan/10-deployment-k3s.md). Pages get the same
// values through <Providers>; this route is for anything that runs outside React.
export const dynamic = "force-dynamic";

const noStore = { "Cache-Control": "no-store" };

export function GET() {
  const rc = getRuntimeConfig();
  if (!rc.ok) {
    console.error(`config: ${rc.problems.join("; ")}`);
    return Response.json(
      { error: { code: "INTERNAL_ERROR", message: "The web server is not configured." } },
      { status: 503, headers: noStore },
    );
  }
  return Response.json(toPublicRuntimeConfig(rc.config), { headers: noStore });
}
