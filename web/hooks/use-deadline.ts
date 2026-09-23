"use client";

import { useEffect, useState } from "react";

/** setTimeout clamps longer delays (~24.8 days) to 0; longer waits are re-armed in steps. */
const MAX_TIMEOUT_MS = 2_147_483_647;

/**
 * True once the clock reaches `deadlineMs` (epoch ms), and re-renders the component at
 * that moment with a single timer. Null or NaN never passes. A deadline that was over
 * when the component mounted is reported on the first render; one that arrives later
 * already over is reported right after that commit.
 */
export function useDeadlinePassed(deadlineMs: number | null): boolean {
  const [now, setNow] = useState(() => Date.now());
  const valid = deadlineMs !== null && Number.isFinite(deadlineMs);

  useEffect(() => {
    if (deadlineMs === null || !Number.isFinite(deadlineMs)) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = () => {
      const t = Date.now();
      if (t >= deadlineMs) {
        // Keep the old value when it already counted as passed (no extra render).
        setNow((prev) => (prev >= deadlineMs ? prev : t));
      } else {
        arm();
      }
    };
    const arm = () => {
      timer = setTimeout(tick, Math.min(Math.max(0, deadlineMs - Date.now()), MAX_TIMEOUT_MS));
    };
    arm();
    return () => clearTimeout(timer);
  }, [deadlineMs]);

  return valid && now >= deadlineMs;
}
