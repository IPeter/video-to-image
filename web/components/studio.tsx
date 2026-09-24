"use client";

import { cn } from "cn";
import { Eye, Info, Layers, LoaderCircle } from "lucide-react";
import { useId, type ReactNode } from "react";

import { Badge } from "@/components/ui/badge";
import type { ResultImage, StudioStyle } from "@/lib/api";
import {
  STUDIO_BADGE_DESCRIPTION,
  STUDIO_BADGE_TEXT,
  STUDIO_STYLE_LABELS,
  studioNote,
  type BackgroundChoice,
  type BackgroundMode,
  type ShownVersion,
} from "@/lib/studio";

// Studio-background UI of the results page: the page-wide switch, the badge, and the
// per-image status line and "Show original" toggle (cards and lightbox).

/** Backdrop colours of the styles, for the swatches (the renderer's wall tones). */
const STYLE_SWATCHES: Readonly<Record<StudioStyle, string>> = {
  grey: "#8f9194",
  light: "#f3f3f4",
  dark: "#2c2d30",
};

interface SegmentOption<T extends string> {
  value: T;
  label: string;
  icon?: ReactNode;
}

/**
 * A segmented control built from native radio buttons in a fieldset: one tab stop, the
 * arrow keys move the selection, and screen readers announce "radio, 1 of 2".
 */
