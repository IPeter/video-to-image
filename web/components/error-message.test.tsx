import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ERROR_CODES } from "@/lib/api";
import { carsDomain } from "@/test/fixtures";

import { ErrorMessage, messageForClientProblem, messageForCode } from "./error-message";

describe("messageForCode", () => {
  it.each(ERROR_CODES)("%s has plain-language copy", (code) => {
    const msg = messageForCode(code, carsDomain.limits);
    expect(msg.trim().length).toBeGreaterThan(10);
    expect(msg).not.toContain(code); // never leaks the machine code
    expect(msg).toMatch(/[.!]$/); // full sentences
    // Also without limits.
    expect(messageForCode(code).trim().length).toBeGreaterThan(10);
  });

  it("uses the domain limits where relevant", () => {
    const l = carsDomain.limits;
    expect(messageForCode("UPLOAD_TOO_LARGE", l)).toContain("The file is larger than 500 MB.");
    expect(messageForCode("TOO_LONG", l)).toContain("Videos must be between 5 and 180 seconds long.");
    expect(messageForCode("TOO_SHORT", l)).toContain("Videos must be between 5 and 180 seconds long.");
    expect(messageForCode("RESOLUTION_TOO_LOW", l)).toContain("480 pixels");
    expect(messageForCode("RESOLUTION_TOO_HIGH", l)).toBe(
      "The video's resolution or frame rate is higher than we can process (max about 3840×2160, 120 fps). Export it at 4K or lower and try again.",
    );
  });

  it("asks to retry shortly when the server is busy", () => {
    expect(messageForCode("SERVER_BUSY")).toBe("We're busy processing other uploads. Please try again in a minute.");
  });

  it("tells the user to upload again when a chunked upload went wrong", () => {
    expect(messageForCode("UPLOAD_NOT_FOUND")).toBe("The upload expired or was interrupted. Please upload the video again.");
    expect(messageForCode("UPLOAD_INCOMPLETE")).toBe("Part of the video did not arrive. Please try the upload again.");
  });

  it("degrades gracefully without limits", () => {
    expect(messageForCode("UPLOAD_TOO_LARGE")).toBe(
      "The file is too large. Try a shorter clip or export it at a lower resolution.",
    );
    expect(messageForCode("TOO_LONG")).toBe("The video is too long.");
    expect(messageForCode("TOO_LONG", { maxDurationSec: 180 })).toContain("at most 180 seconds");
    expect(messageForCode("RESOLUTION_TOO_HIGH")).toBe(
      "The video's resolution or frame rate is higher than we can process. Export it at a lower resolution or frame rate and try again.",
    );
  });

  it("covers client-side problems", () => {
    for (const p of ["NETWORK_ERROR", "UPLOAD_TIMEOUT", "UNKNOWN_ERROR"] as const) {
      expect(messageForClientProblem(p).length).toBeGreaterThan(10);
    }
  });
});

describe("<ErrorMessage>", () => {
  it("renders the mapped message as an alert", () => {
    render(<ErrorMessage code="TOO_LONG" limits={carsDomain.limits} title="Upload failed" />);
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Upload failed");
    expect(alert).toHaveTextContent("Videos must be between 5 and 180 seconds long.");
    expect(alert).toHaveAttribute("data-error-code", "TOO_LONG");
  });

  it("falls back to the client problem, then a generic message", () => {
    const { rerender } = render(<ErrorMessage code={null} problem="NETWORK_ERROR" />);
    expect(screen.getByRole("alert")).toHaveTextContent("could not reach the server");
    rerender(<ErrorMessage />);
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong. Please try again.");
  });
});
