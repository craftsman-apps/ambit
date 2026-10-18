import { readFileSync } from "node:fs";
import { expect, test, type Page, type Route } from "@playwright/test";

// Audit-only mode in the console. Every API answer is a mock (page.route) and the clock is fixed, so this needs no
// data: it runs against `npm run dev` with AMBIT_URL=http://localhost:5173. The real console sends a person without a
// session to /login, so against a console (make test-ui) it first signs in as the bootstrap admin
// (AMBIT_ADMIN_PASSWORD). AMBIT_SCREENSHOTS_DIR writes the screens for review.
const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";
const NOW = new Date("2026-10-15T10:00:00Z");

type Config = Record<string, unknown>;
const audit: Config = { mode: "direct", accessMode: "audit", modeSince: "2026-10-12T10:00:00Z", rulesVersion: "3f9a1c2b7d10", rulesSince: "2026-10-14T16:20:00Z" };
const enforce: Config = { mode: "direct", accessMode: "enforce", modeSince: "2026-10-01T00:00:00Z", rulesVersion: "3f9a1c2b7d10", rulesSince: "2026-10-01T00:00:00Z" };

const call = (time: string, extra: Record<string, unknown> = {}) => ({
  time, email: "alice@acme.test", route: "ambit/llm", method: "", server: "", tool: "", model: "gpt-4o", status: "200", error: "",
  inputTokens: 120, outputTokens: 40, router: "", label: "", labelReason: "", accessMode: "audit", mark: "allow", rulesVersion: "3f9a1c2b7d10", outcome: "allow", ...extra,
});
const calls = [
  call("2026-10-15T09:58:10Z", { mark: "refuse:model", outcome: "would-refuse" }),
  call("2026-10-15T09:55:00Z", { method: "tools/call", server: "github", tool: "create_issue", model: "", route: "ambit/mcp", mark: "refuse:tool", outcome: "would-refuse", inputTokens: 0, outputTokens: 0 }),
  call("2026-10-15T09:50:00Z", { model: "auto → mock/echo", mark: "other-model", outcome: "other-model" }),
  call("2026-10-15T09:40:00Z", { mark: "", outcome: "mark-missing" }),
  call("2026-10-15T09:30:00Z", { status: "403", error: "authorization failed", mark: "", outcome: "refused", inputTokens: 0, outputTokens: 0 }),
  call("2026-10-15T09:20:00Z", { mark: "allow", outcome: "allow" }),
  call("2026-10-15T09:10:00Z", { method: "tools/list", route: "ambit/mcp", model: "", mark: "", outcome: "n/a", inputTokens: 0, outputTokens: 0 }),
];
const summary = {
  from: "2026-10-08T10:00:00Z", to: "2026-10-15T10:01:00Z", rules: "all", requests: 1240, people: 18, markMissing: 0, refused: 12, newest: "2026-10-15T09:58:10Z", limited: false,
  groups: [
    { group: "data-science", target: "gpt-4o", outcome: "would-refuse", requests: 812, people: 9 },
    { group: "eng", target: "gpt-4o", outcome: "would-refuse", requests: 388, people: 11 },
    { group: "(no group)", target: "github · create_issue", outcome: "would-refuse", requests: 40, people: 3 },
    { group: "eng", target: "auto → mock/echo", outcome: "other-model", requests: 7, people: 2 },
  ],
};

async function setup(page: Page, o: { role?: string; config: Config; summary?: object; calls?: object[]; seen?: string[] }) {
  await page.clock.setFixedTime(NOW);
  const json = (route: Route, body: unknown) => route.fulfill({ contentType: "application/json", body: JSON.stringify(body) });
  await page.route("**/api/**", (r) => json(r, []));
  await page.route("**/api/me", (r) => json(r, { email: "priya@acme.test", name: "Priya", role: o.role ?? "admin", bootstrap: false }));
  await page.route("**/api/keys*", (r) => json(r, { settings: { enabled: true }, keys: [] }));
  await page.route("**/api/config", (r) => json(r, o.config));
  await page.route("**/api/audit?*", (r) => json(r, { email: "alice@acme.test", calls: o.calls ?? calls, limited: false }));
  await page.route("**/api/audit/would-refuse*", (r) => { o.seen?.push(new URL(r.request().url()).search); return json(r, o.summary ?? summary); });
}

async function snap(page: Page, name: string) {
  const dir = process.env.AMBIT_SCREENSHOTS_DIR;
  if (!dir) return;
  await page.waitForLoadState("networkidle");
  await page.screenshot({ path: `${dir}/${name}.png`, fullPage: true });
}

async function search(page: Page) {
  await page.goto("/audit");
  await page.locator("#audit-email").fill("alice@acme.test");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByRole("row", { name: /tools\/call · create_issue/ })).toBeVisible();
}

