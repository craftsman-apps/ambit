import { expect, test, type Page } from "@playwright/test";
import type { Savings } from "../src/lib/savings";

const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";

// The page is tested against a fixed answer of /api/savings, so labels, empty states and the "later turns" control do
// not depend on what the cluster has recorded. The API itself is tested in Go and in test/stepsavings.sh.
const comp = (o: Partial<Savings["totals"]["compression"]> = {}) => ({
  results: 40, bytesIn: 41_000_000, bytesOut: 14_000_000, tokensSaved: 9_800_000, tokensLow: 7_350_000, tokensHigh: 12_250_000,
  dollars: 29, dollarsLow: 21.75, dollarsHigh: 36.25, priceSource: "llm", ...o,
});
const fixture: Savings = {
  estimated: true,
  settings: { referenceInputPrice: null },
  totals: { compression: comp(), routing: { requests: 12400, dollars: 182 } },
  days: [
    { day: "2026-10-30", compressionDollars: 14, routingDollars: 90, compressionTokens: 4_700_000 },
    { day: "2026-10-31", compressionDollars: 15, routingDollars: 92, compressionTokens: 5_100_000 },
  ],
  groups: [
    { group: "eng", compression: comp(), routing: { requests: 12400, dollars: 182 } },
    { group: "support", compression: comp({ tokensSaved: 4_900_000, dollars: 14.5 }), routing: { requests: 6200, dollars: 91 } }, // half of eng's dollars: a person in both groups counts in both
    { group: "ml-research", compression: comp({ tokensSaved: 1_000_000, dollars: null, dollarsLow: null, dollarsHigh: null, priceSource: "none" }), routing: { requests: 0, dollars: null } },
  ],
  servers: [{ server: "github", type: "json", results: 40, bytesIn: 41_000_000, bytesOut: 14_000_000, tokensSaved: 9_800_000, omitted: 0 }],
  routers: [{ router: "auto", baselineModels: ["claude-sonnet", "gemma"], requests: 12400, dollars: 182, unpricedModels: ["local-llama"], missing: false,
    classifierRequests: 12400, classifierDollars: 1.86 }],
  notes: ["local-llama has no price, so it is not a baseline for router auto."],
};
const empty: Savings = { ...fixture, totals: { compression: comp({ results: 0, bytesIn: 0, bytesOut: 0, tokensSaved: 0, tokensLow: 0, tokensHigh: 0, dollars: null, dollarsLow: null, dollarsHigh: null, priceSource: "none" }), routing: { requests: 0, dollars: null } },
  days: [], groups: [], servers: [], routers: [], notes: [] };

async function signInAsBootstrapAdmin(page: Page) {
  test.skip(!password, "AMBIT_ADMIN_PASSWORD not set");
  await page.goto("/");
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
}

async function open(page: Page, body: unknown, status = 200) {
  await page.route("**/api/savings*", (r) => r.fulfill({ status, contentType: typeof body === "string" ? "text/plain" : "application/json", body: typeof body === "string" ? body : JSON.stringify(body) }));
  await signInAsBootstrapAdmin(page);
  await page.getByRole("link", { name: "Savings" }).click();
  await expect(page.getByRole("heading", { name: "Savings", level: 1 })).toBeVisible();
}

