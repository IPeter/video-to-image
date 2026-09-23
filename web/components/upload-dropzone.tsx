"use client";

import { cn } from "cn";
import { Upload } from "lucide-react";
import { useId, useRef, useState, type ChangeEvent, type DragEvent } from "react";

export interface UploadDropzoneProps {
  onFile: (file: File) => void;
  disabled?: boolean;
  /** Extra text under the call to action (e.g. accepted formats and size). */
  hint?: string;
  className?: string;
}

/**
 * Drag-and-drop area plus a native file input. The whole area is the input's <label>, so
 * a click, Enter/Space on the focused input, or a drop all pick a file, and screen readers
 * announce a regular file chooser.
 */
export function UploadDropzone({ onFile, disabled = false, hint, className }: UploadDropzoneProps) {
  const inputId = useId();
  const hintId = useId();
  const [dragging, setDragging] = useState(false);
  // dragenter/dragleave fire for every child element: count them to know when we left.
  const dragDepth = useRef(0);

  const pick = (files: FileList | null | undefined) => {
    const file = files?.[0];
    if (file && !disabled) onFile(file);
  };

  const onChange = (e: ChangeEvent<HTMLInputElement>) => {
    pick(e.target.files);
    // Allow picking the same file again after a reset.
    e.target.value = "";
  };

  const hasFiles = (e: DragEvent) => Array.from(e.dataTransfer?.types ?? []).includes("Files");

  const onDragEnter = (e: DragEvent<HTMLLabelElement>) => {
    if (disabled || !hasFiles(e)) return;
    e.preventDefault();
    dragDepth.current += 1;
    setDragging(true);
  };
  const onDragOver = (e: DragEvent<HTMLLabelElement>) => {
    if (disabled || !hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  };
  const onDragLeave = () => {
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (dragDepth.current === 0) setDragging(false);
  };
  const onDrop = (e: DragEvent<HTMLLabelElement>) => {
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    pick(e.dataTransfer?.files);
  };

  return (
    <label
      htmlFor={inputId}
      data-dragging={dragging || undefined}
      data-disabled={disabled || undefined}
      onDragEnter={onDragEnter}
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
      className={cn(
        "flex min-h-52 cursor-pointer flex-col items-center justify-center gap-3 rounded-xl border-2 border-dashed border-input bg-card px-6 py-10 text-center transition-colors",
        "hover:border-primary/60 hover:bg-accent/50",
        "has-[input:focus-visible]:border-ring has-[input:focus-visible]:ring-3 has-[input:focus-visible]:ring-ring/50",
        "data-dragging:border-primary data-dragging:bg-accent",
        "data-disabled:cursor-not-allowed data-disabled:opacity-60",
        className,
      )}
    >
      <span className="flex size-12 items-center justify-center rounded-full bg-accent text-accent-foreground">
        <Upload aria-hidden="true" className="size-6" />
      </span>
      <span className="text-base font-medium">
        {dragging ? (
          "Drop the video to select it"
        ) : (
          <>
            Drag a video here, or <span className="text-primary underline underline-offset-4">choose a file</span>
          </>
        )}
      </span>
      {hint && (
        <span id={hintId} className="max-w-md text-sm text-muted-foreground">
          {hint}
        </span>
      )}
      <input
        id={inputId}
        type="file"
        accept="video/*"
        className="sr-only"
        disabled={disabled}
        aria-label="Choose a video file"
        aria-describedby={hint ? hintId : undefined}
        onChange={onChange}
      />
    </label>
  );
}
