"use client";

import { ChevronLeft, ChevronRight, Download } from "lucide-react";
import type { KeyboardEvent } from "react";

import { imageAltText, ReframeBadge } from "@/components/result-card";
import { StudioStatusLine, StudioToggle } from "@/components/studio";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { resolveApiUrl, type ResultImage } from "@/lib/api";
import { originalVersion, type ShownVersion } from "@/lib/studio";

export interface LightboxProps {
  images: readonly ResultImage[];
  /** Index of the open image, or null when closed. */
  index: number | null;
  onIndexChange: (index: number | null) => void;
  apiBaseUrl: string;
  /** The version of an image on screen (the same as on its card); default: the original. */
  versionOf?: (image: ResultImage) => ShownVersion;
  /** Flips an image between its studio version and the original (shared with its card). */
  onToggleOriginal?: (image: ResultImage) => void;
}

/**
 * Full-resolution viewer with previous/next buttons and ←/→ keys. Escape closes it. It
 * shows the version the card shows, with the same "Show original" toggle, and Download
 * saves that version.
 */
export function Lightbox({
  images,
  index,
  onIndexChange,
  apiBaseUrl,
  versionOf = originalVersion,
  onToggleOriginal,
}: LightboxProps) {
  const position = index !== null && index >= 0 && index < images.length ? index : null;
  const image = position !== null ? images[position] : null;
  const hasPrev = position !== null && position > 0;
  const hasNext = position !== null && position < images.length - 1;
  const shown = image ? versionOf(image) : null;
  const studio = shown?.variant != null;

  const go = (delta: number) => {
    if (position === null) return;
    const next = position + delta;
    if (next >= 0 && next < images.length) onIndexChange(next);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "ArrowRight") {
      e.preventDefault();
      go(1);
    } else if (e.key === "ArrowLeft") {
      e.preventDefault();
      go(-1);
    }
  };

  return (
    <Dialog open={image !== null} onOpenChange={(o) => !o && onIndexChange(null)}>
      {image && shown && (
        <DialogContent
          className="gap-3 p-3 sm:max-w-5xl sm:p-4"
          onKeyDown={onKeyDown}
          aria-describedby={`lightbox-${image.id}-description`}
        >
          <div className="flex items-center gap-2 pr-8">
            <DialogTitle className="flex items-center gap-2">
              <Badge variant="secondary">{image.shotLabel}</Badge>
              <span className="text-sm font-normal tabular-nums text-muted-foreground">at {image.timestampLabel}</span>
            </DialogTitle>
            <ReframeBadge image={image} />
            <span className="ml-auto text-xs tabular-nums text-muted-foreground" aria-live="polite">
              {(position ?? 0) + 1} of {images.length}
            </span>
          </div>

          <div className="flex items-center justify-center overflow-hidden rounded-lg bg-muted">
            <img
              key={image.id}
              src={resolveApiUrl(apiBaseUrl, shown.url)}
              alt={imageAltText(image, studio)}
              width={shown.width}
              height={shown.height}
              className="max-h-[70vh] w-auto object-contain"
            />
          </div>

          <DialogDescription id={`lightbox-${image.id}-description`}>{image.reason}</DialogDescription>

          {image.studio && (
            <div className="flex flex-wrap items-center gap-2">
              <StudioStatusLine image={image} shown={shown} />
              {onToggleOriginal && (
                <StudioToggle shown={shown} onToggle={() => onToggleOriginal(image)} className="ml-auto" />
              )}
            </div>
          )}

          <div className="flex items-center justify-between gap-2">
            <div className="flex gap-2">
              <Button type="button" variant="outline" size="icon" onClick={() => go(-1)} disabled={!hasPrev} aria-label="Previous image">
                <ChevronLeft aria-hidden="true" />
              </Button>
              <Button type="button" variant="outline" size="icon" onClick={() => go(1)} disabled={!hasNext} aria-label="Next image">
                <ChevronRight aria-hidden="true" />
              </Button>
            </div>
            <Button asChild variant="outline">
              <a href={resolveApiUrl(apiBaseUrl, shown.downloadUrl)} download={shown.fileName}>
                <Download aria-hidden="true" />
                {studio ? "Download studio photo" : "Download"}
                <span className="sr-only">
                  {" "}
                  {image.shotLabel} ({shown.fileName})
                </span>
              </a>
            </Button>
          </div>
        </DialogContent>
      )}
    </Dialog>
  );
}
