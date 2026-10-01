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

test("MCP servers: a server already running elsewhere is requested by URL", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: /MCP servers/ }).click();
  const name = `ui-remote-${Date.now().toString(36)}`;
  await page.getByRole("button", { name: "Request a server" }).click();
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByLabel("Already running elsewhere, reached by URL").check();
  await page.getByLabel("Server URL").fill("http://mcp.example.com/mcp");
  await page.getByRole("button", { name: "Send request" }).click();
  await expect(page.getByText(/use https for a server outside the cluster/)).toBeVisible();
  await page.getByLabel("Server URL").fill("https://mcp.example.com/mcp");
  await page.getByRole("button", { name: "Send request" }).click();
  const card = page.getByTestId(`request-${name}`);
  await expect(card.getByText("https://mcp.example.com/mcp")).toBeVisible();
  await expect(card.getByText(/streamable-http/)).toBeVisible();
});

test("rules form offers each server's tools", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.goto("/access-rules?edit=new");
  await page.getByLabel(/^time\b/).check();
  await page.getByLabel("Only these tools").check();
  await expect(page.getByLabel("convert_time")).toBeVisible();
  await expect(page.getByLabel("get_current_time")).toBeVisible();
  // Argument limits: typed values keep their commas and spaces while typing.
  await page.getByLabel("get_current_time").check();
  const limits = page.getByTestId("limits-time");
  await limits.getByRole("button", { name: "Add a limit" }).click();
  await limits.getByLabel("Argument name").fill("timezone");
  await limits.getByLabel("Values").pressSequentially("UTC, Europe/Paris");
  await expect(limits.getByLabel("Values")).toHaveValue("UTC, Europe/Paris");
});

test("Sign-in settings: Check verifies the provider and shows the callback URL", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Sign-in" }).click();
  await expect(page.getByRole("heading", { name: "Sign-in", exact: true })).toBeVisible();
  await page.getByLabel("Issuer URL").fill("http://keycloak.localtest.me:8080/realms/acme/");
  await page.getByRole("button", { name: "Check" }).click();
  await expect(page.getByText(/a trailing slash counts/)).toBeVisible();
  await page.getByLabel("Issuer URL").fill("http://keycloak.localtest.me:8080/realms/acme");
  await page.getByRole("button", { name: "Check" }).click();
  await expect(page.getByTestId("sso-check")).toContainText("http://console.localtest.me:8080/dex/callback");
});

test("My connections page lists the apps you can connect", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "My connections" }).click();
  await expect(page.getByRole("heading", { name: "My connections" })).toBeVisible();
  await expect(page.getByText("Each call uses your grant, never someone else's.")).toBeVisible();
});

test("Offboarding page shows the SCIM endpoint and reveals the token on request", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Offboarding" }).click();
  await expect(page.getByText("http://console.localtest.me:8080/scim/v2")).toBeVisible();
  await page.getByRole("button", { name: "Show token" }).click();
  await expect(page.getByRole("button", { name: "Show token" })).toHaveCount(0);
});

test("Audit page searches one person's calls", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Audit" }).click();
  await page.getByLabel("Email").fill("alice@acme.test");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText(/\d+ tool and model calls: \d+ refused by policy, \d+ failed/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Export CSV" })).toBeVisible();
});

test("Budgets page adds, changes and removes a group budget", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Budgets" }).click();
  await expect(page.getByRole("heading", { name: "Budgets" })).toBeVisible();
  await page.getByRole("button", { name: "New budget" }).click();
  await page.getByLabel("Group (as your identity provider names it)").fill("ui-test-group");
  await page.getByLabel("Each person, requests per day").fill("25");
  await page.getByRole("button", { name: "Add budget" }).click();
  const card = page.getByTestId("budget-ui-test-group");
  await expect(card.getByText("25 per day")).toBeVisible();
  await card.getByRole("button", { name: "Edit" }).click();
  await page.getByLabel("Each person, requests per day").fill("40");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(card.getByText("40 per day")).toBeVisible();
  await card.getByRole("button", { name: "Delete" }).click();
  await card.getByRole("button", { name: "Remove budget" }).click();
  await expect(card).toHaveCount(0);
});

test("Apps page adds an app for per-user sign-in and removes it", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Apps" }).click();
  await page.getByRole("button", { name: "Add an app" }).click();
  await expect(page.getByText(/\/connections\/callback/)).toBeVisible();
  const name = `ui-app-${Date.now().toString(36)}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByLabel("Name users see").fill("UI Test App");
  await page.getByLabel("Authorization URL").fill("https://auth.example.com/authorize");
  await page.getByLabel("Token URL").fill("https://auth.example.com/token");
  await page.getByLabel(/Revocation URL/).fill("https://api.github.com/applications/ui-client/grant");
  await page.getByLabel("Client ID").fill("ui-client");
  await page.getByLabel("Client secret").fill("ui-secret-not-shown");
  await page.getByRole("button", { name: "Add app" }).click();
  const row = page.getByTestId(`app-${name}`);
  await expect(row.getByText("UI Test App")).toBeVisible();
  await expect(page.getByText("ui-secret-not-shown")).toHaveCount(0);
  await expect(row.getByRole("cell", { name: "Yes", exact: true })).toBeVisible();
  await row.getByRole("button", { name: "Remove" }).click();
  await row.getByRole("button", { name: "Remove app" }).click();
  await expect(row).toHaveCount(0);
});

test("Dependencies page shows each component, its failure mode and its health", async ({ page }) => {
  await signInAsBootstrapAdmin(page);
  await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Dependencies" }).click();
  const gateway = page.getByTestId("dep-Gateway");
  await expect(gateway.getByText("Answers")).toBeVisible();
  await expect(gateway.getByText(/All MCP and LLM requests fail/)).toBeVisible();
  await expect(page.getByTestId("dep-ClickHouse").getByText("answers queries")).toBeVisible();
  await expect(page.getByTestId("dep-Valkey").getByText(/Fails (open|closed)\./)).toBeVisible();
  await expect(page.getByText("Everything answers")).toBeVisible();
});
