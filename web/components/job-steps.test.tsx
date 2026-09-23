import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { Progress } from "@/lib/api";

import { combineStatuses, deriveStepRows, JobSteps, STEP_ROWS } from "./job-steps";

const allSteps = ["preview", "analyze", "extract", "score", "select", "export"] as const;

function progress(overrides: Partial<Progress> = {}): Progress {
  return { step: null, percent: 0, steps: [], ...overrides };
}

describe("STEP_ROWS", () => {
  it("maps every pipeline step to exactly one friendly row", () => {
    const mapped = STEP_ROWS.flatMap((r) => r.steps);
    expect([...mapped].sort()).toEqual([...allSteps].sort());
    expect(STEP_ROWS.map((r) => r.label)).toEqual([
      "Preparing video",
      "Understanding the video",
      "Extracting frames",
      "Picking the best shots",
      "Packaging",
    ]);
    expect(STEP_ROWS.find((r) => r.id === "pick")?.steps).toEqual(["score", "select"]);
  });
});

describe("combineStatuses", () => {
  it.each([
    [[], "pending"],
    [["pending", "pending"], "pending"],
    [["done", "pending"], "running"],
    [["done", "running"], "running"],
    [["done", "done"], "done"],
    [["done", "skipped"], "done"],
    [["skipped", "skipped"], "skipped"],
    [["done", "failed"], "failed"],
    [["running", "failed"], "failed"],
  ] as const)("%j → %s", (statuses, want) => {
    expect(combineStatuses(statuses)).toBe(want);
  });
});

describe("deriveStepRows", () => {
  it("treats steps the API has not listed yet as pending", () => {
    const rows = deriveStepRows(progress({ step: "preview", steps: [{ name: "preview", status: "running" }] }));
    expect(rows.map((r) => r.status)).toEqual(["running", "pending", "pending", "pending", "pending"]);
    expect(rows.map((r) => r.current)).toEqual([true, false, false, false, false]);
  });

  it("collapses score and select into one row and marks it current", () => {
    const rows = deriveStepRows(
      progress({
        step: "select",
        steps: [
          { name: "preview", status: "done", ms: 2100 },
          { name: "analyze", status: "done", ms: 9000 },
          { name: "extract", status: "done", ms: 4000 },
          { name: "score", status: "done", ms: 1500 },
          { name: "select", status: "running" },
        ],
      }),
    );
    expect(rows).toHaveLength(5);
    const pick = rows[3];
    expect(pick).toMatchObject({ id: "pick", status: "running", current: true, ms: null });
    expect(rows[0]).toMatchObject({ status: "done", ms: 2100 });
  });

  it("sums the durations of a finished collapsed row", () => {
    const rows = deriveStepRows(
      progress({
        steps: [
          { name: "score", status: "done", ms: 1500 },
          { name: "select", status: "done", ms: 500 },
        ],
      }),
    );
    expect(rows[3]).toMatchObject({ status: "done", ms: 2000, current: false });
  });

  it("falls back to the running row when progress.step is null", () => {
    const rows = deriveStepRows(progress({ steps: [{ name: "preview", status: "done" }, { name: "extract", status: "running" }] }));
    expect(rows.findIndex((r) => r.current)).toBe(2);
  });

  it("has no current row while queued", () => {
    expect(deriveStepRows(progress()).some((r) => r.current)).toBe(false);
  });
});

describe("<JobSteps>", () => {
  const midway = progress({
    step: "analyze",
    percent: 35,
    steps: [
      { name: "preview", status: "done", ms: 2100 },
      { name: "analyze", status: "running" },
    ],
  });

  it("renders the friendly labels, ticks done steps and highlights the current one", () => {
    render(<JobSteps progress={midway} status="processing" elapsedSec={42} />);
    const list = screen.getByRole("list", { name: "Processing steps" });
    const items = within(list).getAllByRole("listitem");
    expect(items.map((li) => li.querySelector("span.flex-1")?.textContent)).toEqual([
      "Preparing video",
      "Understanding the video",
      "Extracting frames",
      "Picking the best shots",
      "Packaging",
    ]);
    expect(items[0]).toHaveAttribute("data-status", "done");
    expect(items[0]).toHaveTextContent("done");
    expect(items[0]).toHaveTextContent("2.1 s");
    expect(items[1]).toHaveAttribute("aria-current", "step");
    expect(items[1]).toHaveTextContent("in progress");
    expect(items.filter((li) => li.getAttribute("aria-current") === "step")).toHaveLength(1);
    expect(items[2]).toHaveAttribute("data-status", "pending");

    expect(screen.getByTestId("job-steps-headline")).toHaveTextContent("Step 2 of 5: Understanding the video");
    expect(screen.getByRole("progressbar", { name: "Processing progress" })).toHaveAttribute("aria-valuenow", "35");
    expect(screen.getByText(/Elapsed 0:42/)).toBeInTheDocument();
  });

  it("shows a waiting message while queued", () => {
    render(<JobSteps progress={progress()} status="queued" />);
    expect(screen.getByTestId("job-steps-headline")).toHaveTextContent("Waiting to start");
    expect(screen.queryByText(/Elapsed/)).not.toBeInTheDocument();
  });

  it("clamps the percentage", () => {
    render(<JobSteps progress={progress({ percent: 140 })} status="processing" />);
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "100");
  });
});