test.use({ viewport: { width: 1280, height: 800 }, colorScheme: "light" });

test.beforeEach(async ({ page }) => {
  if (!password) return;
  await page.goto("/");
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
});

test("the banner shows only in audit-only mode, with the days", async ({ page }) => {
  await setup(page, { config: audit });
  await page.goto("/access-rules");
  const banner = page.getByTestId("access-mode-banner");
  await expect(banner).toContainText("Audit-only mode since 2026-10-12 (3 days).");
  await expect(banner).toContainText("every tool is listed to everyone");
  await expect(banner).toContainText("Access rules are recorded, not enforced.");
  await expect(banner.getByRole("link", { name: "See what would be refused" })).toBeVisible();
  await expect(banner).toContainText("access.mode: enforce");
  await snap(page, "banner-audit");

  await page.unrouteAll();
  await setup(page, { config: enforce });
  await page.goto("/access-rules");
  await expect(page.getByRole("navigation", { name: "Console" })).toBeVisible();
  await expect(page.getByTestId("access-mode-banner")).toHaveCount(0);
});

test("everyone sees the banner; only admins get the link and the upgrade step", async ({ page }) => {
  await setup(page, { role: "viewer", config: audit });
  await page.goto("/access-rules");
  const banner = page.getByTestId("access-mode-banner");
  await expect(banner).toContainText("Audit-only mode since 2026-10-12");
  await expect(banner.getByRole("link")).toHaveCount(0);
  await expect(banner).not.toContainText("access.mode");
});

test("a switch that is not complete is shown with its error", async ({ page }) => {
  const pending = { mode: "audit", rulesVersion: "bbbbbbbbbbbb", step: "authorization", error: "waiting for agentgateway to apply AgentgatewayModel/gpt-4o generation 3: Accepted is Unknown (Pending: waiting for controller)" };
  await setup(page, { config: { ...enforce, pending } });
  await page.goto("/access-rules");
  await expect(page.getByTestId("mode-pending-banner")).toContainText("Mode switch to audit is not complete: waiting for agentgateway to apply AgentgatewayModel/gpt-4o generation 3");
  await expect(page.getByTestId("access-mode-banner")).toHaveCount(0); // the published mode is still enforce
  await snap(page, "banner-switch-not-complete");
});

test("a rules change that agentgateway rejected names the Model, the reason and the next step, and links the Models page", async ({ page }) => {
  const error = "agentgateway rejected Model/mistral-x: backend mistral-x: GCP credential JSON missing `type` field. Check the provider secret of Model/mistral-x, or remove the Model.";
  const pending = { mode: "enforce", rulesVersion: "bbbbbbbbbbbb", step: "authorization", error, objects: ["Model/mistral-x"] };
  await setup(page, { config: { ...enforce, pending } });
  await page.goto("/access-rules");
  const banner = page.getByTestId("mode-pending-banner");
  await expect(banner).toContainText(`The latest access-rule change is not active yet: ${error}`);
  await snap(page, "gate-rejected");
  await banner.getByRole("link", { name: "Open the Models page" }).click();
  await expect(page).toHaveURL(/\/models$/);
  // Nothing to link to for an unnamed rejection: the text alone.
  await page.unrouteAll();
  await setup(page, { config: { ...enforce, pending: { ...pending, objects: undefined, error: "a gateway pod rejected the update (AgentGatewayNackError): bad. agentgateway does not say which object; check the provider secret of each Model." } } });
  await page.goto("/access-rules");
  await expect(page.getByTestId("mode-pending-banner")).toContainText("does not say which object");
  await expect(page.getByTestId("mode-pending-banner").getByRole("link")).toHaveCount(0);
});

test("the list tells a real refusal, a verdict and a missing mark apart", async ({ page }) => {
  await setup(page, { config: audit });
  await search(page);
  const row = (re: RegExp) => page.getByRole("row", { name: re });
  await expect(row(/tools\/call · create_issue/)).toContainText("Would be refused (200)");
  await expect(row(/tools\/call · create_issue/)).toContainText("refuse:tool");
  await expect(row(/09:58:10.*gpt-4o/)).toContainText("Would be refused (200)");
  await expect(row(/auto → mock\/echo/)).toContainText("Would use another model (unless a later route matched)");
  await expect(row(/authorization failed/)).toContainText("Refused (403)");
  await expect(row(/authorization failed/)).not.toContainText("Would be refused");
  const missing = page.getByRole("row", { name: /Mark missing/ });
  await expect(missing).toHaveCount(1);
  await expect(missing).toContainText("The rule expression failed, or the audit fields were not active yet.");
  await expect(missing).not.toContainText("Would be refused");
  await snap(page, "audit-list");
});

