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
  expect((await page.request.post("/api/test/keys/reset-limits", { data: {} })).status()).toBe(204);
}

// Names are never reused, so every run makes a new one.
const uniqueName = () => `e2e-${Date.now().toString(36)}`;

test("service accounts: create, key shown once, rotate, delete", async ({ page }) => {
  await signInAsAdmin(page);
  const name = uniqueName();
  const identity = `${name}@svc.ambit.internal`;
  try {
    await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Service accounts" }).click();
    await expect(page).toHaveURL(/\/settings\/service-accounts$/);
    await page.getByRole("button", { name: "New service account" }).click();
    await page.getByLabel("Name").fill(name);
    await expect(page.getByTestId("sa-identity")).toHaveText(identity);
    await page.getByLabel("Group").fill("eng");
    await page.getByLabel("Description").fill("Playwright run");
    await page.getByRole("button", { name: "Create", exact: true }).click();
    const card = page.getByTestId(`sa-${name}`);
    await expect(card).toContainText(identity);
    await expect(card).toContainText("eng");

    // first key: shown once, with the CI snippets; the workflow never holds the key
    await card.getByRole("button", { name: "New key" }).click();
    await card.getByLabel("Key name").fill("gha-1");
    await card.getByRole("button", { name: "Create key" }).click();
    const first = await card.getByTestId("new-key").textContent();
    expect(first).toMatch(/^ambit_[0-9A-Za-z]{43}$/);
    await expect(card.getByTestId("snippet-env-0")).toContainText(first!);
    await card.getByRole("tab", { name: "GitHub Actions" }).click();
    await expect(card.getByTestId("snippet-github-0")).toContainText("${{ secrets.AMBIT_API_KEY }}");
    await expect(card.getByTestId("snippet-github-0")).not.toContainText(first!);
    await card.getByRole("button", { name: "Done" }).click();
    await expect(card.getByTestId("new-key")).toHaveCount(0);
    const row1 = card.getByRole("row", { name: /gha-1/ });
    await expect(row1).toContainText(first!.slice(0, 10));
    await expect(row1).not.toContainText(first!.slice(10));
    await expect(row1).toContainText("admin@ambit.local"); // issued by

    // rotate: the second key, then revoke the old one from the same screen
    await card.getByRole("button", { name: "Rotate key" }).click();
    await card.getByLabel("Key name").fill("gha-2");
    await card.getByRole("button", { name: "Create key" }).click();
    const second = await card.getByTestId("new-key").textContent();
    expect(second).not.toBe(first);
    await expect(card.getByTestId("rotate-old")).toContainText("gha-1");
    await card.getByRole("button", { name: "Revoke the old key now" }).click();
    await expect(card.getByText("The old key is revoked.")).toBeVisible();
    await card.getByRole("button", { name: "Done" }).click();
    await expect(card.getByRole("row", { name: /gha-1/ })).toHaveCount(0);
    await expect(card.getByRole("row", { name: /gha-2/ })).toHaveCount(1);

    // Settings → Keys lists it with the service account badge
    await page.getByRole("navigation", { name: "Console" }).getByRole("link", { name: "Keys", exact: true }).last().click();
    await expect(page.getByRole("row", { name: new RegExp(identity) })).toContainText("service account");
    await page.goBack();

    // delete: the confirmation names the identity and the live keys; the name is then taken for good
    await card.getByRole("button", { name: "Delete service account" }).click();
    await expect(page.getByRole("alertdialog", { name: "Delete service account" })).toContainText(`${identity} and its 1 live key`);
    await page.getByRole("alertdialog").getByRole("button", { name: "Delete service account" }).click();
    await expect(card).toHaveCount(0);
    const again = await page.request.post("/api/admin/service-accounts", { data: { name, group: "eng", description: "" } });
    expect(again.status()).toBe(409);
  } finally {
    await page.request.delete(`/api/admin/service-accounts/${name}`);
  }
});