test("every figure is labelled as an estimate and every chart has a text table", async ({ page }) => {
  await open(page, { from: "2026-10-03", to: "2026-11-02", since: "2026-10-30", classifierSince: "2026-11-01", savings: fixture });
  await expect(page.getByText("All figures are estimates.")).toBeVisible();
  await expect(page.getByText("counted since 2026-10-30")).toBeVisible();
  const totals = page.getByLabel("Totals");
  for (const label of ["Tokens saved", "Compression", "Routing", "Total"]) {
    await expect(totals.getByText(label, { exact: false }).first()).toBeVisible();
  }
  await expect(totals.getByText("(estimated)")).toHaveCount(4);
  await expect(totals).toContainText("~9.8 M");
  await expect(totals).toContainText("~$29.00");
  await expect(totals).toContainText("~$182.00");
  await expect(totals).toContainText("~$211.00");
  // a visible "how" line under each tile, and the caveats
  await expect(totals).toContainText("Net of the classifier's own cost");
  await expect(page.getByRole("table", { name: "Routing by router" })).toContainText("~$1.86");
  await expect(page.getByText("is subtracted (counted since 2026-11-01)")).toBeVisible();
  await expect(page.getByLabel("Notes")).toContainText("local-llama has no price");
  // a text table for each chart, and the server and router tables
  for (const name of ["Dollars saved per day", "Savings by group", "Compression by server and type", "Routing by router"]) {
    await expect(page.getByRole("table", { name })).toBeVisible();
  }
  await expect(page.getByRole("img", { name: /Estimated dollars saved per day/ })).toBeVisible();
  await expect(page.getByRole("img", { name: /Estimated share of the total dollars saved, for the top groups/ })).toBeVisible();
  await expect(page.getByRole("table", { name: "Savings by group" })).toContainText("tokens only"); // ml-research has no price
  await expect(page.getByLabel("What is not counted")).toContainText("Per-group rows can overlap");
});

test("the group chart shows each group's share of the total, and says the shares overlap", async ({ page }) => {
  await open(page, { from: "2026-10-03", to: "2026-11-02", since: "2026-10-30", savings: fixture });
  await expect(page.getByRole("heading", { name: "Share of the total saved, per group (estimated)" })).toBeVisible();
  await expect(page.getByText("add up to more than 100%")).toBeVisible();
  const chart = page.getByRole("img", { name: /Estimated share of the total dollars saved/ });
  await expect(chart.getByText("100%", { exact: true })).toBeVisible(); // eng: all of the total
  await expect(chart.getByText("50%", { exact: true })).toBeVisible(); // support
  await expect(chart.locator("text", { hasText: /\$/ })).toHaveCount(0); // no dollar label that could be read against the total tile
  await expect(page.getByRole("table", { name: "Savings by group" })).toContainText("~$14.50"); // plain dollars stay in the table
});

test("later turns multiply compression only; 0 is the headline", async ({ page }) => {
  await open(page, { from: "2026-10-03", to: "2026-11-02", since: "2026-10-30", savings: fixture });
  const totals = page.getByLabel("Totals");
  await expect(page.getByLabel("Later turns that carry each result")).toHaveValue("0");
  await expect(page.getByText("0 = count the first send only")).toBeVisible();
  await page.getByLabel("Later turns that carry each result").selectOption("3");
  await expect(page.getByText("as if each result is sent 3 more times (your assumption, not measured)")).toBeVisible();
  await expect(totals).toContainText("~39.2 M"); // 9.8 M x 4
  await expect(totals).toContainText("~$116.00"); // 29 x 4
  await expect(totals).toContainText("~$182.00"); // routing is not multiplied
  await expect(totals).toContainText("~$298.00");
  await expect(page.getByRole("table", { name: "Compression by server and type" })).toContainText("39,200,000");
});

test("empty sections say why", async ({ page }) => {
  await open(page, { from: "2026-10-03", to: "2026-11-02", since: "", savings: empty });
  await expect(page.getByText("No tool result was compressed in this period.")).toBeVisible();
  await expect(page.getByText("No request went through a ModelRouter in this period.").first()).toBeVisible();
  await expect(page.getByText("No data in this period.").first()).toBeVisible();
});

test("without the audit trail the page says how to turn it on; a store error is shown, not an empty page", async ({ page }) => {
  await open(page, "404 page not found", 404); // what the console answers for a route it does not have
  await expect(page.getByText("Savings need the audit trail")).toBeVisible();
  await page.unroute("**/api/savings*");
  await page.route("**/api/savings*", (r) => r.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "the savings summaries are not created yet. Usage and Audit are not affected." }) }));
  await page.reload();
  await expect(page.getByRole("alert")).toContainText("not created yet");
});
