"use client";

import { RotateCw } from "lucide-react";

import { Button } from "@/components/ui/button";

/** Reloads the current page (full navigation, so server-side config is fetched again). */
export function ReloadButton({ label = "Try again" }: { label?: string }) {
  return (
    <Button type="button" variant="outline" onClick={() => window.location.reload()}>
      <RotateCw aria-hidden="true" />
      {label}
    </Button>
  );
}
