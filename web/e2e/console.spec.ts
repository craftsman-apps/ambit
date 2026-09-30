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

test("MCP servers: tools are listed, and a request waits for another approver", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: /MCP servers/ }).click();
  await expect(page.getByRole("heading", { name: "MCP servers" })).toBeVisible();
  const time = page.getByRole("row", { name: /^time\b/ }).first();
  await expect(time.getByText("Running")).toBeVisible();
  await time.click();
  await expect(page.getByText("get_current_time", { exact: true })).toBeVisible();

  const name = `ui-${Date.now().toString(36)}`; // unique per run; step 4b's cleanup removes requests
  await page.getByRole("button", { name: "Request a server" }).click();
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByLabel("Image, pinned by digest").fill("ghcr.io/stacklok/dockyard/uvx/mcp-server-time:2026.8.18");
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByText(/pin the image by digest/)).toBeVisible();
  await page.getByLabel("Image, pinned by digest").fill(
    "ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949");
  await page.getByRole("button", { name: "Send request" }).click();
  const card = page.getByTestId(`request-${name}`);
  await expect(card.getByText("You asked for this, so another approver decides.")).toBeVisible();
  await expect(card.getByRole("button", { name: /^Approve/ })).toHaveCount(0);
});

test("rules form offers each server's tools", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.goto("/access-rules?edit=new");
  await page.getByLabel(/^time\b/).check();
  await page.getByLabel("Only these tools").check();
  await expect(page.getByLabel("convert_time")).toBeVisible();
  await expect(page.getByLabel("get_current_time")).toBeVisible();
});
