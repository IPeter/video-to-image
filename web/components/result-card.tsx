"use client";

import { Clock, Download, Maximize2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { resolveApiUrl, type ResultImage } from "@/lib/api";

export interface ResultCardProps {
  image: ResultImage;
  /** Browser-facing API base URL; image URLs from the API are relative to it. */
  apiBaseUrl: string;
  /** Opens the lightbox for this image. */
  onOpen: () => void;
}

/** Alt text for a selected frame: what it shows and where it came from in the video. */
export function imageAltText(image: Pick<ResultImage, "shotLabel" | "timestampLabel" | "reason">): string {
  return `${image.shotLabel}, frame at ${image.timestampLabel}. ${image.reason}`.trim();
}

/** One selected image: thumbnail (opens the lightbox), shot label, timestamp, reason, download. */
export function ResultCard({ image, apiBaseUrl, onOpen }: ResultCardProps) {
  return (
    <article
      className="group/result flex w-full flex-col overflow-hidden rounded-xl bg-card text-card-foreground ring-1 ring-foreground/10"
      data-image-id={image.id}
      aria-labelledby={`${image.id}-label`}
    >
      <button
        type="button"
        onClick={onOpen}
        className="relative block w-full overflow-hidden bg-muted outline-none focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:ring-inset"
        // Frames of one video share dimensions, so cards in a row line up; 4:3 if unknown.
        style={{ aspectRatio: image.width > 0 && image.height > 0 ? `${image.width} / ${image.height}` : "4 / 3" }}
        aria-label={`View ${image.shotLabel} at ${image.timestampLabel} in full size`}
      >
        <img
          src={resolveApiUrl(apiBaseUrl, image.thumbnailUrl)}
          alt={imageAltText(image)}
          width={image.width}
          height={image.height}
          loading="lazy"
          decoding="async"
          className="size-full object-contain transition-transform duration-200 motion-safe:group-hover/result:scale-[1.02]"
        />
        <span className="absolute top-2 right-2 flex size-7 items-center justify-center rounded-md bg-background/80 text-foreground opacity-0 transition-opacity group-hover/result:opacity-100 group-focus-within/result:opacity-100">
          <Maximize2 aria-hidden="true" className="size-4" />
        </span>
      </button>
      <div className="flex flex-1 flex-col gap-2 p-3">
        <div className="flex items-center justify-between gap-2">
          <Badge id={`${image.id}-label`} variant="secondary" data-testid="shot-label">
            {image.shotLabel}
          </Badge>
          <span className="flex items-center gap-1 text-xs tabular-nums text-muted-foreground">
            <Clock aria-hidden="true" className="size-3.5" />
            <span className="sr-only">Timestamp</span>
            <span data-testid="timestamp">{image.timestampLabel}</span>
          </span>
        </div>
        <div className="flex items-center gap-2">
          <p className="line-clamp-1 flex-1 text-sm text-muted-foreground" title={image.reason}>
            {image.reason}
          </p>
          <a
            href={resolveApiUrl(apiBaseUrl, image.downloadUrl)}
            download={image.fileName}
            className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg text-foreground outline-none hover:bg-muted focus-visible:ring-3 focus-visible:ring-ring/50"
            aria-label={`Download ${image.shotLabel} (${image.fileName})`}
            title="Download image"
          >
            <Download aria-hidden="true" className="size-4" />
          </a>
        </div>
      </div>
    </article>
  );
}
