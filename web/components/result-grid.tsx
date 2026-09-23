"use client";

import { Archive, ImageOff } from "lucide-react";
import Link from "next/link";
import { useState, type ReactNode } from "react";

import { DeleteJobButton } from "@/components/delete-job-button";
import { Lightbox } from "@/components/lightbox";
import { MissingShots } from "@/components/missing-shots";
import { ResultCard } from "@/components/result-card";
import { Button } from "@/components/ui/button";
import { UsageSummary } from "@/components/usage-summary";
import { resolveApiUrl, type JobResult, type JobUsage } from "@/lib/api";
import { formatDateTime, pluralize } from "@/lib/format";

/** Id of the results <h1>, the focus target when processing finishes (see JobView). */
export const RESULTS_HEADING_ID = "job-results-title";

export interface ResultGridProps {
  result: JobResult;
  /** ISO timestamp after which the files are deleted. */
  expiresAt: string;
  /** Browser-facing API base URL; every URL in `result` is relative to it. */
  apiBaseUrl: string;
  /** The job's model usage, shown as "AI usage: … tokens · $…" under the heading. */
  usage?: JobUsage | null;
  /** Shows a "Delete now" action when set. */
  onDeleteNow?: () => void;
  deleting?: boolean;
  /** Rendered under the header, e.g. an error from a failed delete. */
  notice?: ReactNode;
  /** Locale for the expiry date; defaults to the user's. */
  locale?: string;
}

/** Results page body: header with summary and actions, missing-shot callout, image grid. */
export function ResultGrid({
  result,
  expiresAt,
  apiBaseUrl,
  usage,
  onDeleteNow,
  deleting,
  notice,
  locale,
}: ResultGridProps) {
  const [openIndex, setOpenIndex] = useState<number | null>(null);
  const images = result.images;
  const expiry = formatDateTime(expiresAt, locale);

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-4">
        <div className="flex flex-col gap-2">
          <p className="text-sm font-medium text-muted-foreground" data-testid="image-count">
            {pluralize(images.length, "image")}
          </p>
          <h1
            id={RESULTS_HEADING_ID}
            tabIndex={-1}
            className="font-heading text-2xl font-semibold tracking-tight text-balance outline-none sm:text-3xl"
          >
            {result.summary || "Your images"}
          </h1>
          {expiry && (
            <p className="text-sm text-muted-foreground">
              Available until <time dateTime={expiresAt}>{expiry}</time>
            </p>
          )}
          <UsageSummary usage={usage} />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {result.zipUrl && images.length > 0 && (
            <Button asChild size="lg">
              <a href={resolveApiUrl(apiBaseUrl, result.zipUrl)} download>
                <Archive aria-hidden="true" />
                Download all (ZIP)
              </a>
            </Button>
          )}
          {onDeleteNow && <DeleteJobButton onConfirm={onDeleteNow} deleting={deleting} />}
        </div>
        {notice}
      </header>

      {images.length === 0 ? (
        <section
          aria-labelledby="no-results-title"
          className="flex flex-col items-center gap-3 rounded-xl border border-dashed px-6 py-12 text-center"
          data-testid="empty-results"
        >
          <ImageOff aria-hidden="true" className="size-8 text-muted-foreground" />
          <h2 id="no-results-title" className="font-heading text-lg font-semibold">
            No suitable scenes found
          </h2>
          {result.explanation && <p className="max-w-prose text-muted-foreground">{result.explanation}</p>}
          <Button asChild variant="outline">
            <Link href="/">Try another video</Link>
          </Button>
        </section>
      ) : (
        <>
          {result.explanation && <p className="max-w-prose text-pretty text-muted-foreground">{result.explanation}</p>}
          <MissingShots shots={result.missingShots} />
          <ul
            className="grid grid-cols-1 gap-4 min-[480px]:grid-cols-2 lg:grid-cols-3"
            aria-label="Selected images"
            data-testid="result-grid"
          >
            {images.map((image, i) => (
              <li key={image.id} className="flex">
                <ResultCard image={image} apiBaseUrl={apiBaseUrl} onOpen={() => setOpenIndex(i)} />
              </li>
            ))}
          </ul>
          <Lightbox images={images} index={openIndex} onIndexChange={setOpenIndex} apiBaseUrl={apiBaseUrl} />
        </>
      )}

      {images.length === 0 && <MissingShots shots={result.missingShots} />}
    </div>
  );
}
