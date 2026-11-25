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

// A date with several access snapshots: the page offers them; the choice is sent as `at`; the default sends none.
// The two endpoints are answered here, so the test needs no second rules change in the cluster.
test("evidence: the snapshots of a date are offered, and the chosen time is sent as at", async ({ page }) => {
  await signInAsAdmin(page);
  const day = today();
  await page.route("**/api/evidence/access/snapshots?*", (route) => route.fulfill({ json: { date: day, snapshots: [
    { time: `${day}T08:00:00Z`, rulesVersion: "aaaaaaaaaaaa", rulesHash: "h1", accessMode: "audit" },
    { time: `${day}T15:30:00Z`, rulesVersion: "bbbbbbbbbbbb", rulesHash: "h2", accessMode: "enforce" },
  ] } }));
  const asked: string[] = [];
  await page.route("**/api/evidence/access.zip?*", (route) => {
    asked.push(new URL(route.request().url()).search);
    return route.fulfill({ status: 200, contentType: "application/zip", headers: { "Content-Disposition": 'attachment; filename="ambit-access-test.zip"' }, body: "PK" });
  });
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Evidence" }).click();
  await page.getByLabel("Access review date").fill(day);
  const card = page.getByTestId("export-access");
  await expect(card.getByTestId("access-snapshots")).toBeVisible();
  await expect(card.getByRole("option")).toHaveText(["End of the day (the last snapshot)", "08:00:00 UTC · rules aaaaaaaaaaaa (audit-only)", "15:30:00 UTC · rules bbbbbbbbbbbb"]);

  await card.getByRole("button", { name: "Download" }).click();
  await expect(page.getByTestId("done-access")).toBeVisible();
  expect(asked.at(-1)).toBe(`?date=${day}`);

  await card.getByLabel("Access review snapshot").selectOption({ label: "08:00:00 UTC · rules aaaaaaaaaaaa (audit-only)" });
  await card.getByRole("button", { name: "Download" }).click();
  await expect.poll(() => asked.length).toBe(2);
  expect(new URLSearchParams(asked[1]).get("at")).toBe(`${day}T08:00:00Z`);

  // Another date: the choice is dropped.
  await page.getByLabel("Access review date").fill("2000-01-01");
  await page.getByLabel("Access review date").fill(day);
  await expect(card.getByLabel("Access review snapshot")).toHaveValue("");
});
