import { expect, test, type Page } from "@playwright/test";

// The Agents page against a real console (make test-ui): the bootstrap admin makes an agent for themselves, makes a key
// (shown once), revokes it and deletes the agent. The mocked screens and filters are in agents.spec.ts.
const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";
// No trace: it would hold the plain key (responses and page snapshots), and CI uploads traces.
test.use({ trace: "off" });

async function signInAsAdmin(page: Page) {
  test.skip(!password, "AMBIT_ADMIN_PASSWORD not set");
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
  // The hourly key count is in the console's memory; start from zero.
  expect((await page.request.post("/api/test/keys/reset-limits", { data: {} })).status()).toBe(204);
}

// Agent names are never reused, so every run uses its own display name.
const uniqueName = () => `e2e agent ${Date.now().toString(36)}`;

async function removeAgents(page: Page) {
  const list = await (await page.request.get("/api/agents")).json();
  for (const a of list.agents.filter((a: { displayName: string }) => a.displayName.startsWith("e2e agent"))) await page.request.delete(`/api/agents/${a.name}`);
}

test("agents: create, key shown once, revoke, delete", async ({ page }) => {
  await signInAsAdmin(page);
  const display = uniqueName();
  try {
    await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Agents" }).click();
    await expect(page).toHaveURL(/\/agents$/);
    await page.getByRole("button", { name: "New agent" }).click();
    const form = page.getByRole("form", { name: "New agent" });
    await form.getByLabel("Name", { exact: true }).fill(display);
    await form.getByLabel("Declared platform").selectOption("grok");
    await form.getByRole("button", { name: "Create agent" }).click();

    const detail = page.getByTestId("agent-detail");
    await expect(detail).toContainText(display);
    await expect(page.getByTestId("agent-owner")).toHaveText("admin@ambit.local");
    await expect(page.getByTestId("agent-state")).toHaveText("Active");
    await expect(page.getByTestId("agent-scope")).toContainText("None.");

    // The key. A console that has not yet published the scope clause answers "agents are not ready": try again.
    await detail.getByRole("button", { name: "New key" }).click();
    const keyForm = detail.getByRole("form", { name: "New key for the agent" });
    await keyForm.getByLabel("Key name").fill("e2e connector");
    await keyForm.getByLabel("Source address ranges (optional)").fill("203.0.113.0/24");
    for (let i = 0; i < 20; i++) {
      await keyForm.getByRole("button", { name: "Create key" }).click();
      const shown = await page.getByTestId("new-key").waitFor({ timeout: 5_000 }).then(() => true, () => false);
      if (shown) break;
      await page.waitForTimeout(3_000);
    }
    const plain = (await page.getByTestId("new-key").textContent())!;
    expect(plain).toMatch(/^ambit_[0-9A-Za-z]{43}$/);
    await expect(page.getByText("This is the only time the key is shown")).toBeVisible();
    await expect(page.getByTestId("agent-header")).toContainText(`Authorization: Bearer ${plain}`);
    await page.getByRole("button", { name: "Done" }).click();
    await expect(page.getByTestId("new-key")).toHaveCount(0);

    const row = detail.getByRole("row", { name: /e2e connector/ });
    await expect(row).toContainText(plain.slice(0, 10));
    await expect(row).not.toContainText(plain.slice(10));
    await expect(row).toContainText("203.0.113.0/24");

    // The list shows the agent with its declared platform; the key's agent field links it to its agent.
    await detail.getByRole("button", { name: "Close" }).click();
    const listRow = page.getByRole("row", { name: new RegExp(display) });
    await expect(listRow).toContainText("Grok Bot");
    await expect(listRow).toContainText("admin@ambit.local");
    await expect(listRow).toContainText("Person");
    await listRow.getByRole("button", { name: display }).click();

    // Revoke the key, then delete the agent.
    await row.getByRole("button", { name: "Revoke" }).click();
    await row.getByRole("button", { name: "Revoke key" }).click();
    await expect(detail.getByRole("row", { name: /e2e connector/ })).toHaveCount(0);
    await detail.getByRole("button", { name: "Delete agent" }).click();
    await page.getByRole("alertdialog", { name: "Delete agent" }).getByRole("button", { name: "Delete agent" }).click();
    await expect(page.getByTestId("agent-detail")).toHaveCount(0);
    await expect(page.getByRole("row", { name: new RegExp(display) })).toHaveCount(0);
  } finally {
    await removeAgents(page);
  }
});

test("Audit has the agent, platform and entrance filters", async ({ page }) => {
  await signInAsAdmin(page);
  await page.goto("/audit");
  await expect(page.locator("#audit-agent")).toBeVisible();
  await expect(page.locator("#audit-platform")).toBeVisible();
  await expect(page.locator("#audit-ingress")).toBeDisabled(); // it only narrows another filter
  await page.locator("#audit-agent").fill("no-such-agent-aaaaaa");
  await expect(page.locator("#audit-ingress")).toBeEnabled();
});

test("agents: stop is confirmed by the gateways, then start; the connection boxes are there", async ({ page }) => {
  test.setTimeout(300_000); // a stop is confirmed in seconds, but the console refuses a start for 60 seconds after it
  await signInAsAdmin(page);
  const display = uniqueName();
  try {
    await page.goto("/agents");
    await page.getByRole("button", { name: "New agent" }).click();
    const form = page.getByRole("form", { name: "New agent" });
    await form.getByLabel("Name", { exact: true }).fill(display);
    await form.getByRole("button", { name: "Create agent" }).click();
    const detail = page.getByTestId("agent-detail");
    await expect(page.getByTestId("agent-state")).toHaveText("Active");

    // Consent: nothing is ticked by default (the boxes exist when the install has apps with their own sign-in).
    const consent = page.getByTestId("consent");
    await expect(consent).toContainText("Nothing is ticked by default.");
    await expect(consent.getByRole("checkbox", { checked: true })).toHaveCount(0);
    await expect(consent.getByRole("alert")).toHaveCount(0); // the list of connections arrived

    // Stop: the page says Stopping, then Stopped only after the console reports every gateway confirmed it.
    await detail.getByRole("button", { name: "Stop agent" }).click();
    await page.getByRole("alertdialog", { name: "Stop agent" }).getByRole("button", { name: "Stop agent" }).click();
    await expect(page.getByTestId("agent-state")).toHaveText(/Stopping|Stopped/);
    await expect(page.getByTestId("agent-state")).toHaveText("Stopped", { timeout: 120_000 });
    await expect(page.getByTestId("stop-note-ok")).toContainText("seconds after the request (measured)");
    await expect(detail.getByRole("button", { name: "New key" })).toHaveCount(0);

    // Start: the console refuses it for one minute after the keys were deleted; ask again until it works.
    const start = detail.getByRole("button", { name: "Start agent" });
    for (let i = 0; i < 30 && (await page.getByTestId("agent-state").textContent()) !== "Active"; i++) {
      await start.click({ timeout: 3_000 }).catch(() => null);
      await page.waitForTimeout(5_000);
    }
    await expect(page.getByTestId("agent-state")).toHaveText("Active");
    await expect(detail.getByRole("button", { name: "Stop agent" })).toBeVisible();
  } finally {
    await removeAgents(page);
  }
});
