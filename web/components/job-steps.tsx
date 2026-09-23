import { cn } from "cn";
import { Check, Circle, CircleMinus, LoaderCircle, X } from "lucide-react";

import { Progress as ProgressBar } from "@/components/ui/progress";
import type { JobStatus, Progress, StepName, StepStatus } from "@/lib/api";
import { formatDuration } from "@/lib/format";

// The pipeline's six steps shown as five friendly rows: score and select are one
// user-visible activity ("Picking the best shots").

export type StepRowId = "preview" | "analyze" | "extract" | "pick" | "export";

export interface StepRowDef {
  id: StepRowId;
  label: string;
  steps: readonly StepName[];
}

const ROWS = [
  { id: "preview", label: "Preparing video", steps: ["preview"] },
  { id: "analyze", label: "Understanding the video", steps: ["analyze"] },
  { id: "extract", label: "Extracting frames", steps: ["extract"] },
  { id: "pick", label: "Picking the best shots", steps: ["score", "select"] },
  { id: "export", label: "Packaging", steps: ["export"] },
] as const satisfies readonly StepRowDef[];

export const STEP_ROWS: readonly StepRowDef[] = ROWS;

// Compile-time guard: every pipeline step must belong to a row, so a new step in Go
// breaks the build here until it gets a label.
type UnmappedSteps = Exclude<StepName, (typeof ROWS)[number]["steps"][number]>;
const allStepsMapped: [UnmappedSteps] extends [never] ? true : UnmappedSteps = true;
void allStepsMapped;

export interface StepRowState extends StepRowDef {
  status: StepStatus;
  /** Total duration of the row's finished steps, when all of them reported one. */
  ms: number | null;
  current: boolean;
}

/** Combines the statuses of the pipeline steps in one row. */
export function combineStatuses(statuses: readonly StepStatus[]): StepStatus {
  if (statuses.length === 0) return "pending";
  if (statuses.includes("failed")) return "failed";
  if (statuses.includes("running")) return "running";
  const finished = statuses.filter((s) => s === "done" || s === "skipped");
  if (finished.length === statuses.length) return statuses.every((s) => s === "skipped") ? "skipped" : "done";
  // Some finished, the rest not started yet: the row is mid-way.
  return finished.length > 0 ? "running" : "pending";
}

/** Maps API progress to the five display rows and marks the current one. */
export function deriveStepRows(progress: Progress): StepRowState[] {
  const byName = new Map(progress.steps.map((s) => [s.name, s]));
  const rows = STEP_ROWS.map((row): StepRowState => {
    const members = row.steps.map((name) => byName.get(name));
    const status = combineStatuses(members.map((m) => m?.status ?? "pending"));
    const durations = members.map((m) => m?.ms);
    const ms =
      status === "done" && durations.every((d) => typeof d === "number")
        ? durations.reduce<number>((sum, d) => sum + (d ?? 0), 0)
        : null;
    return { ...row, status, ms, current: false };
  });

  const byStep = progress.step ? rows.findIndex((r) => r.steps.includes(progress.step as StepName)) : -1;
  const currentIndex = byStep >= 0 ? byStep : rows.findIndex((r) => r.status === "running");
  if (currentIndex >= 0) rows[currentIndex] = { ...rows[currentIndex], current: true };
  return rows;
}

const STATUS_TEXT: Record<StepStatus, string> = {
  pending: "not started",
  running: "in progress",
  done: "done",
  failed: "failed",
  skipped: "skipped",
};

function StepIcon({ status, current }: { status: StepStatus; current: boolean }) {
  const common = "size-5 shrink-0";
  switch (status) {
    case "done":
      return (
        <span className="flex size-6 items-center justify-center rounded-full bg-success/15 text-success">
          <Check aria-hidden="true" className="size-4" strokeWidth={3} />
        </span>
      );
    case "failed":
      return (
        <span className="flex size-6 items-center justify-center rounded-full bg-destructive/15 text-destructive">
          <X aria-hidden="true" className="size-4" strokeWidth={3} />
        </span>
      );
    case "skipped":
      return (
        <span className="flex size-6 items-center justify-center text-muted-foreground">
          <CircleMinus aria-hidden="true" className={common} />
        </span>
      );
    case "running":
      return (
        <span className="flex size-6 items-center justify-center text-primary">
          <LoaderCircle aria-hidden="true" className={cn(common, "motion-safe:animate-spin")} />
        </span>
      );
    case "pending":
      return (
        <span className={cn("flex size-6 items-center justify-center", current ? "text-primary" : "text-muted-foreground/60")}>
          <Circle aria-hidden="true" className={common} />
        </span>
      );
  }
}

export interface JobStepsProps {
  progress: Progress;
  /** Job status; "queued" shows a waiting message instead of a current step. */
  status: JobStatus;
  /** Seconds since the job was created, shown as "Elapsed m:ss". */
  elapsedSec?: number;
}

/** Step list with the current step highlighted, finished steps ticked and a progress bar. */
export function JobSteps({ progress, status, elapsedSec }: JobStepsProps) {
  const rows = deriveStepRows(progress);
  const currentIndex = rows.findIndex((r) => r.current);
  const current = currentIndex >= 0 ? rows[currentIndex] : null;
  const percent = Math.max(0, Math.min(100, Math.round(progress.percent)));

  const headline =
    status === "queued"
      ? "Waiting to start…"
      : current
        ? `Step ${currentIndex + 1} of ${rows.length}: ${current.label}`
        : "Working on it…";

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-2">
        <div className="flex items-baseline justify-between gap-4">
          <p className="font-medium" aria-live="polite" data-testid="job-steps-headline">
            {headline}
          </p>
          <p className="text-sm tabular-nums text-muted-foreground">
            {percent}%
            {elapsedSec !== undefined && (
              <>
                <span aria-hidden="true"> · </span>
                <span className="sr-only">, </span>
                Elapsed {formatDuration(elapsedSec)}
              </>
            )}
          </p>
        </div>
        <ProgressBar value={percent} aria-label="Processing progress" />
      </div>

      <ol className="flex flex-col gap-1" aria-label="Processing steps">
        {rows.map((row) => (
          <li
            key={row.id}
            data-step={row.id}
            data-status={row.status}
            aria-current={row.current ? "step" : undefined}
            className={cn(
              "flex items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors",
              row.current && "bg-accent font-medium text-accent-foreground",
              !row.current && row.status === "pending" && "text-muted-foreground",
            )}
          >
            <StepIcon status={row.status} current={row.current} />
            <span className="flex-1">{row.label}</span>
            <span className="sr-only">, {STATUS_TEXT[row.status]}</span>
            {row.ms !== null && (
              <span className="text-xs tabular-nums text-muted-foreground" aria-hidden="true">
                {(row.ms / 1000).toFixed(1)} s
              </span>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}
