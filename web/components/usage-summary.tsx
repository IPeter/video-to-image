import { cn } from "cn";
import { ChevronRight, Sparkles } from "lucide-react";
import type { ReactNode } from "react";

import type { JobUsage } from "@/lib/api";
import { pluralize } from "@/lib/format";
import {
  formatStepCost,
  formatTokens,
  formatTotalCost,
  formatUsageSummary,
  hasUsage,
  isFreeTestMode,
  unpricedNote,
  usageStepLabel,
} from "@/lib/usage";

export interface UsageSummaryProps {
  /** The job's usage; nothing renders until the first model call. */
  usage: JobUsage | null | undefined;
  className?: string;
}

/**
 * "AI usage: 9,012 tokens · $0.0041" with a disclosure listing each step's model, calls,
 * input / output / thinking tokens and cost. A native <details>, so it needs no script
 * and keyboard and screen-reader support come with it. "Free test mode" only when the API
 * says so (usage.testMode); a model without a price makes the cost "at least $X" with a
 * footnote naming it.
 */
export function UsageSummary({ usage, className }: UsageSummaryProps) {
  if (!hasUsage(usage)) return null;
  const steps = usage.steps ?? [];
  const note = unpricedNote(usage);
  return (
    <details className={cn("group/usage text-sm text-muted-foreground", className)} data-testid="ai-usage">
      <summary className="inline-flex cursor-pointer list-none items-center gap-1.5 rounded-md outline-none select-none hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden">
        <Sparkles aria-hidden="true" className="size-3.5 shrink-0" />
        <span>
          AI usage:{" "}
          <span className="font-medium tabular-nums text-foreground" data-testid="ai-usage-total">
            {formatUsageSummary(usage)}
          </span>
        </span>
        <ChevronRight aria-hidden="true" className="size-3.5 shrink-0 transition-transform group-open/usage:rotate-90" />
      </summary>

      <div className="@container mt-2 flex flex-col gap-2 text-left">
        <div className="w-full max-w-2xl overflow-x-auto rounded-lg border">
          <table className="w-full text-xs tabular-nums" data-testid="ai-usage-steps">
            <caption className="sr-only">AI usage per step</caption>
            <thead className="bg-muted/50 text-muted-foreground">
              <tr>
                <th scope="col" className="px-2 py-2 @md:px-3 text-left font-medium">
                  Step
                </th>
                <th scope="col" className={cn("px-2 py-2 @md:px-3 text-left font-medium", WIDE_ONLY)}>
                  Model
                </th>
                <NumberHeader className={WIDE_ONLY}>Calls</NumberHeader>
                <NumberHeader>
                  Input<span className="sr-only"> tokens</span>
                </NumberHeader>
                <NumberHeader>
                  Output<span className="sr-only"> tokens</span>
                </NumberHeader>
                <NumberHeader>
                  Thinking<span className="sr-only"> tokens</span>
                </NumberHeader>
                <NumberHeader>Cost</NumberHeader>
              </tr>
            </thead>
            <tbody className="text-foreground">
              {steps.map((s, i) => (
                <tr key={`${s.step}-${s.model}-${i}`} className="border-t align-top" data-step={s.step}>
                  <th scope="row" className="px-2 py-2 @md:px-3 text-left font-medium">
                    {usageStepLabel(s.step)}
                    {/* Narrow panel: model and calls under the label instead of in their own columns. */}
                    <span className={cn("block font-normal text-muted-foreground", NARROW_ONLY)} data-narrow-only="">
                      <span className="font-mono break-all">{s.model || "—"}</span> · {pluralize(s.calls, "call")}
                    </span>
                  </th>
                  <td className={cn("px-2 py-2 @md:px-3 font-mono whitespace-nowrap text-muted-foreground", WIDE_ONLY)}>
                    {s.model || "—"}
                  </td>
                  <NumberCell className={WIDE_ONLY}>{formatTokens(s.calls)}</NumberCell>
                  <NumberCell>{formatTokens(s.inputTokens)}</NumberCell>
                  <NumberCell>{formatTokens(s.outputTokens)}</NumberCell>
                  <NumberCell>{formatTokens(s.thinkingTokens)}</NumberCell>
                  <NumberCell>{formatStepCost(s)}</NumberCell>
                </tr>
              ))}
            </tbody>
            {steps.length !== 1 && (
              <tfoot className="border-t font-medium text-foreground">
                <tr className="align-top">
                  <th scope="row" className="px-2 py-2 @md:px-3 text-left">
                    Total
                    <span className={cn("block font-normal text-muted-foreground", NARROW_ONLY)} data-narrow-only="">
                      {pluralize(usage.calls, "call")}
                    </span>
                  </th>
                  <td className={WIDE_ONLY} />
                  <NumberCell className={WIDE_ONLY}>{formatTokens(usage.calls)}</NumberCell>
                  <NumberCell>{formatTokens(usage.inputTokens)}</NumberCell>
                  <NumberCell>{formatTokens(usage.outputTokens)}</NumberCell>
                  <NumberCell>{formatTokens(usage.thinkingTokens)}</NumberCell>
                  <NumberCell>{formatTotalCost(usage)}</NumberCell>
                </tr>
              </tfoot>
            )}
          </table>
        </div>
        <p className="text-xs">
          {isFreeTestMode(usage)
            ? "Free test mode: the built-in test analyzer answered these calls, so nothing was charged."
            : "Costs are estimated from the model's list prices at the time of each call."}
        </p>
        {note && (
          <p className="text-xs font-medium text-foreground" data-testid="ai-usage-unpriced">
            {note}
          </p>
        )}
      </div>
    </details>
  );
}

// Model and calls get their own columns when the panel is at least 42rem wide (container
// query: the panel sits in columns of different widths); narrower, they sit under the step
// label so the token and cost columns fit a phone screen or the rejected/failed state.
const WIDE_ONLY = "hidden @2xl:table-cell";
const NARROW_ONLY = "@2xl:hidden";

function NumberHeader({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <th scope="col" className={cn("px-2 py-2 @md:px-3 text-right font-medium whitespace-nowrap", className)}>
      {children}
    </th>
  );
}

function NumberCell({ children, className }: { children: ReactNode; className?: string }) {
  return <td className={cn("px-2 py-2 @md:px-3 text-right whitespace-nowrap", className)}>{children}</td>;
}
