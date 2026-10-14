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
  // The hourly creation count is in the console's memory; start each test from zero.
  expect((await page.request.post("/api/test/keys/reset-limits", { data: {} })).status()).toBe(204);
}

const nav = (page: Page) => page.getByRole("navigation", { name: "Console" });
type KeyRow = { id: string; name: string };
const listKeys = async (page: Page): Promise<KeyRow[]> => (await (await page.request.get("/api/keys")).json()).keys;
const revokeAll = async (page: Page) => { for (const k of (await listKeys(page)).filter((k) => k.name.startsWith("e2e"))) await page.request.delete(`/api/keys/${k.id}`); };
const createViaAPI = (page: Page, name: string) =>
  page.request.post("/api/keys", { data: { name, scope: "llm", days: 30 } });

test("create a key, see it once, revoke it", async ({ page }) => {
  await signInAsAdmin(page);
  try {
    await nav(page).getByRole("link", { name: "Keys", exact: true }).first().click();
    await page.getByRole("button", { name: "Create key" }).click();
    await page.getByLabel("Name").fill("e2e laptop");
    await page.getByLabel("Scope").selectOption("both");
    await page.getByLabel("End date").selectOption("30");
    await page.getByRole("button", { name: "Create", exact: true }).click();
    const key = await page.getByTestId("new-key").textContent();
    expect(key).toMatch(/^ambit_[0-9A-Za-z]{43}$/);
    await expect(page.getByText("This is the only time the key is shown")).toBeVisible();
    await expect(page.getByTestId("snippet-claudeCode-0")).toContainText(key!);
    await page.getByRole("button", { name: "Done" }).click();
    await expect(page.getByTestId("new-key")).toHaveCount(0);
    const row = page.getByRole("row", { name: /e2e laptop/ });
    await expect(row).toContainText(key!.slice(0, 10));
    await expect(row).not.toContainText(key!.slice(10));
    await row.getByRole("button", { name: "Revoke" }).click();
    await page.getByRole("button", { name: "Revoke key" }).click(); // in-page confirmation, no window.confirm
    await expect(row).toHaveCount(0);
  } finally {
    await revokeAll(page);
  }
});

test("admins see every key and the settings read-only", async ({ page }) => {
  await signInAsAdmin(page);
  await nav(page).getByRole("link", { name: "Keys", exact: true }).last().click();
  await expect(page).toHaveURL(/\/settings\/keys$/);
  await expect(page.getByText("Inactivity limit")).toBeVisible();
  await expect(page.getByRole("spinbutton")).toHaveCount(0); // settings are not editable
});

test("a key that stops within 7 days shows a banner", async ({ page }) => {
  await signInAsAdmin(page);
  try {
    const made = await createViaAPI(page, "e2e banner");
    expect(made.status()).toBe(201);
    const id = (await made.json()).view.id;
    const until = new Date(Date.now() + 3 * 864e5).toISOString().replace(/\.\d+Z$/, "Z");
    const r = await page.request.post(`/api/test/keys/${id}/expire`, { data: { until } });
    expect(r.status()).toBe(204);
    await page.goto("/");
    await expect(page.getByTestId("key-banner")).toContainText("e2e banner");
    await expect(page.getByTestId("key-banner")).toContainText("stops on");
    await expect(page.getByTestId("key-banner")).toContainText("Sign in again"); // until is before the end date: inactivity
    await page.goto("/keys");
    await expect(page.getByRole("row", { name: /e2e banner/ }).getByRole("cell", { name: "Stops soon", exact: true })).toBeVisible();
  } finally {
    await revokeAll(page);
  }
});

test("the hourly limit shows its message", async ({ page }) => {
  await signInAsAdmin(page);
  try {
    // Create keys through the API until the hourly allowance of 10 is used; earlier keys this hour count too,
    // so stop at the first 429 (at most 10 tries).
    for (let i = 0; i < 10; i++) {
      const r = await createViaAPI(page, `e2e fill ${i}`);
      if (r.status() === 429) break;
      expect(r.status()).toBe(201);
    }
    await nav(page).getByRole("link", { name: "Keys", exact: true }).first().click();
    await page.getByRole("button", { name: "Create key" }).click();
    await page.getByLabel("Name").fill("e2e eleventh");
    await page.getByRole("button", { name: "Create", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText("at most 10 keys per hour");
  } finally {
    await revokeAll(page);
  }
});
