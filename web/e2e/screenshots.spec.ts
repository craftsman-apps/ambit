import { expect, test, type Page } from "@playwright/test";

// Documentation screenshots of every console page, signed in as priya (an admin, through the test
// identity provider). Skipped unless SCREENSHOTS=1; writes PNGs to SCREENSHOT_DIR. Adds a budget for
// eng and a remote server request through the UI first, so the pages show real data.
const dir = process.env.SCREENSHOT_DIR ?? "screenshots";
const user = process.env.SCREENSHOT_USER ?? "priya";
const pass = process.env.SCREENSHOT_PASSWORD ?? "priya-test-pass"; // test/keycloak/keycloak.yaml

test.use({ viewport: { width: 1440, height: 900 }, colorScheme: "light" });

async function shot(page: Page, name: string) {
  // A client-side page change starts its requests after the first paint: wait for them, then for
  // the page's own loading message to go.
  await page.waitForTimeout(400);
  await page.waitForLoadState("networkidle");
  await expect(page.getByText("Loading…")).toHaveCount(0);
  await page.screenshot({ path: `${dir}/${name}.png` }); // the window, not the full page: long lists (Audit) show their top
}
const nav = (page: Page, name: string | RegExp) => page.getByRole("navigation", { name: "Console" }).getByRole("link", { name }).click();

test("console screenshots", async ({ page }) => {
  test.skip(process.env.SCREENSHOTS !== "1", "set SCREENSHOTS=1");
  test.setTimeout(180_000);
  await page.goto("/");
  await page.getByRole("button", { name: /Continue with/ }).or(page.getByRole("link", { name: /Continue with/ })).click();
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(pass);
  await page.locator("#kc-login").click();
  await expect(page.getByTestId("signed-in-as")).toContainText("Signed in as");

  // Data, through the UI.
  await nav(page, "Budgets");
  if (await page.getByTestId("budget-eng").count() === 0) {
    await page.getByRole("button", { name: "New budget" }).click();
    await page.getByLabel("Group (as your identity provider names it)").fill("eng");
    await page.getByLabel("Whole group, requests per day").fill("200");
    await page.getByLabel("Each person, requests per day").fill("5");
    await page.getByRole("button", { name: "Add budget" }).click();
    await expect(page.getByTestId("budget-eng")).toBeVisible();
  }
  await nav(page, /MCP servers/);
  if (await page.getByTestId("request-jira").count() === 0) {
    await page.getByRole("button", { name: "Request a server" }).click();
    await page.getByLabel("Name", { exact: true }).fill("jira");
    await page.getByLabel("Display name").fill("Jira");
    await page.getByLabel("Already running elsewhere, reached by URL").check();
    await page.getByLabel("Server URL").fill("https://mcp.atlassian.example/v1/mcp");
    await page.getByLabel("Sign-in for each user").selectOption("atlassian");
    await page.getByLabel("Why your team needs it").fill("Engineering triages Jira tickets from Claude Code; each person uses their own Atlassian account.");
    await shot(page, "servers-request-form");
    await page.getByRole("button", { name: "Send request" }).click();
    await expect(page.getByTestId("request-jira")).toBeVisible();
  }

  await nav(page, "Overview");
  await shot(page, "overview");
  await nav(page, "Access rules");
  await page.getByRole("row", { name: /^eng-time\b/ }).click();
  await shot(page, "access-rules");
  await nav(page, "Models");
  await shot(page, "models");
  await nav(page, /MCP servers/);
  await page.getByRole("row", { name: /^time\b/ }).first().click();
  await shot(page, "servers");
  await nav(page, "Budgets");
  await shot(page, "budgets");
  await nav(page, "Audit");
  await page.getByLabel("Email").fill("alice@acme.test");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText(/tool and model calls/)).toBeVisible();
  await shot(page, "audit");
  await nav(page, "Usage");
  await shot(page, "usage");
  await nav(page, "My connections");
  await shot(page, "connections");
  await nav(page, "Apps");
  await shot(page, "apps");
  await page.getByRole("button", { name: "Add an app" }).click();
  await shot(page, "apps-form");
  await nav(page, "Sign-in");
  await shot(page, "sign-in");
  await nav(page, "Offboarding");
  await shot(page, "offboarding");
});
