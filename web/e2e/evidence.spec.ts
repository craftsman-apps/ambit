import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { expect, test, type Page } from "@playwright/test";

const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";

async function signInAsAdmin(page: Page) {
  test.skip(!password, "AMBIT_ADMIN_PASSWORD not set");
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
}

const today = () => new Date().toISOString().slice(0, 10);

// Download one export: the file name, a ZIP, and the SHA-256 the page shows is the file's.
async function download(page: Page, name: "access" | "offboarding" | "audit", file: RegExp) {
  const card = page.getByTestId(`export-${name}`);
  const [dl] = await Promise.all([page.waitForEvent("download"), card.getByRole("button", { name: "Download" }).click()]);
  expect(dl.suggestedFilename()).toMatch(file);
  const bytes = await readFile((await dl.path())!);
  expect(bytes.subarray(0, 2).toString()).toBe("PK");
  await expect(page.getByTestId(`done-${name}`)).toContainText(createHash("sha256").update(bytes).digest("hex"));
}

test("evidence: warnings, download of each export with its SHA-256, export history", async ({ page }) => {
  await signInAsAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Evidence" }).click();
  await expect(page).toHaveURL(/\/settings\/evidence$/);
  await expect(page.getByTestId("personal-data")).toContainText("This file contains personal data");

  // The default date (the end of the last full quarter) is before this install's first access snapshot.
  const access = page.getByTestId("export-access");
  await page.getByLabel("Access review date").fill("2000-01-01");
  await expect(access.getByTestId("no-snapshot")).toContainText("No snapshot for this date");
  await expect(access.getByRole("button", { name: "Download" })).toBeDisabled();

  // A period that starts before the oldest audit record is marked.
  await page.getByLabel("Audit log from").fill("2000-01-01");
  await expect(page.getByTestId("retention")).toContainText("Audit records are kept for");
  await expect(page.getByTestId("retention")).toContainText("The audit log period starts before it");
  await expect(page.getByTestId("retention")).toHaveClass(/text-bad/);

  await page.getByLabel("Access review date").fill(today());
  await page.getByLabel("Audit log from").fill(today());
  await page.getByLabel("Audit log to").fill(today());
  await page.getByLabel("Offboarding from").fill(today());
  await page.getByLabel("Offboarding to").fill(today());
  await download(page, "access", new RegExp(`^ambit-access-${today()}\\.zip$`));
  await download(page, "offboarding", new RegExp(`^ambit-offboarding-${today()}-${today()}\\.zip$`));
  await download(page, "audit", new RegExp(`^ambit-audit-${today()}-${today()}\\.zip$`));

  const history = page.getByTestId("history");
  for (const name of ["access", "offboarding", "audit"]) {
    await expect(history.getByRole("row").filter({ hasText: name }).filter({ hasText: "complete" }).first()).toBeVisible();
  }
});
