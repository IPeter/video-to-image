"use client";

import { cn } from "cn";
import { Film, Info, TriangleAlert } from "lucide-react";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useRef, useState } from "react";

import { ErrorMessage, type ClientProblem } from "@/components/error-message";
import { useRuntimeConfig } from "@/components/providers";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { UploadDropzone } from "@/components/upload-dropzone";
import { UploadProgress } from "@/components/upload-progress";
import { useDeadlinePassed } from "@/hooks/use-deadline";
import { useNow } from "@/hooks/use-now";
import type { ErrorCode } from "@/lib/api";
import { formatBytes, formatDuration, formatWait } from "@/lib/format";
import { uploadVideo, UploadError, type UploadFailure, type UploadHandle, type UploadProgress as Progress } from "@/lib/upload";
import {
  hasBlockingIssue,
  precheckFile,
  precheckMetadata,
  type CheckIssue,
  type UploadLimits,
} from "@/lib/upload-checks";
import { probeVideoFile, type VideoProbe } from "@/lib/video-meta";

interface Selection {
  file: File;
  /** Blocking file checks plus advisory metadata checks. */
  issues: CheckIssue[];
  probing: boolean;
  probe: VideoProbe | null;
}

type Phase =
  | { kind: "idle" }
  | { kind: "uploading"; progress: Progress }
  | { kind: "redirecting"; jobId: string }
  | { kind: "failed"; failure: Exclude<UploadFailure, { kind: "aborted" }> }
  | { kind: "cancelled" };

/** The API asked the browser to wait before the next upload (Retry-After on 429 / 503). */
interface RetryWait {
  startedAt: number;
  until: number;
}

/** A wait the API asked for, when the failure is one where waiting helps. */
function retryWaitFor(failure: UploadFailure, nowMs: number): RetryWait | null {
  if (failure.kind !== "http" || !failure.retryAfterSec) return null;
  if (failure.status !== 429 && failure.status !== 503) return null;
  return { startedAt: nowMs, until: nowMs + failure.retryAfterSec * 1000 };
}

export interface UploadFormProps {
  limits: UploadLimits;
}

function failureToError(failure: Exclude<UploadFailure, { kind: "aborted" }>): {
  code: ErrorCode | null;
  problem: ClientProblem;
} {
  switch (failure.kind) {
    case "http":
      return { code: failure.code, problem: "UNKNOWN_ERROR" };
    case "network":
      return { code: null, problem: "NETWORK_ERROR" };
    case "timeout":
      return { code: null, problem: "UPLOAD_TIMEOUT" };
  }
}

