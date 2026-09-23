import type { VideoMetadata } from "./upload-checks";

// Reads duration and resolution of a picked video with a detached <video> element and
// grabs a poster frame (~1 s in) onto a canvas, so the user sees they picked the right
// file. Browsers cannot decode every format the server accepts (e.g. HEVC in some
// browsers): then the probe resolves with metadata null and the upload is still allowed.

export interface VideoProbe {
  metadata: VideoMetadata | null;
  /** JPEG data URL of a frame near 1 s, or null if the browser could not draw one. */
  posterDataUrl: string | null;
}

export interface ProbeOptions {
  /** Give up after this many milliseconds (default 10 s). */
  timeoutMs?: number;
  /** Poster width in pixels (default 480); height follows the aspect ratio. */
  posterWidth?: number;
  signal?: AbortSignal;
}

const POSTER_TIME_SEC = 1;

export function probeVideoFile(file: Blob, opts: ProbeOptions = {}): Promise<VideoProbe> {
  const { timeoutMs = 10_000, posterWidth = 480, signal } = opts;

  return new Promise<VideoProbe>((resolve) => {
    if (typeof document === "undefined" || typeof URL.createObjectURL !== "function") {
      resolve({ metadata: null, posterDataUrl: null });
      return;
    }

    const url = URL.createObjectURL(file);
    const video = document.createElement("video");
    let metadata: VideoMetadata | null = null;
    let done = false;

    const finish = (posterDataUrl: string | null) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      video.removeAttribute("src");
      video.load(); // releases the decoder
      URL.revokeObjectURL(url);
      resolve({ metadata, posterDataUrl });
    };
    const onAbort = () => finish(null);
    const timer = setTimeout(() => finish(null), timeoutMs);
    signal?.addEventListener("abort", onAbort, { once: true });

    video.muted = true;
    video.playsInline = true;
    video.preload = "auto";

    video.addEventListener("loadedmetadata", () => {
      const durationSec = Number.isFinite(video.duration) ? video.duration : 0;
      metadata = { durationSec, width: video.videoWidth, height: video.videoHeight };
      if (!video.videoWidth || !video.videoHeight) {
        finish(null); // audio-only or undecodable video track
        return;
      }
      const target = durationSec > 0 ? Math.min(POSTER_TIME_SEC, durationSec / 2) : 0;
      if (target > 0) {
        video.currentTime = target;
      } else if (video.readyState >= 2) {
        drawPoster();
      } else {
        video.addEventListener("loadeddata", drawPoster, { once: true });
      }
    });
    video.addEventListener("seeked", drawPoster, { once: true });
    video.addEventListener("error", () => finish(null));

    function drawPoster() {
      if (done) return;
      try {
        const scale = Math.min(1, posterWidth / video.videoWidth);
        const canvas = document.createElement("canvas");
        canvas.width = Math.max(1, Math.round(video.videoWidth * scale));
        canvas.height = Math.max(1, Math.round(video.videoHeight * scale));
        const ctx = canvas.getContext("2d");
        if (!ctx) {
          finish(null);
          return;
        }
        ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
        finish(canvas.toDataURL("image/jpeg", 0.8));
      } catch {
        finish(null);
      }
    }

    video.src = url;
  });
}
