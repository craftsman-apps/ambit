import { expect, test, type Page } from "@playwright/test";

const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";

// Fail any test whose page reports a Content-Security-Policy violation.
test.beforeEach(async ({ page }) => {
  const violations: string[] = [];
  page.on("console", (m) => { if (/Content Security Policy/i.test(m.text())) violations.push(m.text()); });
  (page as Page & { _cspViolations?: string[] })._cspViolations = violations;
});
test.afterEach(async ({ page }) => {
  expect((page as Page & { _cspViolations?: string[] })._cspViolations ?? []).toEqual([]);
});

async function signInAsBootstrapAdmin(page: Page) {
  test.skip(!password, "AMBIT_ADMIN_PASSWORD not set");
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
}

test("unauthenticated visitors land on the login page", async ({ page }) => {
  await page.goto("/access-rules");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
});

test("bootstrap admin signs in and sees the overview", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await expect(page.getByText("You are using the bootstrap admin.")).toBeVisible();
});

test("theme switch changes and remembers the mode", async ({ page }) => {
  await page.goto("/login");
  await page.getByRole("button", { name: "Dark" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.getByRole("button", { name: "System" }).click();
  await expect(page.locator("html")).not.toHaveAttribute("data-theme", /.+/);
});

test("admin creates a rule in the form, sees the merged policy, and deletes it", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: /Access rules/ }).click();
  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByLabel("Name", { exact: true }).fill("ui-test-finance");
  await page.getByLabel("Group names, separated by commas").fill("finance");
  await page.getByLabel(/nemotron-3-super/).check();
  await page.getByRole("button", { name: "Create rule" }).click();
  await expect(page).toHaveURL(/rule=ui-test-finance/);
  await expect(page.getByText("created in the console by admin@ambit.local")).toBeVisible();
  await expect(page.getByText(/jwt\.groups\.exists\(g, g in \["finance"/)).toBeVisible();
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("button", { name: "Delete ui-test-finance" }).click();
  await expect(page.getByRole("cell", { name: "ui-test-finance" })).toHaveCount(0);
});