function Segmented<T extends string>({
  legend,
  label,
  options,
  value,
  onChange,
  testId,
}: {
  legend: string;
  /** Visible label before the options; the legend is read instead. */
  label?: string;
  options: readonly SegmentOption<T>[];
  value: T;
  onChange: (value: T) => void;
  testId: string;
}) {
  const name = useId();
  return (
    <fieldset className="flex min-w-0 items-center gap-2" data-testid={testId}>
      <legend className="sr-only">{legend}</legend>
      {label && (
        <span aria-hidden="true" className="text-sm text-muted-foreground">
          {label}
        </span>
      )}
      <div className="inline-flex rounded-lg bg-muted p-0.5 ring-1 ring-foreground/5">
        {options.map((o) => (
          <label key={o.value} className="relative">
            {/* The transparent input covers its segment, so a tap or click lands on the
                radio itself (and touch screen readers find it where it is drawn). */}
            <input
              type="radio"
              name={name}
              value={o.value}
              checked={value === o.value}
              onChange={() => onChange(o.value)}
              className="peer absolute inset-0 m-0 size-full cursor-pointer appearance-none rounded-md opacity-0"
            />
            <span
              className={cn(
                "flex h-7 items-center gap-1.5 rounded-md px-3 text-sm font-medium whitespace-nowrap text-muted-foreground transition-colors select-none",
                "hover:text-foreground peer-checked:bg-background peer-checked:text-foreground peer-checked:shadow-sm dark:peer-checked:bg-input/60",
                "peer-focus-visible:ring-3 peer-focus-visible:ring-ring/50",
              )}
            >
              {o.icon}
              {o.label}
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

export interface BackgroundSwitchProps {
  /** The job's styles, display order (default first). */
  styles: readonly StudioStyle[];
  choice: BackgroundChoice;
  onModeChange: (mode: BackgroundMode) => void;
  onStyleChange: (style: StudioStyle) => void;
}

/** "Background: Original | Studio", plus the style picker while Studio is on. */
export function BackgroundSwitch({ styles, choice, onModeChange, onStyleChange }: BackgroundSwitchProps) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2" data-testid="background-switch">
      <Segmented<BackgroundMode>
        legend="Background"
        label="Background"
        options={[
          { value: "original", label: "Original" },
          { value: "studio", label: "Studio" },
        ]}
        value={choice.mode}
        onChange={onModeChange}
        testId="background-mode"
      />
      {choice.mode === "studio" && styles.length > 1 && (
        <Segmented<StudioStyle>
          legend="Studio style"
          options={styles.map((s) => ({
            value: s,
            label: STUDIO_STYLE_LABELS[s],
            icon: (
              <span
                aria-hidden="true"
                className="size-3 shrink-0 rounded-full ring-1 ring-foreground/25"
                style={{ backgroundColor: STYLE_SWATCHES[s] }}
              />
            ),
          }))}
          value={choice.style}
          onChange={onStyleChange}
          testId="studio-style"
        />
      )}
    </div>
  );
}

/** Shown above the grid while the studio versions are being made and none is ready yet. */
export function StudioPreparing() {
  return (
    <p className="flex items-center gap-2 text-sm text-muted-foreground" data-testid="studio-preparing">
      <LoaderCircle aria-hidden="true" className="size-4 shrink-0 motion-safe:animate-spin" />
      Preparing studio backgrounds for the exterior photos. The photos below are ready to use now.
    </p>
  );
}

/** "Studio background · car unchanged", with the explanation as tooltip and for screen readers. */
export function StudioBadge({ className }: { className?: string }) {
  return (
    <Badge
      variant="outline"
      className={cn("h-auto min-h-5 rounded-md font-normal whitespace-normal text-muted-foreground", className)}
      title={STUDIO_BADGE_DESCRIPTION}
      data-testid="studio-badge"
    >
      <Layers aria-hidden="true" />
      <span aria-hidden="true">{STUDIO_BADGE_TEXT}</span>
      <span className="sr-only">
        {STUDIO_BADGE_TEXT}. {STUDIO_BADGE_DESCRIPTION}
      </span>
    </Badge>
  );
}

export interface StudioToggleProps {
  shown: ShownVersion;
  onToggle: () => void;
  className?: string;
}

/**
 * "Show original": a toggle button (aria-pressed) that switches one image between its
 * studio version and the original. Pressed = the original is on screen.
 */
export function StudioToggle({ shown, onToggle, className }: StudioToggleProps) {
  if (!shown.hasStudio) return null;
  const pressed = shown.variant === null;
  return (
    <button
      type="button"
      aria-pressed={pressed}
      onClick={onToggle}
      data-testid="studio-toggle"
      className={cn(
        "inline-flex h-7 shrink-0 items-center gap-1 rounded-md px-2 text-xs font-medium whitespace-nowrap outline-none transition-colors select-none",
        "bg-background/90 text-foreground shadow-sm ring-1 ring-foreground/15 backdrop-blur-sm hover:bg-background",
        "aria-pressed:bg-foreground aria-pressed:text-background aria-pressed:hover:bg-foreground/90",
        "focus-visible:ring-3 focus-visible:ring-ring/50",
        className,
      )}
    >
      <Eye aria-hidden="true" className="size-3.5" />
      Show original
    </button>
  );
}

/**
 * The studio line of an image (nothing for images without a studio version): the badge
 * or "Original photo" when it has one, "Preparing studio version…" while it is made, or
 * the muted "Studio version withheld: <reason>" note (up to three lines). At least two
 * text lines tall, so a card keeps its height when a pending version becomes ready.
 */
export function StudioStatusLine({ image, shown, className }: { image: ResultImage; shown: ShownVersion; className?: string }) {
  const studio = image.studio;
  if (!studio) return null;
  let content: ReactNode;
  if (shown.hasStudio) {
    content = shown.variant ? (
      <StudioBadge />
    ) : (
      <span className="text-xs text-muted-foreground" data-testid="studio-original">
        Original photo
      </span>
    );
  } else if (studio.status === "pending") {
    content = (
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground" data-testid="studio-pending">
        <LoaderCircle aria-hidden="true" className="size-3.5 shrink-0 motion-safe:animate-spin" />
        Preparing studio version…
      </span>
    );
  } else {
    const note = studioNote(studio);
    if (!note) return null;
    content = (
      <span className="flex items-start gap-1.5 text-xs text-muted-foreground" data-testid="studio-note">
        <Info aria-hidden="true" className="mt-px size-3.5 shrink-0" />
        <span className="line-clamp-3" title={note}>
          {note}
        </span>
      </span>
    );
  }
  return (
    <div className={cn("flex min-h-8 items-center", className)} data-testid="studio-line" data-studio-status={studio.status}>
      {content}
    </div>
  );
}
