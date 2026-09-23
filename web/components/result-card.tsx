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

/** How the export reframed a video frame: the badge text and its explanation. */
export interface ReframeInfo {
  /** "Straightened", "Cropped" or "Straightened · Cropped". */
  label: string;
  /** Full sentence for the tooltip and screen readers. */
  description: string;
}

/** Null when the image is the video frame as recorded (no rotation, no crop). */
export function reframeInfo(image: Pick<ResultImage, "rotation" | "cropped">): ReframeInfo | null {
  const degrees = (((Math.round(image.rotation) % 360) + 360) % 360) || 0;
  const straightened = degrees !== 0;
  if (!straightened && !image.cropped) return null;
  const labels: string[] = [];
  const actions: string[] = [];
  if (straightened) {
    labels.push("Straightened");
    actions.push(`rotated ${degrees}° clockwise to stand upright`);
  }
  if (image.cropped) {
    labels.push("Cropped");
    actions.push("cropped to frame the subject");
  }
  return {
    label: labels.join(" · "),
    description: `Reframed from the video frame: ${actions.join(" and ")}.`,
  };
}

/** Small muted badge for a straightened or cropped image; nothing for an untouched frame. */
export function ReframeBadge({ image }: { image: Pick<ResultImage, "rotation" | "cropped"> }) {
  const info = reframeInfo(image);
  if (!info) return null;
  return (
    <Badge variant="outline" className="font-normal text-muted-foreground" title={info.description} data-testid="reframed">
      <span aria-hidden="true">{info.label}</span>
      <span className="sr-only">{info.description}</span>
    </Badge>
  );
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
        // One frame shape for every card, so a row lines up even when photos differ
        // (straightened portrait frames, 4:3 crops, 16:9 originals): the image is
        // letterboxed inside the 4:3 listing shape.
        style={{ aspectRatio: "4 / 3" }}
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
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Badge id={`${image.id}-label`} variant="secondary" data-testid="shot-label">
            {image.shotLabel}
          </Badge>
          <span className="ml-auto flex flex-wrap items-center justify-end gap-2">
            <ReframeBadge image={image} />
            <span className="flex items-center gap-1 text-xs tabular-nums text-muted-foreground">
              <Clock aria-hidden="true" className="size-3.5" />
              <span className="sr-only">Timestamp</span>
              <span data-testid="timestamp">{image.timestampLabel}</span>
            </span>
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
