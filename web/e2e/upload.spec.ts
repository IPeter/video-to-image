import { existsSync } from "node:fs";
import path from "node:path";

import { expect, test, type Page } from "@playwright/test";

import { API_URL, DOMAIN_KEY } from "./env";

// Full flow against `v2i serve --embed-worker --analyzer=fake` (see playwright.config.ts).
// Fixture videos come from scripts/gen-fixtures.sh (`make fixtures`). The studio-background
// spec needs the backend started with V2I_STUDIO_URL=fake (the deterministic segmenter);
// without it the job's studio.status is "none" and that spec skips itself.

const repoRoot = path.resolve(__dirname, "..", "..");
const WALKAROUND = process.env.E2E_VIDEO ?? path.join(repoRoot, "testdata", "videos", "car-walkaround-like.mp4");
const REJECT = process.env.E2E_REJECT_VIDEO ?? path.join(repoRoot, "testdata", "videos", "reject-not-a-car.mp4");

/**
 * In CI a missing backend or fixture is a failure, never a skip: a run where every spec
 * skipped itself exits 0 and would hide a `v2i serve` that no longer starts.
 */
const FAIL_INSTEAD_OF_SKIP = !!process.env.CI;
const START_BACKEND_HINT = "Start the backend first: go run ./cmd/v2i serve --embed-worker --analyzer=fake";

let apiProblem: string | null = null;
let rejectedTitle = "";

test.beforeAll(async () => {
  try {
    const health = await fetch(`${API_URL}/healthz`, { signal: AbortSignal.timeout(5000) });
    if (!health.ok) {
      apiProblem = `API at ${API_URL}/healthz answered HTTP ${health.status}`;
      return;
    }
    const domain = await fetch(`${API_URL}/v1/domains/${DOMAIN_KEY}`, { signal: AbortSignal.timeout(5000) });
    if (!domain.ok) {
      apiProblem = `GET ${API_URL}/v1/domains/${DOMAIN_KEY} answered HTTP ${domain.status}`;
      return;
    }
    rejectedTitle = ((await domain.json()) as { copy: { rejectedTitle: string } }).copy.rejectedTitle;
  } catch (err) {
    apiProblem = `API not reachable at ${API_URL} (${err instanceof Error ? err.message : String(err)})`;
  }
  if (apiProblem && FAIL_INSTEAD_OF_SKIP) throw new Error(`[e2e] ${apiProblem}. ${START_BACKEND_HINT}`);
});

test.afterAll(() => {
  if (apiProblem && !FAIL_INSTEAD_OF_SKIP) console.warn(`[e2e] skipped: ${apiProblem}`);
});

test.beforeEach(() => {
  test.skip(apiProblem !== null, `${apiProblem}. ${START_BACKEND_HINT}`);
});

async function uploadFixture(page: Page, file: string) {
  if (!existsSync(file)) {
    const problem = `Fixture ${file} is missing: run \`make fixtures\` (scripts/gen-fixtures.sh) first`;
    if (FAIL_INSTEAD_OF_SKIP) throw new Error(`[e2e] ${problem}`);
    test.skip(true, problem);
  }
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await page.getByLabel("Choose a video file").setInputFiles(file);
  await expect(page.getByRole("region", { name: "Selected video" })).toContainText(path.basename(file));
  // Every upload goes through the chunked API (small files as one chunk).
  const firstChunk = page.waitForResponse(
    (res) => res.request().method() === "PUT" && /\/uploads\/upl_[A-Za-z0-9]+\/chunks\/0$/.test(res.url()),
  );
  await page.getByRole("button", { name: "Upload and find the best shots" }).click();
  expect((await firstChunk).status()).toBe(204);
  await page.waitForURL(/\/jobs\/job_[A-Za-z0-9]+$/, { timeout: 60_000 });
}

