import type { Metadata } from "next";
import { notFound } from "next/navigation";

import { JobView } from "@/components/job-view";

export const metadata: Metadata = {
  title: "Your images",
  // The job URL is the only handle on a result: keep it out of search engines.
  robots: { index: false, follow: false },
};

// Loose shape check only (job_ + ULID in practice); the API is the authority.
const JOB_ID_RE = /^job_[A-Za-z0-9]{1,64}$/;

export default async function JobPage({ params }: { params: Promise<{ jobId: string }> }) {
  const { jobId } = await params;
  if (!JOB_ID_RE.test(jobId)) notFound();
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-8 px-4 py-10 sm:px-6">
      <JobView jobId={jobId} />
    </div>
  );
}
