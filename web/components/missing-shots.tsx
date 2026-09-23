import { Camera } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { ShotRef } from "@/lib/api";

/** Callout listing required shots the video did not show clearly, with a re-shoot hint. */
export function MissingShots({ shots }: { shots: readonly ShotRef[] }) {
  if (shots.length === 0) return null;
  return (
    <Alert role="note" data-testid="missing-shots" className="border-amber-500/40 bg-amber-50 text-amber-950 dark:bg-amber-950/30 dark:text-amber-50">
      <Camera aria-hidden="true" />
      <AlertTitle>{shots.length === 1 ? "One shot is missing" : `${shots.length} shots are missing`}</AlertTitle>
      <AlertDescription className="text-amber-950/90 dark:text-amber-50/90 [&_p:not(:last-child)]:mb-0">
        <p>We could not find a clear view of:</p>
        <ul className="my-1.5 list-disc pl-5">
          {shots.map((s) => (
            <li key={s.id}>{s.label}</li>
          ))}
        </ul>
        <p>To get these, film them slowly and steadily, then upload the video again.</p>
      </AlertDescription>
    </Alert>
  );
}
