"use client";

import { Archive, ImageOff } from "lucide-react";
import Link from "next/link";
import { useState, type ReactNode } from "react";

import { DeleteJobButton } from "@/components/delete-job-button";
import { Lightbox } from "@/components/lightbox";
import { MissingShots } from "@/components/missing-shots";
import { ResultCard } from "@/components/result-card";
import { BackgroundSwitch, StudioPreparing } from "@/components/studio";
import { Button } from "@/components/ui/button";
import { UsageSummary } from "@/components/usage-summary";
import { useBackgroundPreference } from "@/hooks/use-background-preference";
import { resolveApiUrl, type JobResult, type JobStudio, type JobUsage, type ResultImage, type StudioStyle } from "@/lib/api";
import { formatDateTime, pluralize } from "@/lib/format";
import {
  hasReadyStudio,
  isStudioInProgress,
  resolveBackground,
  shownVersion,
  STUDIO_STYLE_LABELS,
  studioStyleOptions,
  type BackgroundMode,
} from "@/lib/studio";

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
  /** The job's studio backgrounds (job.studio); absent or "none" → originals only. */
  studio?: JobStudio | null;
  /** Shows a "Delete now" action when set. */
  onDeleteNow?: () => void;
  deleting?: boolean;
  /** Rendered under the header, e.g. an error from a failed delete. */
  notice?: ReactNode;
  /** Locale for the expiry date; defaults to the user's. */
  locale?: string;
}

/**
 * Results page body: header with summary and actions, missing-shot callout, image grid.
 *
 * Studio backgrounds: once any image has a studio version, a switch above the grid picks
 * Original or Studio (and the style); the choice is remembered in this browser, and with
 * none remembered the studio version in the job's default style is shown. Each image can
 * be flipped on its own ("Show original" on the card or in the lightbox); changing the
 * switch resets those flips. Images without a studio version always show the original.
 */
export function ResultGrid({
  result,
  expiresAt,
  apiBaseUrl,
  usage,
  studio,
  onDeleteNow,
  deleting,
  notice,
  locale,
}: ResultGridProps) {
  const [openIndex, setOpenIndex] = useState<number | null>(null);
  const images = result.images;
  const expiry = formatDateTime(expiresAt, locale);

  const [preference, setPreference] = useBackgroundPreference();
  const styles = studioStyleOptions(studio, images);
  const studioReady = hasReadyStudio(images);
  const preparing = !studioReady && isStudioInProgress(studio) && images.some((img) => img.studio?.status === "pending");
  const choice = resolveBackground(preference, styles, studio?.defaultStyle);
  // Images flipped away from the switch's mode by their own toggle.
  const [flipped, setFlipped] = useState<ReadonlySet<string>>(() => new Set());
  const showsOriginal = (image: ResultImage) => (choice.mode === "original") !== flipped.has(image.id);
  const versionOf = (image: ResultImage) => shownVersion(image, choice, showsOriginal(image));
  const toggleOriginal = (image: ResultImage) =>
    setFlipped((prev) => {
      const next = new Set(prev);
      if (!next.delete(image.id)) next.add(image.id);
      return next;
    });
  const setMode = (mode: BackgroundMode) => {
    setPreference({ mode, style: choice.style });
    setFlipped(new Set());
  };
  const setStyle = (style: StudioStyle) => {
    setPreference({ mode: "studio", style });
    setFlipped(new Set());
  };

  // Tell screen-reader users once when studio versions arrive while the page is open.
  const [wasReady, setWasReady] = useState(studioReady);
  const [studioAnnouncement, setStudioAnnouncement] = useState("");
  if (wasReady !== studioReady) {
    setWasReady(studioReady);
    if (studioReady) {
      setStudioAnnouncement(
        choice.mode === "studio"
          ? `Studio backgrounds are ready. Showing the ${STUDIO_STYLE_LABELS[choice.style].toLowerCase()} studio background; the originals stay available.`
          : "Studio backgrounds are ready. Switch the background to Studio to see them.",
      );
    }
  }

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
          {studio?.zipUrl && studioReady && (
            <Button asChild size="lg" variant="outline">
              <a href={resolveApiUrl(apiBaseUrl, studio.zipUrl)} download>
                <Archive aria-hidden="true" />
                Download studio photos (ZIP)
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
          {(studioReady || preparing) && (
            // Same height in both states, so the grid does not move when the switch appears.
            <div className="flex min-h-9 items-center" data-testid="studio-bar">
              {studioReady ? (
                <BackgroundSwitch styles={styles} choice={choice} onModeChange={setMode} onStyleChange={setStyle} />
              ) : (
                <StudioPreparing />
              )}
            </div>
          )}
          <ul
            className="grid grid-cols-1 gap-4 min-[480px]:grid-cols-2 lg:grid-cols-3"
            aria-label="Selected images"
            data-testid="result-grid"
          >
            {images.map((image, i) => (
              <li key={image.id} className="flex">
                <ResultCard
                  image={image}
                  apiBaseUrl={apiBaseUrl}
                  onOpen={() => setOpenIndex(i)}
                  shown={versionOf(image)}
                  onToggleOriginal={() => toggleOriginal(image)}
                />
              </li>
            ))}
          </ul>
          <Lightbox
            images={images}
            index={openIndex}
            onIndexChange={setOpenIndex}
            apiBaseUrl={apiBaseUrl}
            versionOf={versionOf}
            onToggleOriginal={toggleOriginal}
          />
        </>
      )}

      {images.length === 0 && <MissingShots shots={result.missingShots} />}
      <p aria-live="polite" className="sr-only" data-testid="studio-announcer">
        {studioAnnouncement}
      </p>
    </div>
  );
}
