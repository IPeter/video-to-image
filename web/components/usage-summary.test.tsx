import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { JobUsage } from "@/lib/api";
import { makeUsage, makeUsageStep, NO_USAGE } from "@/test/fixtures";

import { UsageSummary } from "./usage-summary";

/** Cell texts per row as a wide panel shows them (without the narrow-panel extras). */
function rowTexts(table: HTMLElement, section: "tbody" | "tfoot"): string[][] {
  const el = table.querySelector(section);
  if (!el) return [];
  return Array.from(el.querySelectorAll("tr")).map((tr) =>
    Array.from(tr.querySelectorAll("th, td")).map((cell) => {
      const copy = cell.cloneNode(true) as HTMLElement;
      copy.querySelectorAll("[data-narrow-only]").forEach((n) => n.remove());
      return copy.textContent ?? "";
    }),
  );
}

/** What a narrow panel shows under each row's label instead of the Model and Calls columns. */
function narrowTexts(table: HTMLElement): string[] {
  return Array.from(table.querySelectorAll("[data-narrow-only]")).map((n) => n.textContent ?? "");
}

describe("<UsageSummary>", () => {
  it("shows total tokens and cost in a collapsed line", () => {
    render(<UsageSummary usage={makeUsage()} />);
    const panel = screen.getByTestId("ai-usage");
    expect(panel.tagName).toBe("DETAILS");
    expect(panel).not.toHaveAttribute("open");
    expect(panel.querySelector("summary")).toHaveTextContent("AI usage: 9,012 tokens · $0.0041");
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · $0.0041");
  });

  it("lists each step with model, calls, tokens and cost when opened", async () => {
    const user = userEvent.setup();
    render(<UsageSummary usage={makeUsage()} />);
    const panel = screen.getByTestId("ai-usage");
    await user.click(within(panel).getByText(/AI usage:/));
    expect(panel).toHaveAttribute("open");

    const table = within(panel).getByRole("table", { name: "AI usage per step" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Step",
      "Model",
      "Calls",
      "Input tokens",
      "Output tokens",
      "Thinking tokens",
      "Cost",
    ]);
    expect(rowTexts(table, "tbody")).toEqual([
      ["Understanding the video", "gemini-3.5-flash-lite", "1", "6,200", "900", "400", "$0.0031"],
      ["Checking the photos", "gemini-3.5-flash-lite", "1", "1,200", "212", "100", "$0.001"],
    ]);
    expect(rowTexts(table, "tfoot")).toEqual([["Total", "", "2", "7,400", "1,112", "500", "$0.0041"]]);
    expect(narrowTexts(table)).toEqual(["gemini-3.5-flash-lite · 1 call", "gemini-3.5-flash-lite · 1 call", "2 calls"]);
    expect(panel).toHaveTextContent("estimated from the model's list prices");
  });

  it("labels the studio-background QA step", () => {
    render(
      <UsageSummary
        usage={makeUsage({
          calls: 3,
          steps: [
            makeUsageStep(),
            makeUsageStep({ step: "review" }),
            makeUsageStep({ step: "studio", model: "gemini-3.8-flash", calls: 1 }),
          ],
        })}
      />,
    );
    const table = screen.getByRole("table", { name: "AI usage per step" });
    expect(within(table).getAllByRole("rowheader").map((h) => h.firstChild?.textContent)).toEqual([
      "Understanding the video",
      "Checking the photos",
      "Studio check",
      "Total",
    ]);
  });

  it("keeps unknown step names, shows a dash for a missing model and omits the total for one step", () => {
    render(
      <UsageSummary
        usage={makeUsage({
          calls: 3,
          inputTokens: 40_000,
          outputTokens: 2000,
          thinkingTokens: 0,
          totalTokens: 42_000,
          costUsd: 0.00003,
          steps: [makeUsageStep({ step: "summarize", model: "", calls: 3, inputTokens: 40_000, outputTokens: 2000, thinkingTokens: 0, costUsd: 0.00003 })],
        })}
      />,
    );
    const table = screen.getByRole("table", { name: "AI usage per step" });
    expect(rowTexts(table, "tbody")).toEqual([["summarize", "—", "3", "40,000", "2,000", "0", "<$0.0001"]]);
    expect(narrowTexts(table)).toEqual(["— · 3 calls"]);
    expect(rowTexts(table, "tfoot")).toEqual([]);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("42,000 tokens · <$0.0001");
  });

  it("says free test mode when the API reports test mode (the fake analyzer)", () => {
    const steps = [
      makeUsageStep({ model: "fake:gemini-3.5-flash-lite", costUsd: 0 }),
      makeUsageStep({ step: "review", model: "fake:gemini-3.5-flash-lite", costUsd: 0 }),
    ];
    render(<UsageSummary usage={makeUsage({ costUsd: 0, testMode: true, steps })} />);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · $0.00 (free test mode)");
    const table = screen.getByRole("table", { name: "AI usage per step" });
    expect(rowTexts(table, "tbody").map((r) => r[6])).toEqual(["$0.00", "$0.00"]);
    expect(screen.getByTestId("ai-usage")).toHaveTextContent("Free test mode");
    expect(screen.queryByTestId("ai-usage-unpriced")).not.toBeInTheDocument();
  });

  it("does not call a real model at $0 free test mode", () => {
    const steps = [makeUsageStep({ costUsd: 0 }), makeUsageStep({ step: "review", costUsd: 0 })];
    render(<UsageSummary usage={makeUsage({ costUsd: 0, testMode: false, steps })} />);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · $0.00");
    expect(screen.getByTestId("ai-usage")).not.toHaveTextContent(/free test mode/i);
  });

  it("shows a lower bound and names the model without a price", () => {
    const steps = [
      makeUsageStep({ costUsd: 0.0041 }),
      makeUsageStep({ step: "review", model: "gemini-9-ultra", costUsd: 0, priced: false }),
    ];
    render(<UsageSummary usage={makeUsage({ costUsd: 0.0041, costComplete: false, steps })} />);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · at least $0.0041");
    const table = screen.getByRole("table", { name: "AI usage per step" });
    expect(rowTexts(table, "tbody").map((r) => r[6])).toEqual(["$0.0041", "unknown"]);
    expect(rowTexts(table, "tfoot")[0][6]).toBe("at least $0.0041");
    expect(screen.getByTestId("ai-usage-unpriced")).toHaveTextContent(
      "Price unknown for model gemini-9-ultra; the real cost is higher.",
    );
    expect(screen.getByTestId("ai-usage")).not.toHaveTextContent(/free test mode/i);
  });

  it("says the cost is unknown when no step has a price", () => {
    const steps = [makeUsageStep({ model: "gemini-9-ultra", costUsd: 0, priced: false })];
    render(<UsageSummary usage={makeUsage({ calls: 1, costUsd: 0, costComplete: false, steps })} />);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · cost unknown");
    expect(screen.getByTestId("ai-usage-unpriced")).toHaveTextContent("Price unknown for model gemini-9-ultra");
  });

  it("reads a payload without the price flags as priced and not test mode", () => {
    const legacy: Partial<JobUsage> = makeUsage({ costUsd: 0 });
    delete legacy.testMode;
    delete legacy.costComplete;
    render(<UsageSummary usage={legacy as JobUsage} />);
    expect(screen.getByTestId("ai-usage-total")).toHaveTextContent("9,012 tokens · $0.00");
    expect(screen.getByTestId("ai-usage")).not.toHaveTextContent(/free test mode/i);
    expect(screen.queryByTestId("ai-usage-unpriced")).not.toBeInTheDocument();
  });

  it("renders nothing before the first model call", () => {
    const { container, rerender } = render(<UsageSummary usage={NO_USAGE} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<UsageSummary usage={undefined} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<UsageSummary usage={makeUsage({ calls: 1 })} />);
    expect(screen.getByTestId("ai-usage")).toBeInTheDocument();
  });

  it("tolerates a payload without steps", () => {
    render(<UsageSummary usage={{ ...makeUsage(), steps: null as unknown as [] }} />);
    const table = screen.getByRole("table", { name: "AI usage per step" });
    expect(rowTexts(table, "tbody")).toEqual([]);
    expect(rowTexts(table, "tfoot")).toEqual([["Total", "", "2", "7,400", "1,112", "500", "$0.0041"]]);
  });
});
