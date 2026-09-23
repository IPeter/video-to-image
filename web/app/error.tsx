"use client";

import { useEffect } from "react";

import { Button } from "@/components/ui/button";

// Last-resort boundary for unexpected render errors below the root layout.
export default function RouteError({ error, retry }: { error: Error & { digest?: string }; retry: () => void }) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <section
      aria-labelledby="route-error-title"
      className="mx-auto flex max-w-lg flex-col items-center gap-4 px-4 py-20 text-center"
    >
      <h1 id="route-error-title" className="font-heading text-2xl font-semibold tracking-tight">
        Something went wrong
      </h1>
      <p className="text-muted-foreground">Please try again. If it keeps happening, come back in a few minutes.</p>
      <Button type="button" onClick={() => retry()}>
        Try again
      </Button>
    </section>
  );
}