test("uploads a walkaround video and shows the selected images", async ({ page }) => {
  await uploadFixture(page, WALKAROUND);

  const grid = page.getByRole("list", { name: "Selected images" });
  await expect(grid).toBeVisible({ timeout: 90_000 });

  // The fake analyzer's calls are recorded as usage at zero cost.
  await expect(page.getByTestId("ai-usage")).toContainText(/AI usage: [\d,]+ tokens · \$0\.00 \(free test mode\)/);

  const cards = grid.getByRole("listitem");
  expect(await cards.count()).toBeGreaterThanOrEqual(1);
  await expect(cards.first().getByTestId("shot-label")).not.toBeEmpty();
  await expect(cards.first().getByTestId("timestamp")).toHaveText(/^\d{2}:\d{2}/);

  const zip = page.getByRole("link", { name: /Download all \(ZIP\)/ });
  await expect(zip).toBeVisible();
  await expect(zip).toHaveAttribute("href", new RegExp(`^${API_URL.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/.+\\.zip`));

  const thumb = cards.first().getByRole("img");
  await thumb.scrollIntoViewIfNeeded();
  await expect
    .poll(() => thumb.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth), { timeout: 20_000 })
    .toBeGreaterThan(0);

  // The lightbox shows the full-resolution image.
  await cards.first().getByRole("button", { name: /in full size/ }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect
    .poll(() => dialog.getByRole("img").evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth), {
      timeout: 20_000,
    })
    .toBeGreaterThan(0);
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();

  // "Delete now" (a CORS-preflighted DELETE) ends in a confirmation, not in "expired".
  await page.getByRole("button", { name: "Delete now" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete now" }).click();
  const deleted = page.getByTestId("job-deleted");
  await expect(deleted).toBeVisible();
  const heading = deleted.getByRole("heading", { level: 1 });
  await expect(heading).toHaveText("Your images and video were deleted");
  await expect(heading).toBeFocused();
  await expect(page.getByRole("link", { name: /Download all \(ZIP\)/ })).toHaveCount(0);
});

type StudioStatus = "none" | "pending" | "running" | "done" | "failed" | "skipped";
interface JobStudioBody {
  status: string;
  studio?: { status: StudioStatus; zipUrl: string | null };
  result?: { images: { studio: { status: string } | null }[] } | null;
}

async function getJobBody(jobId: string): Promise<JobStudioBody> {
  const res = await fetch(`${API_URL}/v1/domains/${DOMAIN_KEY}/jobs/${jobId}`, { signal: AbortSignal.timeout(5000) });
  expect(res.ok, `GET job answered HTTP ${res.status}`).toBe(true);
  return (await res.json()) as JobStudioBody;
}

/**
 * The job's studio outcome once the studio run is over. "none" is final for a completed
 * job (the pipeline sets studio.status to pending in the same transaction that completes
 * it): the backend makes no studio versions (V2I_STUDIO_URL unset). Polls every 2 s: the
 * page polls too, and both count against the per-client status rate limit.
 */
async function settledStudio(jobId: string): Promise<JobStudioBody> {
  const deadline = Date.now() + 120_000;
  for (;;) {
    const job = await getJobBody(jobId);
    const status = job.studio?.status ?? "none";
    if (status !== "pending" && status !== "running") return job;
    if (Date.now() > deadline) throw new Error(`[e2e] studio still ${status} after 120 s`);
    await new Promise((r) => setTimeout(r, 2000));
  }
}

test("an exterior photo gets a studio background and the toggle switches the image", async ({ page }) => {
  await uploadFixture(page, WALKAROUND);
  const grid = page.getByRole("list", { name: "Selected images" });
  await expect(grid).toBeVisible({ timeout: 90_000 });

  const jobId = new URL(page.url()).pathname.split("/").pop() ?? "";
  const job = await settledStudio(jobId);
  test.skip((job.studio?.status ?? "none") === "none", "Studio backgrounds are off: start the backend with V2I_STUDIO_URL=fake");
  expect(job.studio?.status).toBe("done");
  const ready = (job.result?.images ?? []).filter((img) => img.studio?.status === "ready");
  expect(ready.length, "at least one exterior image has a studio version").toBeGreaterThan(0);

  // The page picks the studio versions up by itself (it keeps polling) and shows them by default.
  const mode = page.getByRole("group", { name: "Background" });
  await expect(mode).toBeVisible({ timeout: 30_000 });
  await expect(mode.getByRole("radio", { name: "Studio" })).toBeChecked();
  await expect(page.getByRole("link", { name: "Download studio photos (ZIP)" })).toHaveAttribute("href", /\/studio\.zip$/);

  // Pin the card by its image id: after "Show original" it no longer carries the badge.
  const firstStudio = grid.getByRole("article").filter({ has: page.getByTestId("studio-badge") }).first();
  await expect(firstStudio).toBeVisible();
  const imageId = await firstStudio.getAttribute("data-image-id");
  expect(imageId).toMatch(/^img_[A-Za-z0-9]+$/);
  const card = grid.locator(`article[data-image-id="${imageId}"]`);
  await expect(card).toHaveAttribute("data-version", "studio");
  await expect(card.getByTestId("studio-badge")).toContainText("Studio background · car unchanged");
  const thumb = card.getByRole("img");
  await expect(thumb).toHaveAttribute("src", /\/studio\/(grey|light|dark)\/thumb$/);
  await thumb.scrollIntoViewIfNeeded();
  await expect
    .poll(() => thumb.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth), { timeout: 20_000 })
    .toBeGreaterThan(0);

  // "Show original" flips this card only, and back.
  const toggle = card.getByRole("button", { name: "Show original" });
  await expect(toggle).toHaveAttribute("aria-pressed", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-pressed", "true");
  await expect(card).toHaveAttribute("data-version", "original");
  await expect(thumb).not.toHaveAttribute("src", /\/studio\//);
  await expect(thumb).toHaveAttribute("src", /\/images\/img_[A-Za-z0-9]+\/thumb$/);
  await toggle.click();
  await expect(card).toHaveAttribute("data-version", "studio");

  // The page-wide switch: every card shows its original, then the studio versions again.
  await mode.getByRole("radio", { name: "Original" }).check();
  await expect(grid.getByTestId("studio-badge")).toHaveCount(0);
  await mode.getByRole("radio", { name: "Studio" }).check();
  await expect(card).toHaveAttribute("data-version", "studio");

  // The lightbox shows (and downloads) the studio version too.
  await card.getByRole("button", { name: /in full size/ }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("img")).toHaveAttribute("src", /\/studio\/(grey|light|dark)$/);
  await expect(dialog.getByRole("link", { name: /Download studio photo/ })).toHaveAttribute("download", /_studio-(grey|light|dark)\.jpg$/);
  await expect
    .poll(() => dialog.getByRole("img").evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth), {
      timeout: 20_000,
    })
    .toBeGreaterThan(0);
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

test("a video that is not a car ends in the rejected state", async ({ page }) => {
  await uploadFixture(page, REJECT);

  const rejected = page.getByTestId("job-rejected");
  await expect(rejected).toBeVisible({ timeout: 90_000 });
  await expect(rejected.getByRole("heading", { level: 1 })).toHaveText(rejectedTitle);
  await expect(rejected.getByRole("link", { name: "Try another video" })).toHaveAttribute("href", "/");
  // The gate call that rejected it is accounted for too.
  await expect(rejected.getByTestId("ai-usage")).toContainText(/AI usage: [\d,]+ tokens/);
});
