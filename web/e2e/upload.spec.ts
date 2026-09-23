import { existsSync } from "node:fs";
import path from "node:path";

import { expect, test, type Page } from "@playwright/test";

import { API_URL, DOMAIN_KEY } from "./env";

// Full flow against `v2i serve --embed-worker --analyzer=fake` (see playwright.config.ts).
// Fixture videos come from scripts/gen-fixtures.sh (`make fixtures`).

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
  await page.getByRole("button", { name: "Upload and find the best shots" }).click();
  await page.waitForURL(/\/jobs\/job_[A-Za-z0-9]+$/, { timeout: 60_000 });
}

test("uploads a walkaround video and shows the selected images", async ({ page }) => {
  await uploadFixture(page, WALKAROUND);

  const grid = page.getByRole("list", { name: "Selected images" });
  await expect(grid).toBeVisible({ timeout: 90_000 });

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

test("a video that is not a car ends in the rejected state", async ({ page }) => {
  await uploadFixture(page, REJECT);

  const rejected = page.getByTestId("job-rejected");
  await expect(rejected).toBeVisible({ timeout: 90_000 });
  await expect(rejected.getByRole("heading", { level: 1 })).toHaveText(rejectedTitle);
  await expect(rejected.getByRole("link", { name: "Try another video" })).toHaveAttribute("href", "/");
});
