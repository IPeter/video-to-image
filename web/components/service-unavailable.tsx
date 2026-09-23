import { CloudOff } from "lucide-react";

import { ReloadButton } from "@/components/reload-button";

export interface ServiceUnavailableProps {
  /** Operator-facing details, only passed in development. */
  details?: string[];
}

/** Friendly full-page state for a misconfigured web server or an unreachable API. */
export function ServiceUnavailable({ details }: ServiceUnavailableProps) {
  return (
    <section
      aria-labelledby="unavailable-title"
      className="mx-auto flex max-w-lg flex-col items-center gap-4 px-4 py-20 text-center"
    >
      <CloudOff aria-hidden="true" className="size-10 text-muted-foreground" />
      <h1 id="unavailable-title" className="font-heading text-2xl font-semibold tracking-tight">
        Service temporarily unavailable
      </h1>
      <p className="text-muted-foreground">
        We can&apos;t load this page right now. Please try again in a few minutes.
      </p>
      <ReloadButton />
      {details && details.length > 0 && (
        <div className="mt-6 w-full rounded-lg border border-dashed p-4 text-left text-xs text-muted-foreground">
          <p className="mb-2 font-medium text-foreground">Development details</p>
          <ul className="list-disc space-y-1 pl-4">
            {details.map((d) => (
              <li key={d}>{d}</li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}
