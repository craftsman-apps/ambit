import { expect, test } from "@playwright/test";

// The Savings and Usage pages on records that look like real use (test/savings-seed/seed.sh: most people in one
// group, one in ten in two). Skipped unless SAVINGS_SEEDED=1. With SCREENSHOT_DIR it saves a screenshot of each page.
const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";
const dir = process.env.SCREENSHOT_DIR;
test.use({ viewport: { width: 1280, height: 900 }, colorScheme: "light" });

test("Savings and Usage on seeded records", async ({ page }) => {
  test.skip(process.env.SAVINGS_SEEDED !== "1" || !password, "set SAVINGS_SEEDED=1 and AMBIT_ADMIN_PASSWORD");
  await page.goto("/");
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");

  await page.getByRole("link", { name: "Savings" }).click();
  await expect(page.getByRole("heading", { name: "Savings", level: 1 })).toBeVisible();
  const days = page.getByRole("table", { name: "Dollars saved per day" }).locator("tbody tr td:first-child");
  await expect(days.first()).toBeVisible();
  const oldest = (await days.allInnerTexts()).sort()[0];
  // "Counted since" is the oldest day the summaries hold, the first day of the chart: never a date inside it.
  await expect(page.getByText(`counted since ${oldest}`)).toBeVisible();
  // Every group is a part of the total; the shares of all groups add up to more than 100% (people in two groups).
  const shares = (await page.getByRole("table", { name: "Savings by group" }).locator("tbody tr td:last-child").allInnerTexts()).map((t) => parseInt(t));
  expect(shares.length).toBeGreaterThanOrEqual(3);
  for (const s of shares) expect(s).toBeLessThanOrEqual(100);
  expect(shares.reduce((a, b) => a + b, 0)).toBeGreaterThan(100);
  if (dir) await page.screenshot({ path: `${dir}/savings-after.png`, fullPage: true });

  await page.getByRole("link", { name: "Usage" }).click();
  await expect(page.getByRole("heading", { name: "Usage", level: 1 })).toBeVisible();
  await expect(page.getByRole("table").locator("tbody tr").first()).toBeVisible();
  expect(new Set(await page.getByRole("table").locator("tbody tr td:nth-child(2)").allInnerTexts())).toEqual(new Set(["eng", "support", "ml-research"]));
  if (dir) await page.screenshot({ path: `${dir}/usage-after.png`, fullPage: true });
});
