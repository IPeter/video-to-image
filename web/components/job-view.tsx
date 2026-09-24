"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";

import { ErrorMessage } from "@/components/error-message";
import {
  ConnectionNotice,
  DeletedState,
  ExpiredState,
  FailedState,
  JobLoadingState,
  JobNotFoundState,
  RejectedState,
  stateHeadingId,
} from "@/components/job-states";
import { JobSteps } from "@/components/job-steps";
import { useAppContext } from "@/components/providers";
import { RESULTS_HEADING_ID, ResultGrid } from "@/components/result-grid";
import { Button } from "@/components/ui/button";
import { UsageSummary } from "@/components/usage-summary";
import { useDeadlinePassed } from "@/hooks/use-deadline";
import { useNow } from "@/hooks/use-now";
import { ApiError, isTerminalStatus, isTransientError, type Job } from "@/lib/api";
import { elapsedSeconds, pluralize } from "@/lib/format";
import { isStudioInProgress } from "@/lib/studio";

/** Poll interval while a job is queued or processing (plan/07-frontend.md). */
export const POLL_INTERVAL_MS = 1500;
/**
 * A completed job whose studio backgrounds are still being made is polled this many
 * times slower: they take several seconds per photo, and the originals are already shown.
 */
export const STUDIO_POLL_SLOWDOWN = 2;
const MAX_RETRY_DELAY_MS = 15_000;

export function jobQueryKey(domainKey: string, jobId: string) {
  return ["job", domainKey, jobId] as const;
}

/**
 * refetchInterval: poll until the job is terminal or the API gave a final answer; a
 * completed job keeps being polled (more slowly) while its studio backgrounds are made.
 */
export function jobRefetchInterval(
  data: Job | undefined,
  error: unknown,
  intervalMs: number = POLL_INTERVAL_MS,
): number | false {
  if (error && !isTransientError(error)) return false;
  if (data?.status === "completed" && isStudioInProgress(data.studio)) return intervalMs * STUDIO_POLL_SLOWDOWN;
  if (data && isTerminalStatus(data.status)) return false;
  return intervalMs;
}

/** Retry n (1-based, as TanStack Query passes it) waits 1 s, 2 s, 4 s … capped at 15 s. */
export function jobRetryDelay(failureCount: number): number {
  return Math.min(1000 * 2 ** Math.max(0, failureCount - 1), MAX_RETRY_DELAY_MS);
}

/** Keep retrying transient failures (network, 429, 5xx) indefinitely; 4xx answers are final. */
export function shouldRetryJobQuery(_failureCount: number, error: unknown): boolean {
  return isTransientError(error);
}

/**
 * refetchOnWindowFocus: like polling, plus one GET for a completed job. A result left open
 * in a tab can expire or be deleted from another device; the GET then answers 410 or 404
 * and the page switches to the expired or not-found state instead of offering dead links.
 */
export function jobRefetchOnFocus(data: Job | undefined, error: unknown): boolean {
  if (error && !isTransientError(error)) return false;
  if (data?.status === "completed") return true;
  return jobRefetchInterval(data, error) !== false;
}

export interface JobViewProps {
  jobId: string;
  /** Overrides the poll interval (tests). */
  pollIntervalMs?: number;
}

/** What the job page shows. Every kind except "loading" renders its own <h1>. */
type JobViewKind =
  | "loading"
  | "processing"
  | "completed"
  | "rejected"
  | "failed"
  | "expired"
  | "deleted"
  | "not-found"
  | "error";

interface JobViewModel {
  kind: JobViewKind;
  /** Id of the view's <h1>; focus moves there when the page switches to this view. */
  headingId: string | null;
  /** Announced through the page's live region when the job reaches this final view. */
  announcement: string | null;
  node: ReactNode;
}

function expiredView(retentionHours: number): JobViewModel {
  return {
    kind: "expired",
    headingId: stateHeadingId("job-expired"),
    announcement: "These images have expired.",
    node: <ExpiredState retentionHours={retentionHours} />,
  };
}