test("the filter keeps only the calls the rules would refuse", async ({ page }) => {
  await setup(page, { config: audit });
  await search(page);
  await page.getByLabel("Only calls the rules would refuse").check();
  await expect(page.getByText("Would be refused (200)")).toHaveCount(2);
  await expect(page.getByText("Mark missing")).toHaveCount(0);
  await expect(page.getByText("Refused (403)")).toHaveCount(0);
  await expect(page.getByText("Would use another model")).toHaveCount(0);
  await page.getByLabel("Only calls the rules would refuse").uncheck();
  await expect(page.getByText("Mark missing")).toHaveCount(1);
});

test("CSV has the outcome, the mark and the rules version", async ({ page }) => {
  await setup(page, { config: audit });
  await search(page);
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Export CSV" }).click()]);
  const lines = readFileSync(await download.path(), "utf8").trim().split("\n");
  expect(lines[0]).toBe("time_utc,email,what,server_or_model,outcome,status,reason,input_tokens,output_tokens,mark,rules_version");
  expect(lines[1]).toContain('"Would be refused","200"');
  expect(lines[1]).toContain('"refuse:model","3f9a1c2b7d10"');
  expect(lines.find((l) => l.includes("authorization failed"))).toContain('"Refused","403"');
  expect(lines.find((l) => l.includes('"Mark missing"'))).toContain('"Mark missing","200"');
});

test("the summary shows its window, the newest record, the request-time wording and the two side lines", async ({ page }) => {
  const seen: string[] = [];
  await setup(page, { config: audit, seen });
  await page.goto("/audit");
  await expect(page.getByTestId("would-refuse-headline")).toHaveText("Requests from 2026-10-08 10:00 to 2026-10-15 10:01 (UTC): 1,240 requests from 18 people would have been refused.");
  const box = page.getByTestId("would-refuse-summary");
  await expect(page.getByTestId("would-refuse-newest")).toHaveText("Newest record: 2026-10-15 09:58.");
  await expect(box).toContainText("Each mark was computed when the request was served, with the rules in force at that time.");
  await expect(box).toContainText("Only since the current rules (version 3f9a1c2b7d10, since 2026-10-14 16:20)");
  await expect(box.getByRole("row", { name: "data-science gpt-4o 812 9" })).toBeVisible();
  await expect(box.getByRole("row", { name: "(no group) github · create_issue 40 3" })).toBeVisible();
  await expect(box.getByRole("row", { name: /mock\/echo/ })).toHaveCount(0); // another model is not "would be refused"
  await expect(box).toContainText("A person in two groups counts in both.");
  await expect(page.getByTestId("mark-missing-line")).toHaveText("Call records with a missing or failed mark: 0 (not counted above).");
  await expect(page.getByTestId("refused-line")).toHaveText("Refused for real (sign-in, budget, unpriced model): 12");
  await expect(page.getByTestId("stale-warning")).toHaveCount(0);
  await expect(page.getByTestId("switch-check")).toHaveCount(0); // all rules, not the current ones
  expect(seen[0]).toContain("rules=all");
  await snap(page, "audit-summary");
});

test("'only since the current rules' asks for the current version and says what the count means", async ({ page }) => {
  const seen: string[] = [];
  await setup(page, { config: audit, seen });
  await page.goto("/audit");
  await expect(page.getByTestId("would-refuse-headline")).toBeVisible();
  await page.route("**/api/audit/would-refuse*", (r) => { seen.push(new URL(r.request().url()).search); return r.fulfill({ contentType: "application/json", body: JSON.stringify({ ...summary, rules: "current", rulesVersion: "3f9a1c2b7d10", requests: 0, people: 0, groups: [] }) }); });
  await page.getByLabel(/Only since the current rules/).check();
  await expect(page.getByTestId("would-refuse-headline")).toContainText("0 requests from 0 people would have been refused.");
  expect(seen.at(-1)).toContain("rules=current");
  // The rules are 0 days old: nothing seen yet means little.
  await expect(page.getByTestId("switch-check")).toHaveText("The current rules have been in force for less than 1 day. Wait until they have been in force for 7 days before relying on this count.");
  await snap(page, "audit-summary-current-rules");
});

