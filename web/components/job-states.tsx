import { CircleCheck, CircleHelp, Clock3, SearchX, TriangleAlert } from "lucide-react";
import Link from "next/link";
import type { ReactNode } from "react";

import { messageForCode } from "@/components/error-message";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import type { JobError, Rejection } from "@/lib/api";

// Terminal and loading states of the job page. Each renders the page's <h1>, with the
// id `${testId}-title` and tabIndex -1 so JobView can move focus to it when the page
// switches to that state (the previous content, and the focus in it, is gone).

/** Id of a state's <h1>, the focus target after a transition (see JobView). */
export function stateHeadingId(testId: string): string {
  return `${testId}-title`;
}

function StateLayout({
  icon,
  title,
  children,
  testId,
}: {
  icon: ReactNode;
  title: string;
  children: ReactNode;
  testId: string;
}) {
  return (
    <section
      aria-labelledby={stateHeadingId(testId)}
      data-testid={testId}
      className="mx-auto flex w-full max-w-xl flex-col items-center gap-4 py-10 text-center"
    >
      <span className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">{icon}</span>
      <h1
        id={stateHeadingId(testId)}
        tabIndex={-1}
        className="font-heading text-2xl font-semibold tracking-tight text-balance outline-none"
      >
        {title}
      </h1>
      {children}
    </section>
  );
}

function UploadAgain({ label = "Upload a video" }: { label?: string }) {
  return (
    <Button asChild size="lg">
      <Link href="/">{label}</Link>
    </Button>
  );
}

/** The gate refused the video. A normal outcome, so it is styled neutrally, not as an error. */
export function RejectedState({ title, rejection }: { title: string; rejection: Rejection | null }) {
  return (
    <StateLayout icon={<CircleHelp aria-hidden="true" className="size-6" />} title={title} testId="job-rejected">
      {rejection?.reason && <p className="text-pretty text-muted-foreground">{rejection.reason}</p>}
      {rejection?.detectedSubject && (
        <p className="text-sm text-muted-foreground">
          What we saw: <span className="font-medium text-foreground">{rejection.detectedSubject}</span>
        </p>
      )}
      <UploadAgain label="Try another video" />
    </StateLayout>
  );
}

export function FailedState({ jobId, error }: { jobId: string; error: JobError | null }) {
  return (
    <StateLayout
      icon={<TriangleAlert aria-hidden="true" className="size-6" />}
      title="Sorry, we could not process this video"
      testId="job-failed"
    >
      <p className="text-pretty text-muted-foreground">{messageForCode(error?.code ?? "PROCESSING_ERROR")}</p>
      <UploadAgain label="Try again" />
      <p className="text-xs text-muted-foreground">
        If you contact support, mention this reference:{" "}
        <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-foreground select-all">{jobId}</code>
      </p>
    </StateLayout>
  );
}

export function ExpiredState({ retentionHours }: { retentionHours?: number }) {
  return (
    <StateLayout icon={<Clock3 aria-hidden="true" className="size-6" />} title="These images have expired" testId="job-expired">
      <p className="text-pretty text-muted-foreground">
        {retentionHours
          ? `Images are kept for ${retentionHours} hours after upload, or until you delete them. `
          : "Images are only kept for a limited time. "}
        Upload the video again to get new ones.
      </p>
      <UploadAgain />
    </StateLayout>
  );
}

/** The user deleted the job with "Delete now": confirm it, do not call it expired. */
export function DeletedState() {
  return (
    <StateLayout
      icon={<CircleCheck aria-hidden="true" className="size-6" />}
      title="Your images and video were deleted"
      testId="job-deleted"
    >
      <p className="text-pretty text-muted-foreground">
        The images, the ZIP and your video were removed from our servers. This link no longer works.
      </p>
      <UploadAgain label="Upload another video" />
    </StateLayout>
  );
}

export function JobNotFoundState() {
  return (
    <StateLayout icon={<SearchX aria-hidden="true" className="size-6" />} title="We could not find this job" testId="job-not-found">
      <p className="text-pretty text-muted-foreground">{messageForCode("JOB_NOT_FOUND")}</p>
      <UploadAgain />
    </StateLayout>
  );
}

/** Placeholder while the first job status loads. */
export function JobLoadingState({ reconnecting = false }: { reconnecting?: boolean }) {
  return (
    <section
      aria-busy="true"
      aria-labelledby="job-loading-title"
      className="mx-auto flex w-full max-w-2xl flex-col gap-5"
      data-testid="job-loading"
    >
      <h1 id="job-loading-title" className="sr-only">
        Loading your job
      </h1>
      <Skeleton className="h-8 w-2/3" />
      <Skeleton className="h-2 w-full" />
      <div className="flex flex-col gap-2">
        {Array.from({ length: 5 }, (_, i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
      <ConnectionNotice show={reconnecting} />
    </section>
  );
}

/** Polite notice while polling retries after network or server errors. */
export function ConnectionNotice({ show }: { show: boolean }) {
  return (
    <p aria-live="polite" className="text-sm text-muted-foreground empty:hidden">
      {show ? "We are having trouble reaching the server. Retrying…" : ""}
    </p>
  );
}
