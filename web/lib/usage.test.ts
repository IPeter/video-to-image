import { describe, expect, it } from "vitest";

import { makeUsage, makeUsageStep, NO_USAGE } from "@/test/fixtures";

import {
  formatCostUsd,
  formatJobCost,
  formatStepCost,
  formatTokenCount,
  formatTokens,
  formatTotalCost,
  formatUsageSummary,
  hasUsage,
  isCostComplete,
  isFreeTestMode,
  isStepPriced,
  totalTokens,
  unpricedNote,
  usageStepLabel,
} from "./usage";

describe("formatTokens", () => {
  it.each([
    [0, "0"],
    [7, "7"],
    [9012, "9,012"],
    [1_234_567, "1,234,567"],
    [12.6, "13"],
    [-5, "0"],
    [Number.NaN, "0"],
  ])("%s → %s", (n, want) => {
    expect(formatTokens(n)).toBe(want);
  });

  it("names the unit", () => {
    expect(formatTokenCount(1)).toBe("1 token");
    expect(formatTokenCount(0)).toBe("0 tokens");
    expect(formatTokenCount(9012)).toBe("9,012 tokens");
  });
});

describe("formatCostUsd", () => {
  it.each([
    [0, "$0.00"],
    [0.00004, "<$0.0001"],
    [0.000099, "<$0.0001"],
    [0.0001, "$0.0001"],
    [0.0041, "$0.0041"],
    [0.00416, "$0.0042"],
    [0.05, "$0.05"],
    [0.1, "$0.10"],
    [0.12345, "$0.1235"],
    [1, "$1.00"],
    [12.345, "$12.35"],
    [1234.5, "$1,234.50"],
    [-0.01, "$0.00"],
    [Number.NaN, "$0.00"],
  ])("%s → %s", (usd, want) => {
    expect(formatCostUsd(usd)).toBe(want);
  });
});

describe("job cost", () => {
  it("says free test mode only when the API reports test mode", () => {
    expect(isFreeTestMode({ costUsd: 0, testMode: true })).toBe(true);
    // A real model at $0 (no price yet) is not test mode.
    expect(isFreeTestMode({ costUsd: 0, testMode: false })).toBe(false);
    expect(isFreeTestMode({ costUsd: 0 })).toBe(false); // an API without the flag
    expect(formatJobCost({ costUsd: 0, testMode: true, costComplete: true })).toBe("$0.00 (free test mode)");
    expect(formatJobCost({ costUsd: 0, testMode: false, costComplete: true })).toBe("$0.00");
    expect(formatJobCost({ costUsd: 0.00002, testMode: false, costComplete: true })).toBe("<$0.0001");
    expect(formatJobCost({ costUsd: 0.0041, testMode: false, costComplete: true })).toBe("$0.0041");
  });

  it("gives a lower bound when a model has no price", () => {
    expect(isCostComplete({ costUsd: 0.0041, costComplete: false })).toBe(false);
    expect(isCostComplete({ costUsd: 0.0041 })).toBe(true); // an API without the flag
    expect(formatJobCost({ costUsd: 0.0041, testMode: false, costComplete: false })).toBe("at least $0.0041");
    expect(formatJobCost({ costUsd: 12.345, testMode: false, costComplete: false })).toBe("at least $12.35");
    expect(formatJobCost({ costUsd: 0.00002, testMode: false, costComplete: false })).toBe("cost unknown");
    expect(formatJobCost({ costUsd: 0, testMode: false, costComplete: false })).toBe("cost unknown");
    expect(formatTotalCost({ costUsd: 0.0041, costComplete: false })).toBe("at least $0.0041");
    expect(formatTotalCost({ costUsd: 0, costComplete: false })).toBe("unknown");
    expect(formatTotalCost({ costUsd: 0.0041, costComplete: true })).toBe("$0.0041");
  });

  it("marks steps without a price", () => {
    expect(isStepPriced({ costUsd: 0, priced: false })).toBe(false);
    expect(isStepPriced({ costUsd: 0 })).toBe(true); // an API without the flag
    expect(formatStepCost({ costUsd: 0, priced: false })).toBe("unknown");
    expect(formatStepCost({ costUsd: 0.0031, priced: true })).toBe("$0.0031");
    expect(formatStepCost({ costUsd: 0, priced: true })).toBe("$0.00");
  });

  it("names the models without a price in the footnote", () => {
    const step = (model: string, priced: boolean) => makeUsageStep({ model, priced, costUsd: priced ? 0.001 : 0 });
    expect(unpricedNote(makeUsage())).toBeNull();
    expect(unpricedNote(makeUsage({ costComplete: false, steps: [step("gemini-3.5-flash-lite", true), step("gemini-9-ultra", false)] }))).toBe(
      "Price unknown for model gemini-9-ultra; the real cost is higher.",
    );
    expect(
      unpricedNote(
        makeUsage({
          costComplete: false,
          steps: [step("gemini-9-ultra", false), step("gemini-9-ultra", false), step("gemini-10-pro", false)],
        }),
      ),
    ).toBe("Price unknown for models gemini-9-ultra and gemini-10-pro; the real cost is higher.");
    expect(unpricedNote(makeUsage({ costComplete: false, steps: [step("", false)] }))).toBe(
      "Price unknown for one of the models; the real cost is higher.",
    );
    expect(unpricedNote(makeUsage({ costComplete: false }))).toBe(
      "Part of the cost could not be determined; the real cost may be higher.",
    );
  });

  it("formats the summary line", () => {
    expect(formatUsageSummary(makeUsage())).toBe("9,012 tokens · $0.0041");
    expect(formatUsageSummary(makeUsage({ costUsd: 0, testMode: true }))).toBe("9,012 tokens · $0.00 (free test mode)");
    expect(formatUsageSummary(makeUsage({ costUsd: 0 }))).toBe("9,012 tokens · $0.00");
    expect(formatUsageSummary(makeUsage({ costComplete: false }))).toBe("9,012 tokens · at least $0.0041");
    expect(formatUsageSummary(makeUsage({ costUsd: 0, costComplete: false }))).toBe("9,012 tokens · cost unknown");
  });
});

describe("totalTokens", () => {
  it("uses the API total and sums the parts only without one", () => {
    expect(totalTokens(makeUsage())).toBe(9012);
    expect(totalTokens({ totalTokens: 0, inputTokens: 100, outputTokens: 20, thinkingTokens: 3 })).toBe(123);
  });
});

describe("hasUsage", () => {
  it("is true from the first model call", () => {
    expect(hasUsage(makeUsage())).toBe(true);
    expect(hasUsage(makeUsage({ calls: 1 }))).toBe(true);
    expect(hasUsage(NO_USAGE)).toBe(false);
    expect(hasUsage(undefined)).toBe(false);
    expect(hasUsage(null)).toBe(false);
  });
});

describe("usageStepLabel", () => {
  it("names the model steps and keeps unknown ones", () => {
    expect(usageStepLabel("analyze")).toBe("Understanding the video");
    expect(usageStepLabel("review")).toBe("Checking the photos");
    expect(usageStepLabel("rerank")).toBe("rerank");
    expect(usageStepLabel("constructor")).toBe("constructor");
  });
});
