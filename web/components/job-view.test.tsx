import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError, type Api, type Job } from "@/lib/api";
import { API_BASE, JOB_ID, makeJob, makeResult, makeUsage, makeUsageStep } from "@/test/fixtures";
import { renderWithProviders } from "@/test/render";

import {
  jobRefetchInterval,
  jobRefetchOnFocus,
  jobRetryDelay,
  JobView,
  POLL_INTERVAL_MS,
  shouldRetryJobQuery,
} from "./job-view";

type Step = Job | Error;

/** An Api double whose getJob walks through `steps`, repeating the last one. */
function fakeApi(steps: Step[], deleteImpl: () => Promise<void> = async () => {}) {
  let i = 0;
  const getJob = vi.fn(async () => {
    const step = steps[Math.min(i, steps.length - 1)];
    i += 1;
    if (step instanceof Error) throw step;
    return step;
  });
  const deleteJob = vi.fn(deleteImpl);
  const api: Api = {
    client: {} as Api["client"],
    baseUrl: API_BASE,
    getJob,
    deleteJob,
    resolveApiUrl: (p) => `${API_BASE}${p}`,
  };
  return { api, getJob, deleteJob };
}

const completed = makeJob({
  status: "completed",
  progress: { step: null, percent: 100, steps: [] },
  usage: makeUsage(),
  result: makeResult(),
});

/** Usage after the gate's single analyze call. */
const analyzeOnly = makeUsage({
  calls: 1,
  inputTokens: 6200,
  outputTokens: 900,
  thinkingTokens: 400,
  totalTokens: 7500,
  costUsd: 0.0031,
  steps: [makeUsageStep()],
});

function renderJob(api: Api) {
  return renderWithProviders(<JobView jobId={JOB_ID} pollIntervalMs={20} />, { api });
}

describe("polling policy", () => {
  it("polls every 1.5 s until the job is terminal", () => {
    expect(POLL_INTERVAL_MS).toBe(1500);
    expect(jobRefetchInterval(undefined, null)).toBe(1500);
    expect(jobRefetchInterval(makeJob({ status: "queued" }), null)).toBe(1500);
    expect(jobRefetchInterval(makeJob({ status: "processing" }), null)).toBe(1500);
    for (const status of ["completed", "rejected", "failed", "expired"] as const) {
      expect(jobRefetchInterval(makeJob({ status }), null)).toBe(false);
    }
  });

  it("keeps polling through transient errors and stops on final ones", () => {
    const job = makeJob();
    expect(jobRefetchInterval(job, ApiError.network(new TypeError("offline")))).toBe(1500);
    expect(jobRefetchInterval(job, new ApiError("http", 503, null))).toBe(1500);
    expect(jobRefetchInterval(job, new ApiError("http", 410, "EXPIRED"))).toBe(false);
    expect(jobRefetchInterval(undefined, new ApiError("http", 404, "JOB_NOT_FOUND"))).toBe(false);
  });

  it("re-validates a completed job on window focus, but no other final answer", () => {
    expect(jobRefetchOnFocus(makeJob({ status: "completed" }), null)).toBe(true);
    expect(jobRefetchOnFocus(makeJob({ status: "processing" }), null)).toBe(true);
    expect(jobRefetchOnFocus(undefined, null)).toBe(true);
    for (const status of ["rejected", "failed", "expired"] as const) {
      expect(jobRefetchOnFocus(makeJob({ status }), null)).toBe(false);
    }
    expect(jobRefetchOnFocus(makeJob({ status: "completed" }), new ApiError("http", 410, "EXPIRED"))).toBe(false);
    expect(jobRefetchOnFocus(makeJob({ status: "completed" }), ApiError.network(null))).toBe(true);
  });

  it("retries transient errors forever with capped exponential backoff", () => {
    expect(shouldRetryJobQuery(50, ApiError.network(null))).toBe(true);
    expect(shouldRetryJobQuery(1, new ApiError("http", 429, "RATE_LIMITED"))).toBe(true);
    expect(shouldRetryJobQuery(1, new ApiError("http", 404, "JOB_NOT_FOUND"))).toBe(false);
    expect([1, 2, 3, 4, 5, 6, 10].map(jobRetryDelay)).toEqual([1000, 2000, 4000, 8000, 15000, 15000, 15000]);
  });
});

