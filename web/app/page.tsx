import { UploadForm } from "@/components/upload-form";
import { loadDomain } from "@/lib/domain";

// Upload page. Title, hint and limits come from the domain config (GET /v1/domains/:key),
// so the same build serves every domain.
export default async function UploadPage() {
  const state = await loadDomain();
  if (!state.ok) return null; // the root layout renders the unavailable state

  const { copy, limits, shots } = state.domain;
  const requiredShots = shots.filter((s) => s.required).map((s) => s.label);
  return (
    <div className="mx-auto flex w-full max-w-2xl flex-col gap-8 px-4 py-10 sm:px-6 sm:py-14">
      <div className="flex flex-col gap-3">
        <h1 className="font-heading text-3xl font-semibold tracking-tight text-balance sm:text-4xl">
          {copy.uploadTitle}
        </h1>
        <p className="text-base text-pretty text-muted-foreground">{copy.uploadHint}</p>
      </div>
      <UploadForm limits={limits} />
      {requiredShots.length > 0 && (
        <p className="text-sm text-muted-foreground">
          Make sure the video clearly shows: <span className="text-foreground">{requiredShots.join(", ")}</span>.
        </p>
      )}
    </div>
  );
}
