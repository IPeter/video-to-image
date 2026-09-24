import "@testing-library/jest-dom/vitest";

import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

import { resetUnsavedBackgroundPreference } from "@/hooks/use-background-preference";

afterEach(() => {
  cleanup();
  // The photo background choice is remembered per browser: start every test without one.
  try {
    window.localStorage.clear();
  } catch {
    // no storage in this environment
  }
  resetUnsavedBackgroundPreference();
});