/**
 * Job page body: polls the job and renders progress, results or a terminal state.
 *
 * Accessibility: when the page switches from one view to another (processing → results,
 * results → deleted, …) the old content and the focus in it disappear, so focus moves to
 * the new view's heading, and final outcomes are also announced through a persistent
 * visually hidden live region (the processing view announces its steps itself).
 */
export function JobView({ jobId, pollIntervalMs = POLL_INTERVAL_MS }: JobViewProps) {
  const { config, domain, api } = useAppContext();
  const queryClient = useQueryClient();
  const queryKey = jobQueryKey(config.domainKey, jobId);
  const [deleted, setDeleted] = useState(false);

  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => api.getJob(config.domainKey, jobId, signal),
    refetchInterval: (q) => jobRefetchInterval(q.state.data, q.state.error, pollIntervalMs),
    refetchOnWindowFocus: (q) => jobRefetchOnFocus(q.state.data, q.state.error),
    retry: shouldRetryJobQuery,
    retryDelay: jobRetryDelay,
  });

  const onDeleted = () => {
    // "expired" is the server-side status of a deleted job; it also stops focus refetches.
    queryClient.setQueryData<Job>(queryKey, (old) => (old ? { ...old, status: "expired", result: null } : old));
    setDeleted(true);
  };

  const deletion = useMutation({
    mutationFn: () => api.deleteJob(config.domainKey, jobId),
    onSuccess: onDeleted,
    onError: (err) => {
      // Already gone on the server: the outcome the user asked for.
      if (err instanceof ApiError && (err.status === 404 || err.status === 410)) onDeleted();
    },
  });

  const job = query.data;
  // The files are deleted at expiresAt: stop offering them then, even without a new answer.
  const expiresAtMs = job?.status === "completed" ? Date.parse(job.expiresAt) : null;
  const expiredByClock = useDeadlinePassed(expiresAtMs);
  const { refetch } = query;
  useEffect(() => {
    // Confirm with the API (410 EXPIRED, or a new status) once the deadline passes.
    if (expiredByClock) void refetch();
  }, [expiredByClock, refetch]);

  const finalError = query.error && !isTransientError(query.error) ? query.error : null;
  const reconnecting = query.failureCount > 0 && query.fetchStatus !== "idle";

  function statusView(job: Job): JobViewModel {
    switch (job.status) {
      case "queued":
      case "processing":
        return {
          kind: "processing",
          headingId: PROCESSING_HEADING_ID,
          announcement: null,
          node: <JobProcessing job={job} reconnecting={reconnecting} />,
        };
      case "rejected":
        return {
          kind: "rejected",
          headingId: stateHeadingId("job-rejected"),
          announcement: domain.copy.rejectedTitle,
          node: <RejectedState title={domain.copy.rejectedTitle} rejection={job.rejection} usage={job.usage} />,
        };
      case "failed":
        return {
          kind: "failed",
          headingId: stateHeadingId("job-failed"),
          announcement: "Sorry, we could not process this video.",
          node: <FailedState jobId={job.jobId} error={job.error} usage={job.usage} />,
        };
      case "expired":
        return expiredView(domain.retentionHours);
      case "completed": {
        if (!job.result || expiredByClock) return expiredView(domain.retentionHours);
        const count = job.result.images.length;
        const deleteError =
          deletion.isError && !(deletion.error instanceof ApiError && [404, 410].includes(deletion.error.status));
        return {
          kind: "completed",
          headingId: RESULTS_HEADING_ID,
          announcement:
            count > 0
              ? `Processing finished: ${pluralize(count, "image")} ready.`
              : "Processing finished: no suitable scenes were found.",
          node: (
            <ResultGrid
              result={job.result}
              expiresAt={job.expiresAt}
              apiBaseUrl={api.baseUrl}
              usage={job.usage}
              studio={job.studio}
              onDeleteNow={() => deletion.mutate()}
              deleting={deletion.isPending}
              notice={
                deleteError ? (
                  <ErrorMessage
                    title="The images could not be deleted"
                    code={deletion.error instanceof ApiError ? deletion.error.code : null}
                    problem={
                      deletion.error instanceof ApiError && deletion.error.kind === "network"
                        ? "NETWORK_ERROR"
                        : "UNKNOWN_ERROR"
                    }
                  />
                ) : null
              }
            />
          ),
        };
      }
      default: {
        const unhandled: never = job.status;
        return unhandled;
      }
    }
  }

  let view: JobViewModel;
  if (deleted) {
    view = {
      kind: "deleted",
      headingId: stateHeadingId("job-deleted"),
      announcement: "Your images and video were deleted.",
      node: <DeletedState />,
    };
  } else if (finalError) {
    view = errorView(finalError, domain.retentionHours, () => void query.refetch());
  } else if (!job) {
    view = { kind: "loading", headingId: null, announcement: null, node: <JobLoadingState reconnecting={reconnecting} /> };
  } else {
    view = statusView(job);
  }

  // Remember the previous view across renders (state, not a ref, so it is render-safe).
  // A switch away from anything but the initial loading placeholder is a transition the
  // user did not navigate to: focus and announce it.
  const [shown, setShown] = useState<{ kind: JobViewKind; moved: boolean; headingId: string | null }>({
    kind: view.kind,
    moved: false,
    headingId: view.headingId,
  });
  if (shown.kind !== view.kind) {
    setShown({ kind: view.kind, moved: shown.kind !== "loading", headingId: view.headingId });
  }
  useEffect(() => {
    // Runs once per view switch, not on every poll.
    if (shown.moved && shown.headingId) document.getElementById(shown.headingId)?.focus();
  }, [shown]);

  const announcement = shown.moved ? (view.announcement ?? "") : "";

  return (
    <>
      <p aria-live="polite" aria-atomic="true" className="sr-only" data-testid="job-announcer">
        {announcement}
      </p>
      {view.node}
    </>
  );
}