/** Upload page body: pick → pre-check and preview → upload with progress → job page. */
export function UploadForm({ limits }: UploadFormProps) {
  const { apiUrl, domainKey } = useRuntimeConfig();
  const router = useRouter();
  const [selection, setSelection] = useState<Selection | null>(null);
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [retryWait, setRetryWait] = useState<RetryWait | null>(null);
  const waitOver = useDeadlinePassed(retryWait?.until ?? null);
  const waiting = retryWait !== null && !waitOver;
  const now = useNow(1000, waiting);
  const uploadRef = useRef<UploadHandle | null>(null);
  const probeAbort = useRef<AbortController | null>(null);

  // Cancel an in-flight upload/probe when the component goes away.
  useEffect(
    () => () => {
      uploadRef.current?.abort();
      probeAbort.current?.abort();
    },
    [],
  );

  const onFile = useCallback(
    (file: File) => {
      probeAbort.current?.abort();
      const issues = precheckFile(file, limits);
      const blocked = hasBlockingIssue(issues);
      setPhase({ kind: "idle" });
      setSelection({ file, issues, probing: !blocked, probe: null });
      if (blocked) return;

      const controller = new AbortController();
      probeAbort.current = controller;
      void probeVideoFile(file, { signal: controller.signal }).then((probe) => {
        if (controller.signal.aborted) return;
        setSelection((current) =>
          current?.file === file
            ? {
                ...current,
                probing: false,
                probe,
                issues: probe.metadata ? [...current.issues, ...precheckMetadata(probe.metadata, limits)] : current.issues,
              }
            : current,
        );
      });
    },
    [limits],
  );

  const reset = () => {
    probeAbort.current?.abort();
    setSelection(null);
    setPhase({ kind: "idle" });
  };

  const startUpload = () => {
    if (!selection || hasBlockingIssue(selection.issues) || uploadRef.current || waiting) return;
    setRetryWait(null);
    setPhase({
      kind: "uploading",
      progress: { loaded: 0, total: selection.file.size, percent: 0, retrying: false, completing: false },
    });
    const handle = uploadVideo({
      apiUrl,
      domainKey,
      file: selection.file,
      onProgress: (progress) => setPhase((p) => (p.kind === "uploading" ? { kind: "uploading", progress } : p)),
    });
    uploadRef.current = handle;
    handle.promise.then(
      (res) => {
        uploadRef.current = null;
        setPhase({ kind: "redirecting", jobId: res.jobId });
        router.push(`/jobs/${encodeURIComponent(res.jobId)}`);
      },
      (err: unknown) => {
        uploadRef.current = null;
        const failure: UploadFailure = err instanceof UploadError ? err.failure : { kind: "network" };
        setRetryWait(retryWaitFor(failure, Date.now()));
        setPhase(failure.kind === "aborted" ? { kind: "cancelled" } : { kind: "failed", failure });
      },
    );
  };

  const cancelUpload = () => uploadRef.current?.abort();

  const busy = phase.kind === "uploading" || phase.kind === "redirecting";
  const blocked = selection ? hasBlockingIssue(selection.issues) : false;
  const acceptHint = `MP4 or MOV, up to ${formatBytes(limits.maxUploadBytes)}.`;
  // Counts down every second. Inside the error alert it sits in an aria-live="off"
  // element: announced once with the alert, not on every tick.
  const waitText = waiting
    ? `You can upload again in ${formatWait((retryWait.until - Math.max(now, retryWait.startedAt)) / 1000)}.`
    : retryWait
      ? "You can upload again now."
      : null;

  return (
    <div className="flex flex-col gap-6">
      {!selection ? (
        <UploadDropzone onFile={onFile} hint={acceptHint} />
      ) : (
        <SelectedVideo selection={selection} />
      )}

      {selection && selection.issues.length > 0 && (
        <ul className="flex flex-col gap-2" aria-label="Checks on the selected video">
          {selection.issues.map((issue) => (
            <li key={issue.code}>
              <Alert variant={issue.severity === "error" ? "destructive" : "default"} data-issue={issue.code}>
                {issue.severity === "error" ? <TriangleAlert aria-hidden="true" /> : <Info aria-hidden="true" />}
                <AlertDescription className={cn(issue.severity === "warning" && "text-foreground")}>
                  {issue.message}
                </AlertDescription>
              </Alert>
            </li>
          ))}
        </ul>
      )}

      {phase.kind === "uploading" && (
        <UploadProgress
          percent={phase.progress.percent}
          loaded={phase.progress.loaded}
          total={phase.progress.total}
          retrying={phase.progress.retrying}
          completing={phase.progress.completing}
          onCancel={cancelUpload}
        />
      )}

      <div aria-live="polite" className="empty:hidden">
        {phase.kind === "redirecting" && (
          <p className="text-sm text-muted-foreground">Upload complete. Opening your job…</p>
        )}
        {phase.kind === "cancelled" && <p className="text-sm text-muted-foreground">Upload cancelled.</p>}
      </div>

      {phase.kind === "failed" && (
        <ErrorMessage title="The upload did not work" limits={limits} {...failureToError(phase.failure)}>
          {waitText && (
            <p aria-live="off" className="mt-1 font-medium" data-testid="upload-retry-wait">
              {waitText}
            </p>
          )}
        </ErrorMessage>
      )}

      {selection && (
        <div className="flex flex-col gap-2">
          <div className="flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
            <Button type="button" variant="outline" size="lg" onClick={reset} disabled={busy}>
              Choose a different video
            </Button>
            <Button type="button" size="lg" onClick={startUpload} disabled={busy || blocked || waiting}>
              {phase.kind === "failed" || phase.kind === "cancelled" ? "Try the upload again" : "Upload and find the best shots"}
            </Button>
          </div>
          {waiting && phase.kind !== "failed" && (
            <p className="text-sm text-muted-foreground sm:text-right" data-testid="upload-retry-wait">
              {waitText}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function SelectedVideo({ selection }: { selection: Selection }) {
  const { file, probe, probing } = selection;
  const meta = probe?.metadata;
  return (
    <section
      aria-label="Selected video"
      className="flex flex-col gap-4 rounded-xl border bg-card p-4 sm:flex-row sm:items-center"
    >
      <div className="flex aspect-video w-full shrink-0 items-center justify-center overflow-hidden rounded-lg bg-muted sm:w-56">
        {probe?.posterDataUrl ? (
          <img src={probe.posterDataUrl} alt={`First frames of ${file.name}`} className="size-full object-cover" />
        ) : (
          <span className="flex flex-col items-center gap-1 text-xs text-muted-foreground">
            <Film aria-hidden="true" className="size-6" />
            {probing ? "Loading preview…" : "No preview available"}
          </span>
        )}
      </div>
      <dl className="grid min-w-0 grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">File</dt>
        <dd className="truncate font-medium" title={file.name}>
          {file.name}
        </dd>
        <dt className="text-muted-foreground">Size</dt>
        <dd className="tabular-nums">{formatBytes(file.size)}</dd>
        <dt className="text-muted-foreground">Length</dt>
        <dd className="tabular-nums">{meta && meta.durationSec > 0 ? formatDuration(meta.durationSec) : probing ? "…" : "Unknown"}</dd>
        <dt className="text-muted-foreground">Resolution</dt>
        <dd className="tabular-nums">{meta && meta.width > 0 ? `${meta.width}×${meta.height}` : probing ? "…" : "Unknown"}</dd>
      </dl>
    </section>
  );
}
