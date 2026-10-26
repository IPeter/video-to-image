import type { JobUsage, UsageStep } from "@/lib/api";

// A job's model usage (GET …/jobs/{id} → usage), phrased for people: token counts with
// thousands separators and costs precise enough for sub-cent model calls.

const tokenFormat = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });

/** 9012 → "9,012". Negative or non-finite counts read as 0. */
export function formatTokens(n: number): string {
  return tokenFormat.format(Number.isFinite(n) && n > 0 ? Math.round(n) : 0);
}

/** 1 → "1 token", 9012 → "9,012 tokens". */
export function formatTokenCount(n: number): string {
  return `${formatTokens(n)} ${n === 1 ? "token" : "tokens"}`;
}

/** Smallest cost written as a number; anything above zero but below it reads "<$0.0001". */
export const MIN_SHOWN_COST_USD = 0.0001;

const subDollar = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
});
const dollars = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" });

/**
 * A USD amount with enough precision for single model calls:
 * 0 → "$0.00", 0.00004 → "<$0.0001", 0.0041 → "$0.0041", 0.05 → "$0.05", 12.345 → "$12.35".
 */
export function formatCostUsd(usd: number): string {
  if (!Number.isFinite(usd) || usd <= 0) return "$0.00";
  if (usd < MIN_SHOWN_COST_USD) return `<${subDollar.format(MIN_SHOWN_COST_USD)}`;
  return (usd < 1 ? subDollar : dollars).format(usd);
}

/**
 * The price flags of the usage payload, optional here because an API from before the
 * price table omits them. That reads as "every step priced, not test mode", which is
 * what such an API meant.
 */
type UsageFlags = Pick<JobUsage, "costUsd"> & Partial<Pick<JobUsage, "testMode" | "costComplete">>;
type StepFlags = Pick<UsageStep, "costUsd"> & Partial<Pick<UsageStep, "priced">>;

/** Only what the API says: a real model at $0 (no price yet) is not test mode. */
export function isFreeTestMode(usage: UsageFlags): boolean {
  return usage.testMode === true;
}

/** Whether costUsd is the whole cost (no call to a model without a price). */
export function isCostComplete(usage: UsageFlags): boolean {
  return usage.costComplete !== false;
}

export function isStepPriced(step: StepFlags): boolean {
  return step.priced !== false;
}

/**
 * The total cost of the calls: "$0.0041", or "at least $0.0041" when some went to a model
 * without a price ("unknown" when the priced part is below what can be shown).
 */
export function formatTotalCost(usage: UsageFlags): string {
  if (isCostComplete(usage)) return formatCostUsd(usage.costUsd);
  return usage.costUsd >= MIN_SHOWN_COST_USD ? `at least ${formatCostUsd(usage.costUsd)}` : "unknown";
}

/** The job's cost for the summary line: formatTotalCost, "$0.00 (free test mode)", "cost unknown". */
export function formatJobCost(usage: UsageFlags): string {
  if (isFreeTestMode(usage)) return "$0.00 (free test mode)";
  if (!isCostComplete(usage) && usage.costUsd < MIN_SHOWN_COST_USD) return "cost unknown";
  return formatTotalCost(usage);
}

/** A step's cost cell: "unknown" for a model without a price. */
export function formatStepCost(step: StepFlags): string {
  return isStepPriced(step) ? formatCostUsd(step.costUsd) : "unknown";
}

const modelList = new Intl.ListFormat("en-US", { style: "long", type: "conjunction" });

type PricedSteps = UsageFlags & { steps?: readonly (Pick<UsageStep, "model"> & StepFlags)[] | null };

/**
 * The footnote when the cost is incomplete, naming the models without a price:
 * "Price unknown for model gemini-9-ultra; the real cost is higher." Null when complete.
 */
export function unpricedNote(usage: PricedSteps): string | null {
  if (isCostComplete(usage)) return null;
  const unpriced = (usage.steps ?? []).filter((s) => !isStepPriced(s));
  if (unpriced.length === 0) return "Part of the cost could not be determined; the real cost may be higher.";
  const models = [...new Set(unpriced.map((s) => s.model).filter((m) => m !== ""))];
  if (models.length === 0) return "Price unknown for one of the models; the real cost is higher.";
  return `Price unknown for ${models.length === 1 ? "model" : "models"} ${modelList.format(models)}; the real cost is higher.`;
}

/** Total tokens; summed from the parts when the API sent no total. */
export function totalTokens(usage: Pick<JobUsage, "totalTokens" | "inputTokens" | "outputTokens" | "thinkingTokens">): number {
  if (usage.totalTokens > 0) return usage.totalTokens;
  return usage.inputTokens + usage.outputTokens + usage.thinkingTokens;
}

/** The compact line under the results heading: "9,012 tokens · $0.0041". */
export function formatUsageSummary(usage: JobUsage): string {
  return `${formatTokenCount(totalTokens(usage))} · ${formatJobCost(usage)}`;
}

/**
 * Whether there is any usage to show. False before the first model call, and for a
 * payload without usage (an older API during a rolling deploy).
 */
export function hasUsage(usage: JobUsage | null | undefined): usage is JobUsage {
  return !!usage && usage.calls > 0;
}

const STEP_LABELS: ReadonlyMap<string, string> = new Map([
  ["analyze", "Understanding the video"],
  ["locate", "Finding the moments"],
  ["review", "Checking the photos"],
  ["studio", "Studio check"],
  ["studio-gen", "Studio backgrounds"],
]);

/** A usage step in plain language; steps this build does not know keep their API name. */
export function usageStepLabel(step: string): string {
  return STEP_LABELS.get(step) ?? step;
}
