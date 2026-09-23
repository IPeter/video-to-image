"use client";

import { ChevronLeft, ChevronRight, Download } from "lucide-react";
import type { KeyboardEvent } from "react";

import { imageAltText } from "@/components/result-card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { resolveApiUrl, type ResultImage } from "@/lib/api";

export interface LightboxProps {
  images: readonly ResultImage[];
  /** Index of the open image, or null when closed. */
  index: number | null;
  onIndexChange: (index: number | null) => void;
  apiBaseUrl: string;
}

/** Full-resolution viewer with previous/next buttons and ←/→ keys. Escape closes it. */
export function Lightbox({ images, index, onIndexChange, apiBaseUrl }: LightboxProps) {
  const position = index !== null && index >= 0 && index < images.length ? index : null;
  const image = position !== null ? images[position] : null;
  const hasPrev = position !== null && position > 0;
  const hasNext = position !== null && position < images.length - 1;

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
      {image && (
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
            <span className="ml-auto text-xs tabular-nums text-muted-foreground" aria-live="polite">
              {(position ?? 0) + 1} of {images.length}
            </span>
          </div>

          <div className="flex items-center justify-center overflow-hidden rounded-lg bg-muted">
            <img
              key={image.id}
              src={resolveApiUrl(apiBaseUrl, image.url)}
              alt={imageAltText(image)}
              width={image.width}
              height={image.height}
              className="max-h-[70vh] w-auto object-contain"
            />
          </div>

          <DialogDescription id={`lightbox-${image.id}-description`}>{image.reason}</DialogDescription>

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
              <a href={resolveApiUrl(apiBaseUrl, image.downloadUrl)} download={image.fileName}>
                <Download aria-hidden="true" />
                Download
                <span className="sr-only"> {image.shotLabel} ({image.fileName})</span>
              </a>
            </Button>
          </div>
        </DialogContent>
      )}
    </Dialog>
  );
}