describe("<JobView>", () => {
  it("shows progress while processing, then the results, then stops polling", async () => {
    const queued = makeJob({ status: "queued", progress: { step: null, percent: 0, steps: [] } });
    const processing = makeJob();
    const { api, getJob } = fakeApi([queued, processing, completed]);
    renderJob(api);

    expect(await screen.findByRole("heading", { name: "Your video is in line" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Processing your video" })).toBeInTheDocument();
    expect(screen.getByTestId("job-steps-headline")).toHaveTextContent("Understanding the video");

    expect(await screen.findByRole("list", { name: "Selected images" })).toBeInTheDocument();
    expect(getJob).toHaveBeenCalledWith("cars", JOB_ID, expect.any(AbortSignal));
    const calls = getJob.mock.calls.length;
    await new Promise((r) => setTimeout(r, 120));
    expect(getJob.mock.calls.length).toBe(calls);
  });

  it("shows the AI usage with the results", async () => {
    const { api } = fakeApi([completed]);
    renderJob(api);
    await screen.findByRole("list", { name: "Selected images" });
    expect(screen.getByTestId("ai-usage")).toHaveTextContent("AI usage: 9,012 tokens · $0.0041");
  });

  it("shows the AI usage while processing only once a model call was made", async () => {
    const { api } = fakeApi([makeJob(), makeJob({ usage: analyzeOnly }), completed]);
    renderJob(api);
    expect(await screen.findByTestId("job-processing")).toBeInTheDocument();
    expect(screen.queryByTestId("ai-usage")).not.toBeInTheDocument();
    expect(await screen.findByTestId("ai-usage")).toHaveTextContent("7,500 tokens · $0.0031");
    expect(screen.getByTestId("job-processing")).toContainElement(screen.getByTestId("ai-usage"));
    await screen.findByRole("list", { name: "Selected images" });
    expect(screen.getByTestId("ai-usage")).toHaveTextContent("9,012 tokens · $0.0041");
  });

  it("shows what a rejected or failed job cost", async () => {
    const rejected = makeJob({
      status: "rejected",
      usage: analyzeOnly,
      rejection: { reason: "A kitchen.", detectedSubject: "room" },
    });
    const first = fakeApi([rejected]);
    const { unmount } = renderJob(first.api);
    const section = await screen.findByTestId("job-rejected");
    expect(within(section).getByTestId("ai-usage")).toHaveTextContent("7,500 tokens · $0.0031");
    unmount();

    const failed = makeJob({ status: "failed", usage: analyzeOnly, error: { code: "MODEL_BAD_OUTPUT", message: "x" } });
    renderJob(fakeApi([failed]).api);
    const failedSection = await screen.findByTestId("job-failed");
    expect(within(failedSection).getByTestId("ai-usage")).toHaveTextContent("7,500 tokens · $0.0031");
  });

  it("shows no AI usage for a job that failed before any model call", async () => {
    renderJob(fakeApi([makeJob({ status: "failed", error: { code: "PROCESSING_ERROR", message: "x" } })]).api);
    expect(await screen.findByTestId("job-failed")).toBeInTheDocument();
    expect(screen.queryByTestId("ai-usage")).not.toBeInTheDocument();
  });

  it("renders a rejection as a first-class outcome", async () => {
    const { api } = fakeApi([
      makeJob({
        status: "rejected",
        rejection: { reason: "The video shows a kitchen interior, not a vehicle.", detectedSubject: "room interior" },
      }),
    ]);
    renderJob(api);
    const section = await screen.findByTestId("job-rejected");
    expect(within(section).getByRole("heading")).toHaveTextContent("This doesn't look like a car video");
    expect(section).toHaveTextContent("The video shows a kitchen interior, not a vehicle.");
    expect(within(section).getByRole("link", { name: "Try another video" })).toHaveAttribute("href", "/");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders a failure with the mapped message and the job id", async () => {
    const { api } = fakeApi([
      makeJob({ status: "failed", error: { code: "MODEL_UNAVAILABLE", message: "upstream 503 from gemini" } }),
    ]);
    renderJob(api);
    const section = await screen.findByTestId("job-failed");
    expect(section).toHaveTextContent("Sorry, we could not process this video");
    expect(section).toHaveTextContent("analysis service is busy or unavailable");
    expect(section).not.toHaveTextContent("gemini");
    expect(section).toHaveTextContent(JOB_ID);
    expect(within(section).getByRole("link", { name: "Try again" })).toHaveAttribute("href", "/");
  });

  it("shows the expired state for 410 EXPIRED without retrying", async () => {
    const { api, getJob } = fakeApi([new ApiError("http", 410, "EXPIRED")]);
    renderJob(api);
    expect(await screen.findByRole("heading", { name: "These images have expired" })).toBeInTheDocument();
    expect(screen.getByTestId("job-expired")).toHaveTextContent("24 hours");
    await new Promise((r) => setTimeout(r, 80));
    expect(getJob).toHaveBeenCalledTimes(1);
  });

  it("shows the expired state for status expired", async () => {
    const { api } = fakeApi([makeJob({ status: "expired" })]);
    renderJob(api);
    expect(await screen.findByTestId("job-expired")).toBeInTheDocument();
  });

  it("shows the not-found state for 404 JOB_NOT_FOUND", async () => {
    const { api } = fakeApi([new ApiError("http", 404, "JOB_NOT_FOUND")]);
    renderJob(api);
    expect(await screen.findByRole("heading", { name: "We could not find this job" })).toBeInTheDocument();
  });

  it("maps other final errors to copy and offers a retry", async () => {
    const { api, getJob } = fakeApi([new ApiError("http", 403, "ORIGIN_NOT_ALLOWED"), makeJob()]);
    const user = userEvent.setup();
    renderJob(api);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("not allowed to use the processing service");
    await user.click(within(alert).getByRole("button", { name: "Try again" }));
    expect(await screen.findByTestId("job-processing")).toBeInTheDocument();
    expect(getJob.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it("keeps polling through a network error", async () => {
    const { api } = fakeApi([makeJob(), ApiError.network(new TypeError("Failed to fetch")), completed]);
    renderJob(api);
    expect(await screen.findByTestId("job-processing")).toBeInTheDocument();
    expect(await screen.findByText(/trouble reaching the server/)).toBeInTheDocument();
    expect(await screen.findByRole("list", { name: "Selected images" }, { timeout: 4000 })).toBeInTheDocument();
    expect(screen.queryByText(/trouble reaching the server/)).not.toBeInTheDocument();
  });

  it("deletes the job on request and confirms the deletion (not 'expired')", async () => {
    const { api, deleteJob, getJob } = fakeApi([completed]);
    const user = userEvent.setup();
    renderJob(api);
    await user.click(await screen.findByRole("button", { name: "Delete now" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete now" }));

    const section = await screen.findByTestId("job-deleted");
    const heading = within(section).getByRole("heading", { level: 1 });
    expect(heading).toHaveTextContent("Your images and video were deleted");
    expect(within(section).getByRole("link", { name: "Upload another video" })).toHaveAttribute("href", "/");
    expect(screen.queryByTestId("job-expired")).not.toBeInTheDocument();
    expect(screen.queryByText(/expired/i)).not.toBeInTheDocument();
    // Focus does not fall back to <body> when the dialog and its trigger disappear.
    await waitFor(() => expect(heading).toHaveFocus());
    expect(screen.getByTestId("job-announcer")).toHaveTextContent("Your images and video were deleted.");
    expect(deleteJob).toHaveBeenCalledWith("cars", JOB_ID);
    expect(getJob).toHaveBeenCalledTimes(1);
  });

  it("treats deleting an already-deleted job as success", async () => {
    for (const status of [404, 410]) {
      const { api } = fakeApi([completed], async () => {
        throw new ApiError("http", status, status === 404 ? "JOB_NOT_FOUND" : "EXPIRED");
      });
      const user = userEvent.setup();
      const { unmount } = renderJob(api);
      await user.click(await screen.findByRole("button", { name: "Delete now" }));
      await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete now" }));
      expect(await screen.findByTestId("job-deleted")).toBeInTheDocument();
      unmount();
    }
  });

  it("reports a failed delete and keeps the results", async () => {
    const { api } = fakeApi([completed], async () => {
      throw new ApiError("http", 500, "INTERNAL_ERROR");
    });
    const user = userEvent.setup();
    renderJob(api);
    await user.click(await screen.findByRole("button", { name: "Delete now" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete now" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("could not be deleted"));
    expect(screen.getByRole("list", { name: "Selected images" })).toBeInTheDocument();
  });

  it("announces the outcome and focuses the results heading when processing finishes", async () => {
    const { api } = fakeApi([makeJob(), makeJob(), completed]);
    renderJob(api);
    expect(await screen.findByTestId("job-processing")).toBeInTheDocument();
    const announcer = screen.getByTestId("job-announcer");
    expect(announcer).toHaveAttribute("aria-live", "polite");
    expect(announcer).toBeEmptyDOMElement();

    const heading = await screen.findByRole("heading", { level: 1, name: completed.result!.summary });
    // The same live-region element survives the switch, so screen readers announce it.
    expect(screen.getByTestId("job-announcer")).toBe(announcer);
    expect(announcer).toHaveTextContent("Processing finished: 3 images ready.");
    await waitFor(() => expect(heading).toHaveFocus());
  });

  it.each([
    [makeJob({ status: "rejected", rejection: { reason: "A kitchen.", detectedSubject: "room" } }), "job-rejected", "This doesn't look like a car video"],
    [makeJob({ status: "failed", error: { code: "PROCESSING_ERROR", message: "x" } }), "job-failed", "Sorry, we could not process this video."],
  ])("announces and focuses a final outcome after processing (%#)", async (final, testId, text) => {
    const { api } = fakeApi([makeJob(), final]);
    renderJob(api);
    const section = await screen.findByTestId(testId);
    expect(screen.getByTestId("job-announcer")).toHaveTextContent(text);
    await waitFor(() => expect(within(section).getByRole("heading", { level: 1 })).toHaveFocus());
  });

  it("does not announce or steal focus when a finished job is opened", async () => {
    const { api } = fakeApi([completed]);
    renderJob(api);
    const heading = await screen.findByRole("heading", { level: 1, name: completed.result!.summary });
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.getByTestId("job-announcer")).toBeEmptyDOMElement();
    expect(heading).not.toHaveFocus();
  });
});

describe("<JobView> expiry and re-validation", () => {
  afterEach(() => {
    focusManager.setFocused(undefined);
  });

  it("shows the expired state straight away for a completed job past expiresAt", async () => {
    const stale = { ...completed, expiresAt: new Date(Date.now() - 60_000).toISOString() };
    const { api } = fakeApi([stale]);
    renderJob(api);
    expect(await screen.findByTestId("job-expired")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Selected images" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Download all/ })).not.toBeInTheDocument();
  });

  it("switches to the expired state at expiresAt and re-validates with the API", async () => {
    const soon = { ...completed, expiresAt: new Date(Date.now() + 300).toISOString() };
    const { api, getJob } = fakeApi([soon, new ApiError("http", 410, "EXPIRED")]);
    renderJob(api);
    expect(await screen.findByRole("list", { name: "Selected images" })).toBeInTheDocument();
    expect(getJob).toHaveBeenCalledTimes(1);

    const heading = await screen.findByRole("heading", { name: "These images have expired" }, { timeout: 2000 });
    await waitFor(() => expect(getJob).toHaveBeenCalledTimes(2));
    expect(screen.getByTestId("job-announcer")).toHaveTextContent("These images have expired.");
    await waitFor(() => expect(heading).toHaveFocus());
  });

  it.each([
    [new ApiError("http", 410, "EXPIRED"), "job-expired"],
    [new ApiError("http", 404, "JOB_NOT_FOUND"), "job-not-found"],
  ])("re-validates a completed job on window focus: %s", async (answer, testId) => {
    const { api, getJob } = fakeApi([completed, answer]);
    renderJob(api);
    expect(await screen.findByRole("list", { name: "Selected images" })).toBeInTheDocument();
    expect(getJob).toHaveBeenCalledTimes(1);

    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    expect(await screen.findByTestId(testId)).toBeInTheDocument();
    expect(getJob).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("link", { name: /Download all/ })).not.toBeInTheDocument();
  });

  it("does not re-validate other final states on focus", async () => {
    const { api, getJob } = fakeApi([makeJob({ status: "failed", error: null })]);
    renderJob(api);
    expect(await screen.findByTestId("job-failed")).toBeInTheDocument();
    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(getJob).toHaveBeenCalledTimes(1);
  });
});