const PROCESSING_HEADING_ID = "job-processing-title";

function JobProcessing({ job, reconnecting }: { job: Job; reconnecting: boolean }) {
  const now = useNow(1000);
  return (
    <section
      aria-labelledby={PROCESSING_HEADING_ID}
      className="mx-auto flex w-full max-w-2xl flex-col gap-6"
      data-testid="job-processing"
    >
      <div className="flex flex-col gap-2">
        <h1
          id={PROCESSING_HEADING_ID}
          tabIndex={-1}
          className="font-heading text-2xl font-semibold tracking-tight outline-none sm:text-3xl"
        >
          {job.status === "queued" ? "Your video is in line" : "Processing your video"}
        </h1>
        <p className="text-muted-foreground">
          This usually takes under a minute. You can keep this page open, or bookmark it and come back later.
        </p>
      </div>
      <div className="rounded-xl border bg-card p-4 sm:p-6">
        <JobSteps progress={job.progress} status={job.status} elapsedSec={elapsedSeconds(job.createdAt, now)} />
      </div>
      <UsageSummary usage={job.usage} />
      <ConnectionNotice show={reconnecting} />
    </section>
  );
}

function errorView(error: unknown, retentionHours: number, onRetry: () => void): JobViewModel {
  const apiError = error instanceof ApiError ? error : null;
  if (apiError?.code === "EXPIRED" || apiError?.status === 410) return expiredView(retentionHours);
  if (apiError?.code === "JOB_NOT_FOUND" || (apiError?.status === 404 && apiError.code !== "DOMAIN_NOT_FOUND")) {
    return {
      kind: "not-found",
      headingId: stateHeadingId("job-not-found"),
      announcement: "We could not find this job.",
      node: <JobNotFoundState />,
    };
  }
  return {
    kind: "error",
    headingId: ERROR_HEADING_ID,
    announcement: "We could not load this job.",
    node: (
      <section
        aria-labelledby={ERROR_HEADING_ID}
        className="mx-auto flex w-full max-w-2xl flex-col gap-4"
        data-testid="job-error"
      >
        <h1 id={ERROR_HEADING_ID} tabIndex={-1} className="font-heading text-2xl font-semibold tracking-tight outline-none">
          We could not load this job
        </h1>
        <ErrorMessage code={apiError?.code ?? null}>
          <Button type="button" variant="outline" size="sm" className="mt-3" onClick={onRetry}>
            Try again
          </Button>
        </ErrorMessage>
      </section>
    ),
  };
}

const ERROR_HEADING_ID = "job-error-title";