test("after 7 days of the current rules with nothing to count, the summary says switching would refuse nothing", async ({ page }) => {
  await setup(page, { config: { ...audit, rulesSince: "2026-10-07T08:00:00Z" } });
  await page.route("**/api/audit/would-refuse*", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify({ ...summary, rules: new URL(r.request().url()).searchParams.get("rules"), requests: 0, people: 0, groups: [] }) }));
  await page.goto("/audit");
  await page.getByLabel(/Only since the current rules/).check();
  await expect(page.getByTestId("switch-check")).toHaveText("Switching to enforcing now would refuse nothing seen since 2026-10-07 08:00 UTC.");
  // A missing mark is not "nothing".
  await page.route("**/api/audit/would-refuse*", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify({ ...summary, rules: "current", requests: 0, people: 0, markMissing: 3, groups: [] }) }));
  await page.getByLabel(/Only since the current rules/).uncheck();
  await page.getByLabel(/Only since the current rules/).check();
  await expect(page.getByTestId("switch-check")).toHaveCount(0);
});

test("missing marks are a warning with the next step", async ({ page }) => {
  await setup(page, { config: audit, summary: { ...summary, markMissing: 4 } });
  await page.goto("/audit");
  const line = page.getByTestId("mark-missing-line");
  await expect(line).toContainText("Call records with a missing or failed mark: 4 (not counted above).");
  await expect(line).toContainText("run `ambit check --production` and report the rule");
  await snap(page, "audit-summary-mark-missing");
});

test("a stale pipeline is a warning when the newest record is older than one hour", async ({ page }) => {
  await setup(page, { config: audit, summary: { ...summary, newest: "2026-10-15T08:30:00Z" } });
  await page.goto("/audit");
  await expect(page.getByTestId("stale-warning")).toContainText("No record in the last hour.");
  await expect(page.getByTestId("stale-warning")).toContainText("ambit check --production --deep");
  await snap(page, "audit-summary-stale");
});

test("after the switch to enforce, the summary stays for 30 days of audit-mode records, then goes", async ({ page }) => {
  await setup(page, { config: enforce, summary: { ...summary, newest: "2026-10-14T12:00:00Z" } });
  await page.goto("/audit");
  await expect(page.getByTestId("would-refuse-summary")).toBeVisible();
  await expect(page.getByTestId("stale-warning")).toHaveCount(0); // the pipeline warning is for audit-only mode
  await page.unrouteAll();
  await setup(page, { config: enforce, summary: { ...summary, newest: "2026-08-01T12:00:00Z" } });
  await page.goto("/audit");
  await expect(page.getByRole("heading", { name: "Audit" })).toBeVisible();
  await expect(page.getByTestId("would-refuse-summary")).toHaveCount(0);
  await setup(page, { config: enforce, summary: { ...summary, newest: null, requests: 0, groups: [] } });
});

test("in enforce mode the Audit page is as before", async ({ page }) => {
  const plain = [call("2026-10-15T09:00:00Z", { accessMode: "enforce", mark: "", outcome: "n/a" }), call("2026-10-15T08:00:00Z", { status: "403", accessMode: "enforce", mark: "", outcome: "n/a", error: "authorization failed" })];
  await setup(page, { config: enforce, calls: plain, summary: { ...summary, newest: null, requests: 0, groups: [] } });
  await page.goto("/audit");
  await page.locator("#audit-email").fill("alice@acme.test");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText("OK (200)")).toBeVisible();
  await expect(page.getByText("Refused by policy (403)")).toBeVisible();
  await expect(page.getByLabel("Only calls the rules would refuse")).toHaveCount(0);
});

test("the pages say what happens to what no rule names, by mode", async ({ page }) => {
  const pages: Array<[string, RegExp, RegExp]> = [
    ["/access-rules", /Audit-only mode: anything no rule names is let through and marked as would be refused/, /Anything no rule names is closed to everyone/],
    ["/models", /In audit-only mode a model that no access rule names is let through/, /A model is closed to everyone until an access rule names it/],
    ["/servers", /In audit-only mode their tools are let through/, /Their tools are closed to everyone until an access rule names them/],
    ["/", /named by no rule \(let through\)/, /closed to everyone/],
  ];
  const homeMocks = async () => { // the overview needs a person's access and one model that no rule names
    await page.route("**/api/me/access", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify({ groups: [], models: [], servers: [] }) }));
    await page.route("**/api/models", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify(r.request().headers()["referer"]?.endsWith("/") ? [{ name: "gpt-4o", allSignedIn: false, allowedGroups: [], allowedUsers: [] }] : []) }));
  };
  await setup(page, { config: audit });
  await homeMocks();
  for (const [path, inAudit] of pages) {
    await page.goto(path);
    await expect(page.locator("main")).toContainText(inAudit);
    await expect(page.locator("main")).not.toContainText(/closed to everyone|no model can be used/);
    await snap(page, `no-rule-wording-audit-${path === "/" ? "home" : path.slice(1)}`);
  }
  await page.unrouteAll();
  await setup(page, { config: enforce });
  await homeMocks();
  for (const [path, , inEnforce] of pages) {
    await page.goto(path);
    await expect(page.locator("main")).toContainText(inEnforce);
  }
});
