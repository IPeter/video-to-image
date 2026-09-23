import { CircleAlert } from "lucide-react";
import type { ReactNode } from "react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { ErrorCode, PublicLimits } from "@/lib/api";
import { formatBytes, largestResolutionWithin } from "@/lib/format";

// Maps every API error code to plain-language copy. Raw server messages are never shown:
// they are safe English defaults for developers, not product copy.

export type MessageLimits = Partial<
  Pick<PublicLimits, "maxUploadBytes" | "minDurationSec" | "maxDurationSec" | "minShortSidePx" | "maxPixels">
>;

/**
 * Frame-rate limit quoted in RESOLUTION_TOO_HIGH copy. PublicLimits does not carry the
 * domain's maxFps, so this mirrors the server default (domain.Limits.EffectiveMaxFPS).
 */
export const MAX_FPS_HINT = 120;

function resolutionTooHigh(limits: MessageLimits): string {
  const r = largestResolutionWithin(limits.maxPixels);
  if (!r) {
    return "The video's resolution or frame rate is higher than we can process. Export it at a lower resolution or frame rate and try again.";
  }
  return `The video's resolution or frame rate is higher than we can process (max about ${r.width}×${r.height}, ${MAX_FPS_HINT} fps). Export it at ${r.name} or lower and try again.`;
}

function durationRange(limits: MessageLimits): string {
  const { minDurationSec: min, maxDurationSec: max } = limits;
  if (min && max) return ` Videos must be between ${min} and ${max} seconds long.`;
  if (max) return ` Videos can be at most ${max} seconds long.`;
  if (min) return ` Videos must be at least ${min} seconds long.`;
  return "";
}

/**
 * Plain-language message for an API error code. The switch is exhaustive over the
 * generated ErrorCode union: adding a code in Go breaks the TypeScript build here until
 * it gets copy.
 */
export function messageForCode(code: ErrorCode, limits: MessageLimits = {}): string {
  switch (code) {
    case "DOMAIN_NOT_FOUND":
      return "This site is not set up correctly right now. Please try again later.";
    case "ORIGIN_NOT_ALLOWED":
      return "This site is not allowed to use the processing service. Please contact support.";
    case "UNAUTHORIZED":
      return "You are not allowed to do that.";
    case "INVALID_REQUEST":
      return "Something was wrong with the request. Please choose the video again and retry.";
    case "UPLOAD_TOO_LARGE":
      return limits.maxUploadBytes
        ? `The file is larger than ${formatBytes(limits.maxUploadBytes)}. Try a shorter clip or export it at a lower resolution.`
        : "The file is too large. Try a shorter clip or export it at a lower resolution.";
    case "UNSUPPORTED_FORMAT":
      return "This file type is not supported. Please upload an MP4 or MOV video.";
    case "CANNOT_DECODE":
      return "We could not read this video. The file may be damaged or use an unusual format. Try exporting it again as MP4.";
    case "TOO_LONG":
      return `The video is too long.${durationRange(limits)}`;
    case "TOO_SHORT":
      return `The video is too short.${durationRange(limits)}`;
    case "RESOLUTION_TOO_LOW":
      return limits.minShortSidePx
        ? `The video resolution is too low. It needs to be at least ${limits.minShortSidePx} pixels on its shorter side.`
        : "The video resolution is too low. Please film at a higher resolution.";
    case "RESOLUTION_TOO_HIGH":
      return resolutionTooHigh(limits);
    case "SERVER_BUSY":
      return "We're busy processing other uploads. Please try again in a minute.";
    case "RATE_LIMITED":
      return "You have sent a lot of requests in a short time. Please wait a while and try again.";
    case "JOB_NOT_FOUND":
      return "We could not find this job. Check the link, or upload the video again.";
    case "IMAGE_NOT_FOUND":
      return "This image is no longer available.";
    case "NOT_READY":
      return "The images are not ready yet. Please wait a moment.";
    case "EXPIRED":
      return "These images have expired and were deleted. Upload the video again to get new ones.";
    case "MODEL_UNAVAILABLE":
      return "Our video analysis service is busy or unavailable right now. Please try again in a few minutes.";
    case "MODEL_BAD_OUTPUT":
      return "We could not make sense of the analysis for this video. Please try again, or try a different video.";
    case "PROCESSING_ERROR":
      return "Something went wrong while processing the video. Please try again.";
    case "NOT_FOUND":
      return "We could not find what you were looking for.";
    case "INTERNAL_ERROR":
      return "Something went wrong on our side. Please try again.";
    default: {
      const unhandled: never = code;
      return unhandled;
    }
  }
}

/** Problems detected in the browser, with no API error code. */
export type ClientProblem = "NETWORK_ERROR" | "UPLOAD_TIMEOUT" | "UNKNOWN_ERROR";

export function messageForClientProblem(problem: ClientProblem): string {
  switch (problem) {
    case "NETWORK_ERROR":
      return "We could not reach the server. Check your internet connection and try again.";
    case "UPLOAD_TIMEOUT":
      return "The upload took too long. Check your connection and try again.";
    case "UNKNOWN_ERROR":
      return "Something went wrong. Please try again.";
    default: {
      const unhandled: never = problem;
      return unhandled;
    }
  }
}

export interface ErrorMessageProps {
  /** API error code; null/undefined falls back to `problem` or a generic message. */
  code?: ErrorCode | null;
  problem?: ClientProblem;
  limits?: MessageLimits;
  title?: string;
  className?: string;
  /** Extra content under the message, e.g. a retry button. */
  children?: ReactNode;
}

/** A destructive alert with the mapped message for an error code. */
export function ErrorMessage({ code, problem, limits, title = "Something went wrong", className, children }: ErrorMessageProps) {
  const message = code
    ? messageForCode(code, limits)
    : messageForClientProblem(problem ?? "UNKNOWN_ERROR");
  return (
    <Alert variant="destructive" className={className} data-error-code={code ?? problem ?? "UNKNOWN_ERROR"}>
      <CircleAlert aria-hidden="true" />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>
        <p>{message}</p>
        {children}
      </AlertDescription>
    </Alert>
  );
}
