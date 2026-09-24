"use client";

import { useCallback, useMemo, useSyncExternalStore } from "react";

import {
  parseBackgroundPreference,
  serializeBackgroundPreference,
  type BackgroundPreference,
} from "@/lib/studio";

/** localStorage key of the photo background choice (one per browser, for every job). */
export const BACKGROUND_STORAGE_KEY = "v2i:photo-background";

const listeners = new Set<() => void>();

// The choice when localStorage cannot hold it (private mode, blocked site data, quota):
// it then lasts as long as the page. Takes precedence over whatever storage still has.
let unsaved: string | null = null;

function read(): string | null {
  if (unsaved !== null) return unsaved;
  try {
    return window.localStorage.getItem(BACKGROUND_STORAGE_KEY);
  } catch {
    return null;
  }
}

function notify() {
  for (const l of listeners) l();
}

function onStorage(e: StorageEvent) {
  // Another tab changed the choice (or cleared storage).
  if (e.key === BACKGROUND_STORAGE_KEY || e.key === null) notify();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (listeners.size === 1) window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener("storage", onStorage);
  };
}

/** The remembered choice, or null when there is none (or it is unreadable). */
export function readBackgroundPreference(): BackgroundPreference | null {
  return parseBackgroundPreference(read());
}

/** Remembers the choice; never throws. Every mounted useBackgroundPreference re-renders. */
export function writeBackgroundPreference(pref: BackgroundPreference): void {
  const raw = serializeBackgroundPreference(pref);
  try {
    window.localStorage.setItem(BACKGROUND_STORAGE_KEY, raw);
    unsaved = null;
  } catch {
    unsaved = raw;
  }
  notify();
}

/** Tests: forget a choice kept in memory after a failed write. */
export function resetUnsavedBackgroundPreference(): void {
  unsaved = null;
}

/**
 * The photo background choice remembered in this browser (localStorage, guarded: without
 * storage it lasts for the page). Null on the server and before anything was chosen.
 */
export function useBackgroundPreference(): [BackgroundPreference | null, (pref: BackgroundPreference) => void] {
  const raw = useSyncExternalStore(subscribe, read, () => null);
  const pref = useMemo(() => parseBackgroundPreference(raw), [raw]);
  const set = useCallback((p: BackgroundPreference) => writeBackgroundPreference(p), []);
  return [pref, set];
}
