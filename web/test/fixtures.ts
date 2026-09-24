import type {
  ImageStudio,
  Job,
  JobResult,
  JobStudio,
  JobUsage,
  PublicDomain,
  ResultImage,
  StudioStyle,
  UsageStep,
} from "@/lib/api";

// Test data shaped exactly like the API contract (lib/api.d.ts).

export const API_BASE = "http://api.test:4000";
export const JOB_ID = "job_01J8ZK3M2Q7K2M9QXA4B5C6D7E";

export const carsDomain: PublicDomain = {
  key: "cars",
  name: "Car Listing Photos",
  limits: {
    maxUploadBytes: 524_288_000,
    maxDurationSec: 180,
    minDurationSec: 5,
    minShortSidePx: 480,
    maxPixels: 8_912_896, // 4096×2176
  },
  shots: [
    { id: "front_three_quarter", label: "Front 3/4", required: true },
    { id: "side_profile", label: "Side profile", required: true },
    { id: "rear_three_quarter", label: "Rear 3/4", required: true },
    { id: "interior_dashboard", label: "Dashboard", required: true },
  ],
  imageCount: 8,
  retentionHours: 24,
  copy: {
    uploadTitle: "Upload a walkaround video of the car",
    uploadHint: "30–120 seconds. Walk slowly around the outside, then film the interior. MP4 or MOV, up to 500 MB.",
    rejectedTitle: "This doesn't look like a car video",
  },
};

export function makeImage(index: number, overrides: Partial<ResultImage> = {}): ResultImage {
  const id = `img_${String(index).padStart(26, "0")}`;
  const base = `/v1/domains/cars/jobs/${JOB_ID}/images/${id}`;
  return {
    id,
    index,
    shotId: "front_three_quarter",
    shotLabel: "Front 3/4",
    timestampSec: 6.25,
    timestampLabel: "00:06",
    width: 1920,
    height: 1080,
    reason: "Whole car visible at 45°, steady camera, even light.",
    fileName: `cars_QXA4B5C6_${index}_front_three_quarter_00-06.jpg`,
    url: base,
    downloadUrl: `${base}?download=1`,
    thumbnailUrl: `${base}/thumb`,
    rotation: 0,
    cropped: false,
    studio: null,
    ...overrides,
  };
}

/** A ready studio outcome for `image` with one variant per style (default first). */
export function readyStudio(image: Pick<ResultImage, "id" | "fileName">, styles: StudioStyle[] = ["grey", "light", "dark"]): ImageStudio {
  const base = `/v1/domains/cars/jobs/${JOB_ID}/images/${image.id}/studio`;
  return {
    status: "ready",
    reason: "",
    variants: styles.map((style) => ({
      style,
      url: `${base}/${style}`,
      downloadUrl: `${base}/${style}?download=1`,
      thumbnailUrl: `${base}/${style}/thumb`,
      fileName: image.fileName.replace(/\.jpg$/, `_studio-${style}.jpg`),
      width: 1920,
      height: 1080,
    })),
  };
}

/** An exterior image with a ready studio version. */
export function makeStudioImage(index: number, overrides: Partial<ResultImage> = {}, styles?: StudioStyle[]): ResultImage {
  const image = makeImage(index, overrides);
  return { ...image, studio: overrides.studio ?? readyStudio(image, styles) };
}

/** A job without studio backgrounds (feature off, or nothing eligible). */
export const NO_STUDIO: JobStudio = { status: "none", styles: [], defaultStyle: "", zipUrl: null };

export function makeJobStudio(overrides: Partial<JobStudio> = {}): JobStudio {
  return {
    status: "done",
    styles: ["grey", "light", "dark"],
    defaultStyle: "grey",
    zipUrl: `/v1/domains/cars/jobs/${JOB_ID}/studio.zip`,
    ...overrides,
  };
}

export function makeResult(overrides: Partial<JobResult> = {}): JobResult {
  return {
    summary: "Walkaround of a silver sedan, exterior then interior.",
    images: [
      makeImage(1),
      makeImage(2, { shotId: "side_profile", shotLabel: "Side profile", timestampSec: 14, timestampLabel: "00:14", reason: "Full side, level camera." }),
      makeImage(3, { shotId: "interior_dashboard", shotLabel: "Dashboard", timestampSec: 61.5, timestampLabel: "01:01", reason: "Dashboard and wheel in frame." }),
    ],
    missingShots: [{ id: "rear_three_quarter", label: "Rear 3/4" }],
    explanation: "3 images selected. The rear 3/4 angle was not clearly visible in the video.",
    zipUrl: `/v1/domains/cars/jobs/${JOB_ID}/download.zip`,
    ...overrides,
  };
}

/** A job before its first model call. */
export const NO_USAGE: JobUsage = {
  calls: 0,
  inputTokens: 0,
  outputTokens: 0,
  thinkingTokens: 0,
  totalTokens: 0,
  costUsd: 0,
  testMode: false,
  costComplete: true,
  steps: [],
};

export function makeUsageStep(overrides: Partial<UsageStep> = {}): UsageStep {
  return {
    step: "analyze",
    model: "gemini-3.5-flash-lite",
    calls: 1,
    inputTokens: 6200,
    outputTokens: 900,
    thinkingTokens: 400,
    costUsd: 0.0031,
    priced: true,
    ...overrides,
  };
}

/** A finished job's usage: one analyze and one review call, 9,012 tokens, $0.0041. */
export function makeUsage(overrides: Partial<JobUsage> = {}): JobUsage {
  return {
    calls: 2,
    inputTokens: 7400,
    outputTokens: 1112,
    thinkingTokens: 500,
    totalTokens: 9012,
    costUsd: 0.0041,
    testMode: false,
    costComplete: true,
    steps: [
      makeUsageStep(),
      makeUsageStep({ step: "review", inputTokens: 1200, outputTokens: 212, thinkingTokens: 100, costUsd: 0.001 }),
    ],
    ...overrides,
  };
}

/** Far enough ahead that a fixture job never expires by the clock during a test run. */
export const FAR_FUTURE = "2099-09-24T10:00:00Z";

export function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    jobId: JOB_ID,
    domainKey: "cars",
    status: "processing",
    createdAt: "2026-09-23T10:00:00Z",
    expiresAt: FAR_FUTURE,
    video: { durationSec: 74.2, width: 1920, height: 1080, fps: 29.97 },
    progress: {
      step: "analyze",
      percent: 35,
      steps: [
        { name: "preview", status: "done", ms: 2100 },
        { name: "analyze", status: "running" },
      ],
    },
    usage: NO_USAGE,
    studio: NO_STUDIO,
    result: null,
    rejection: null,
    error: null,
    ...overrides,
  };
}
