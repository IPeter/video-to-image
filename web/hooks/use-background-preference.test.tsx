import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  BACKGROUND_STORAGE_KEY,
  readBackgroundPreference,
  resetUnsavedBackgroundPreference,
  useBackgroundPreference,
  writeBackgroundPreference,
} from "./use-background-preference";

afterEach(() => {
  window.localStorage.clear();
  resetUnsavedBackgroundPreference();
});

describe("useBackgroundPreference", () => {
  it("is null until something is chosen, then remembers it in localStorage", () => {
    const { result } = renderHook(() => useBackgroundPreference());
    expect(result.current[0]).toBeNull();

    act(() => result.current[1]({ mode: "original", style: "light" }));
    expect(result.current[0]).toEqual({ mode: "original", style: "light" });
    expect(JSON.parse(window.localStorage.getItem(BACKGROUND_STORAGE_KEY)!)).toEqual({ mode: "original", style: "light" });
  });

  it("reads a choice made earlier (another page load)", () => {
    window.localStorage.setItem(BACKGROUND_STORAGE_KEY, JSON.stringify({ mode: "studio", style: "dark" }));
    const { result } = renderHook(() => useBackgroundPreference());
    expect(result.current[0]).toEqual({ mode: "studio", style: "dark" });
  });

  it("ignores a malformed stored value", () => {
    window.localStorage.setItem(BACKGROUND_STORAGE_KEY, "{oops");
    expect(renderHook(() => useBackgroundPreference()).result.current[0]).toBeNull();
  });

  it("updates every mounted hook, and follows other tabs", () => {
    const a = renderHook(() => useBackgroundPreference());
    const b = renderHook(() => useBackgroundPreference());
    act(() => a.result.current[1]({ mode: "studio", style: "grey" }));
    expect(b.result.current[0]).toEqual({ mode: "studio", style: "grey" });

    act(() => {
      window.localStorage.setItem(BACKGROUND_STORAGE_KEY, JSON.stringify({ mode: "original", style: null }));
      window.dispatchEvent(new StorageEvent("storage", { key: BACKGROUND_STORAGE_KEY }));
    });
    expect(a.result.current[0]).toEqual({ mode: "original", style: null });
  });

  it("keeps working for the page when storage throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    const { result } = renderHook(() => useBackgroundPreference());
    expect(result.current[0]).toBeNull();

    expect(() => act(() => result.current[1]({ mode: "original", style: "dark" }))).not.toThrow();
    expect(result.current[0]).toEqual({ mode: "original", style: "dark" });
    expect(readBackgroundPreference()).toEqual({ mode: "original", style: "dark" });
  });

  it("a later successful write replaces the in-memory choice", () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementationOnce(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    writeBackgroundPreference({ mode: "original", style: null });
    expect(readBackgroundPreference()).toEqual({ mode: "original", style: null });
    setItem.mockRestore();
    writeBackgroundPreference({ mode: "studio", style: "light" });
    expect(readBackgroundPreference()).toEqual({ mode: "studio", style: "light" });
    expect(window.localStorage.getItem(BACKGROUND_STORAGE_KEY)).toContain("light");
  });
});
