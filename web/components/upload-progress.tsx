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
  onCancel: () => void;
}

/** Announce progress in 25 % steps only, so screen readers are not flooded. */
function announcement(percent: number | null): string {
  if (percent === null) return "Uploading";
  if (percent >= 100) return "Upload finished, starting processing";
  return `Upload ${Math.floor(percent / 25) * 25}% complete`;
}

export function UploadProgress({ percent, loaded, total, onCancel }: UploadProgressProps) {
  const shown = percent ?? 0;
  const finishing = percent !== null && percent >= 100;
  return (
    <div className="flex flex-col gap-3 rounded-xl border bg-card p-4">
      <div className="flex items-center justify-between gap-4 text-sm">
        <span className="font-medium">{finishing ? "Checking the video…" : "Uploading…"}</span>
        <span className="tabular-nums text-muted-foreground" aria-hidden="true">
          {percent === null ? formatBytes(loaded) : `${shown}%`}
        </span>
      </div>
      <Progress value={shown} aria-label="Upload progress" />
      <div className="flex items-center justify-between gap-4 text-xs text-muted-foreground">
        <span className="tabular-nums">{total > 0 ? `${formatBytes(loaded)} of ${formatBytes(total)}` : null}</span>
        <Button type="button" variant="outline" size="sm" onClick={onCancel}>
          <X aria-hidden="true" />
          Cancel upload
        </Button>
      </div>
      <p className="sr-only" aria-live="polite">
        {announcement(percent)}
      </p>
    </div>
  );
}
