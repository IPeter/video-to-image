"use client";

import { X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { formatBytes } from "@/lib/format";

export interface UploadProgressProps {
  /** 0–100, or null while the browser cannot tell the total. */
  percent: number | null;
  loaded: number;
  total: number;
  /** A piece of the video failed and is about to be sent again. */
  retrying?: boolean;
  /**
   * Every piece is stored and the server is checking the video. The job may be created
   * even if the browser stops waiting, so Cancel is disabled from here on.
   */
  completing?: boolean;
  onCancel: () => void;
}

/** Announce progress in 25 % steps only, so screen readers are not flooded. */
function announcement(percent: number | null, retrying: boolean, completing: boolean): string {
  if (retrying) return "Having trouble reaching the server, retrying the upload";
  if (completing || (percent !== null && percent >= 100)) return "Upload finished, checking the video";
  if (percent === null) return "Uploading";
  return `Upload ${Math.floor(percent / 25) * 25}% complete`;
}

/** "Uploading… 37% (140 MB of 377 MB)", or "Checking the video…" once every byte is stored. */
export function progressText(percent: number | null, loaded: number, total: number, completing = false): string {
  if (completing || (percent !== null && percent >= 100)) return "Checking the video…";
  if (percent === null || total <= 0) return `Uploading… ${formatBytes(loaded)}`;
  return `Uploading… ${percent}% (${formatBytes(loaded)} of ${formatBytes(total)})`;
}

export function UploadProgress({
  percent,
  loaded,
  total,
  retrying = false,
  completing = false,
  onCancel,
}: UploadProgressProps) {
  return (
    <div className="flex flex-col gap-3 rounded-xl border bg-card p-4">
      <p className="text-sm font-medium tabular-nums" aria-hidden="true" data-testid="upload-progress-text">
        {progressText(percent, loaded, total, completing)}
      </p>
      <Progress value={percent ?? 0} aria-label="Upload progress" />
      <div className="flex items-center justify-between gap-4 text-xs text-muted-foreground">
        <span data-testid="upload-retrying">{retrying ? "Having trouble reaching the server. Retrying…" : null}</span>
        <Button type="button" variant="outline" size="sm" onClick={onCancel} disabled={completing}>
          <X aria-hidden="true" />
          Cancel upload
        </Button>
      </div>
      <p className="sr-only" aria-live="polite">
        {announcement(percent, retrying, completing)}
      </p>
    </div>
  );
}
