import { expect, test, type Page, type Route } from "@playwright/test";

// The Agents page and the Audit filters for agents (docs/agents.md, experimental). Every API answer is a mock
// (page.route) and the clock is fixed, so this needs no data: it runs against `npm run dev` with
// AMBIT_URL=http://localhost:5173 (use a dev server whose /api proxy does not point at a real console: every /api call
// is answered here, so none is proxied). Against a console (make test-ui) it first signs in as the bootstrap admin
// (AMBIT_ADMIN_PASSWORD). AMBIT_SCREENSHOTS_DIR writes the screens for docs/agents.md.
const password = process.env.AMBIT_ADMIN_PASSWORD ?? "";
const NOW = new Date("2026-11-25T10:00:00Z");

type Agent = { stopRequestedAt?: string; stoppedAt?: string; stopUnlisted?: boolean; stopNotConfirmedAt?: string; orphanedAt?: string; name: string; displayName: string; owner: string; mode: "person" | "itself"; group?: string; identity?: string; platform: string;
  scope: string[]; connectionConsent: string[]; state: string; rev: number; created: string; createdBy: string; manageable: boolean };
type Key = { id: string; name: string; prefix: string; scope: "mcp"; created: string; expires: string; until: string; lastUsed: string | null; status: string; stopsBecause: string;
  agent: string; sourceRanges?: string[] };

const agent = (o: Partial<Agent> & { name: string }): Agent => ({
  displayName: o.name, owner: "user09@example.com", mode: "person", platform: "grok", scope: [], connectionConsent: [], state: "active", rev: 1,
  created: "2026-11-20T09:00:00Z", createdBy: "user09@example.com", manageable: true, ...o });
const key = (o: Partial<Key> & { id: string; agent: string }): Key => ({
  name: "connector", prefix: "ambit_AbCdE", scope: "mcp", created: "2026-11-20T09:00:00Z", expires: "2027-02-18T09:00:00Z", until: "2027-02-18T09:00:00Z",
  lastUsed: null, status: "active", stopsBecause: "end-date", ...o });

const servers = [
  { name: "crm", displayName: "CRM", image: "", url: "", connection: "", transport: "streamable-http", ready: "True", message: "", tools: [{ name: "read_contact" }, { name: "update_contact" }], rules: [], changedTools: [], toolPinningOff: false },
  { name: "wiki", displayName: "Wiki", image: "", url: "", connection: "", transport: "streamable-http", ready: "True", message: "", tools: [{ name: "search" }], rules: [], changedTools: [], toolPinningOff: false },
];
const call = (time: string, extra: Record<string, unknown> = {}) => ({
  time, email: "user09@example.com", route: "ambit/mcp", method: "tools/call", server: "crm", tool: "read_contact", model: "", status: "200", error: "",
  inputTokens: 0, outputTokens: 0, router: "", label: "", labelReason: "", ...extra });
const agentCall = (time: string, extra: Record<string, unknown> = {}) => call(time, {
  agent: "grok-bot-x7k2pq", agentMode: "person", agentOwnerAtIssue: "user09@example.com", agentOwnerNow: "user09@example.com", platformDeclared: "grok", ingress: "private", ...extra });

type Setup = { confirmAfter?: number; startError?: string; connections?: object[]; saMaxDays?: number; keyDelayMs?: number; maxDays?: number; role?: "admin" | "viewer"; agents?: Agent[]; keys?: Key[]; enabled?: boolean; lastUsedAvailable?: boolean; calls?: object[]; auditError?: string;
  seen?: { method: string; path: string; search: string; body?: unknown }[] };

// A small stateful mock of the console: agents, their keys, the audit trail.
async function setup(page: Page, o: Setup = {}) {
  await page.clock.setFixedTime(NOW);
  const role = o.role ?? "viewer";
  const email = role === "admin" ? "user10@example.com" : "user09@example.com";
  const agents = o.agents ?? [];
  const keys = o.keys ?? [];
  let polls = 0;
  const json = (route: Route, body: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
  const settings = { enabled: o.enabled ?? true, maxTotal: 2000, maxPerPerson: 10, keyMaxDays: 90 };
  const keySettings = { enabled: true, inactivityDays: 30, maxDays: o.maxDays ?? 90, lastUsedAvailable: o.lastUsedAvailable ?? true };
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const u = new URL(req.url());
    const p = u.pathname;
    const m = req.method();
    o.seen?.push({ method: m, path: p, search: u.search, body: req.postDataJSON() });
    if (p === "/api/me") return json(route, { email, name: email.split("@")[0], role, groups: ["eng"], bootstrap: false });
    if (p === "/api/config") return json(route, { mode: "direct", accessMode: "enforce" });
    if (p === "/api/settings/keys") return json(route, keySettings);
    if (p === "/api/admin/service-accounts") return json(route, { settings: { enabled: true, maxDays: o.saMaxDays ?? 90 }, accounts: [], urls: {} });
    if (p === "/api/connections") return json(route, o.connections ?? [
      { name: "slack", displayName: "Slack", state: "connected", revocable: true, connectURL: "/auth/connect/slack", servers: ["chat"] },
      { name: "jira", displayName: "Jira", state: "not-connected", revocable: false, connectURL: "/auth/connect/jira", servers: ["tickets"] }]);
    if (p === "/api/servers") return json(route, servers);
    if (p === "/api/groups") return json(route, [{ name: "ops", source: "rules" }]);
    if (p === "/api/keys" || p === "/api/admin/keys") return json(route, { settings: keySettings, keys, urls: { llm: "https://llm.example.com", mcp: "https://mcp.example.com/mcp" } });
    if (p === "/api/agents" && m === "GET") return json(route, { agents: role === "admin" ? agents : agents.filter((a) => a.owner === email), settings });
    if (p === "/api/agents" && m === "POST") {
      const b = req.postDataJSON();
      const a = agent({ name: b.mode === "itself" ? b.name : "grok-bot-x7k2pq", displayName: b.displayName, platform: b.platform, scope: b.scope, mode: b.mode ?? "person",
        owner: b.owner || email, group: b.group, identity: b.mode === "itself" ? `${b.name}@svc.ambit.internal` : undefined, createdBy: email });
      agents.push(a);
      return json(route, a, 201);
    }
    const one = p.match(/^\/api\/agents\/([^/]+)$/);
    if (one) {
      const a = agents.find((x) => x.name === one[1]);
      if (!a) return json(route, { error: "no such agent" }, 404);
      if (m === "GET") {
        // a stop is confirmed after a few polls: the console reports Stopped only when every gateway confirmed
        if (a.state === "stopping" && !a.stopNotConfirmedAt && ++polls >= (o.confirmAfter ?? 2)) { a.state = "stopped"; a.stoppedAt = "2026-11-25T10:00:08Z"; }
        return json(route, { ...a, keys: keys.filter((k) => k.agent === a.name) });
      }
      if (m === "PATCH") { Object.assign(a, req.postDataJSON()); return json(route, a); }
      if (m === "DELETE") { agents.splice(agents.indexOf(a), 1); return route.fulfill({ status: 204 }); }
    }
    const own = p.match(/^\/api\/agents\/([^/]+)\/owner$/);
    if (own && m === "PUT") { const a = agents.find((x) => x.name === own[1])!; a.owner = req.postDataJSON().owner; delete a.orphanedAt; return json(route, a); }
    const act = p.match(/^\/api\/agents\/([^/]+)\/(stop|start)$/);
    if (act && m === "POST") {
      const a = agents.find((x) => x.name === act[1]);
      if (!a) return json(route, { error: "no such agent" }, 404);
      if (act[2] === "stop") { a.state = "stopping"; a.stopRequestedAt = "2026-11-25T10:00:00Z"; polls = 0; keys.splice(0, keys.length, ...keys.filter((k) => k.agent !== a.name)); return json(route, a, 202); }
      if (o.startError) return json(route, { error: o.startError }, 409);
      a.state = "active"; delete a.stoppedAt; delete a.stopRequestedAt; return json(route, a);
    }
    const newKey = p.match(/^\/api\/agents\/([^/]+)\/keys$/);
    if (newKey && m === "POST") {
      const b = req.postDataJSON();
      if (o.keyDelayMs) await new Promise((r) => setTimeout(r, o.keyDelayMs));
      const k = key({ id: `k${keys.length + 1}`, agent: newKey[1], name: b.name, sourceRanges: b.sourceRanges?.length ? b.sourceRanges : undefined });
      keys.push(k);
      return json(route, { key: "ambit_SECRETSECRETSECRETSECRETSECRETSECRETSECRET1", view: k, urls: { mcp: "https://mcp.example.com/mcp" } }, 201);
    }
    const revoke = p.match(/^\/api\/(?:admin\/)?keys\/([^/]+)$/);
    if (revoke && m === "DELETE") { keys.splice(keys.findIndex((k) => k.id === revoke[1]), 1); return route.fulfill({ status: 204 }); }
    if (p === "/api/audit") {
      if (o.auditError) return json(route, { error: o.auditError }, 502);
      const q = u.searchParams;
      const hit = (c: Record<string, unknown>) => (!q.get("email") || c.email === q.get("email")) && (!q.get("agent") || c.agent === q.get("agent"))
        && (!q.get("platform_declared") || c.platformDeclared === q.get("platform_declared")) && (!q.get("ingress") || c.ingress === q.get("ingress"));
      return json(route, { calls: (o.calls ?? []).filter((c) => hit(c as Record<string, unknown>)), limited: false });
    }
    return json(route, []);
  });
}

async function snap(page: Page, name: string) {
  const dir = process.env.AMBIT_SCREENSHOTS_DIR;
  if (!dir) return;
  await page.waitForLoadState("networkidle");
  await page.screenshot({ path: `${dir}/${name}.png`, fullPage: true });
}

test.use({ viewport: { width: 1280, height: 900 }, colorScheme: "light" });

test.beforeEach(async ({ page }) => {
  if (!password) return;
  await page.goto("/");
  await page.getByRole("link", { name: "Sign in with bootstrap admin" }).click();
  await page.getByLabel("Email Address").fill("admin@ambit.local");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Login" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText("Signed in as admin (bootstrap)");
});

const lastAudit = (seen: NonNullable<Setup["seen"]>) => seen.filter((s) => s.path === "/api/audit").slice(-1)[0];
const sent = async (seen: NonNullable<Setup["seen"]>, method: string, path: string) => {
  const find = () => seen.find((s) => s.method === method && s.path === path)?.body;
  await expect.poll(find).toBeTruthy();
  return find();
};
const nav = (page: Page) => page.getByRole("navigation", { name: "Console" });

test("empty states: no agents, agents switched off", async ({ page }) => {
  await setup(page);
  await page.goto("/agents");
  await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
  await expect(page.getByTestId("agents-empty")).toContainText("You have no agents yet.");
  await expect(page.getByRole("button", { name: "New agent" })).toBeVisible();
  await expect(page.getByTestId("agents-table")).toHaveCount(0);
  await snap(page, "agents-empty");

  await page.unrouteAll();
  await setup(page, { enabled: false });
  await page.goto("/agents");
  await expect(page.getByText("Agents are switched off by your administrator")).toBeVisible();
  await expect(page.getByRole("button", { name: "New agent" })).toHaveCount(0);
  await expect(page.getByTestId("agents-empty")).toHaveCount(0);
});

test("list: owner, declared platform, kind, last used, tools, state; no audit trail says unknown", async ({ page }) => {
  const agents = [
    agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales", scope: ["crm/read_contact", "wiki"] }),
    agent({ name: "claude-shared-q1w2e3", displayName: "Claude for the team", platform: "claude-ai", owner: "user09@example.com", scope: [] }),
    agent({ name: "nightly-report", displayName: "Nightly report", mode: "itself", owner: "group:platform", group: "ops", identity: "nightly-report@svc.ambit.internal", platform: "my-bot", state: "pending" }),
  ];
  const keys = [key({ id: "k1", agent: "grok-bot-x7k2pq", lastUsed: "2026-11-24T08:30:00Z" }), key({ id: "k2", agent: "grok-bot-x7k2pq", lastUsed: "2026-11-21T08:30:00Z" })];
  await setup(page, { role: "admin", agents, keys });
  await page.goto("/agents");
  const row = (n: string) => page.getByTestId(`agent-row-${n}`);
  await expect(row("grok-bot-x7k2pq")).toContainText("Grok Bot for sales");
  await expect(row("grok-bot-x7k2pq")).toContainText("user09@example.com");
  await expect(row("grok-bot-x7k2pq")).toContainText("Grok Bot"); // the declared platform, as a name
  await expect(row("grok-bot-x7k2pq")).toContainText("Person");
  await expect(row("grok-bot-x7k2pq")).toContainText("2026-11-24"); // the newest of its two keys
  await expect(row("grok-bot-x7k2pq")).toContainText("1 server, 1 tool"); // wiki is one server, crm/read_contact one tool
  await expect(row("claude-shared-q1w2e3")).toContainText("Claude.ai");
  await expect(row("claude-shared-q1w2e3")).toContainText("not yet");
  await expect(row("claude-shared-q1w2e3")).toContainText("none");
  await expect(row("nightly-report")).toContainText("group:platform");
  await expect(row("nightly-report")).toContainText("my-bot"); // an unknown platform is shown as given
  await expect(row("nightly-report")).toContainText("Itself");
  await expect(row("nightly-report")).toContainText("Being set up");
  await expect(page.getByRole("columnheader", { name: "Declared platform" })).toHaveAttribute("title", /Ambit does not check it/);
  await snap(page, "agents-list");

  await page.unrouteAll();
  await setup(page, { role: "admin", agents, keys: [], lastUsedAvailable: false });
  await page.goto("/agents");
  await expect(row("grok-bot-x7k2pq")).toContainText("unknown");
  await expect(page.getByTestId("no-audit-trail-note")).toContainText("no audit trail");
});

test("a person sees only their own agents and no itself-mode choice", async ({ page }) => {
  await setup(page, { agents: [agent({ name: "mine-aaaaaa", displayName: "Mine" }), agent({ name: "theirs-bbbbbb", displayName: "Theirs", owner: "user02@example.com" })] });
  await page.goto("/agents");
  await expect(page.getByTestId("agent-row-mine-aaaaaa")).toBeVisible();
  await expect(page.getByTestId("agent-row-theirs-bbbbbb")).toHaveCount(0);
  await page.getByRole("button", { name: "New agent" }).click();
  const form = page.getByRole("form", { name: "New agent" });
  await expect(form.getByRole("group", { name: "Kind of agent" })).toHaveCount(0);
  await expect(form.getByLabel("Account name")).toHaveCount(0);
});

test("create an agent, make a key: the key is shown once", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { seen });
  await page.goto("/agents");
  await nav(page).getByRole("link", { name: "Agents" }).click();
  await expect(page).toHaveURL(/\/agents$/);
  await page.getByRole("button", { name: "New agent" }).click();
  const form = page.getByRole("form", { name: "New agent" });
  await form.getByLabel("Name", { exact: true }).fill("Grok Bot for sales");
  await form.getByLabel("Declared platform").selectOption("grok");
  await expect(form).toContainText("Ambit does not check it");
  await form.getByLabel("All tools of crm").check();
  await form.getByLabel("All tools of wiki").check();
  await snap(page, "agents-new");
  await form.getByRole("button", { name: "Create agent" }).click();

  expect(await sent(seen, "POST", "/api/agents")).toEqual({ displayName: "Grok Bot for sales", platform: "grok", scope: ["crm", "wiki"] });
  const detail = page.getByTestId("agent-detail");
  await expect(page).toHaveURL(/\/agents\?agent=grok-bot-x7k2pq$/);
  await expect(detail).toContainText("Grok Bot for sales");
  await expect(page.getByTestId("agent-declared-platform")).toHaveText("Grok Bot (declared, not proved)");
  await expect(page.getByTestId("agent-scope")).toContainText("crm (all tools)");
  await expect(detail).toContainText("No keys.");

  await detail.getByRole("button", { name: "New key" }).click();
  const keyForm = detail.getByRole("form", { name: "New key for the agent" });
  await keyForm.getByLabel("Key name").fill("grok connector");
  await keyForm.getByLabel("End date").selectOption("30");
  await keyForm.getByLabel("Source address ranges (optional)").fill("203.0.113.0/24\n2001:db8::/32");
  await keyForm.getByRole("button", { name: "Create key" }).click();
  expect(await sent(seen, "POST", "/api/agents/grok-bot-x7k2pq/keys")).toEqual({ name: "grok connector", days: 30, sourceRanges: ["203.0.113.0/24", "2001:db8::/32"] });
  const shown = page.getByTestId("agent-key-created");
  await expect(shown).toContainText("This is the only time the key is shown");
  const plain = (await page.getByTestId("new-key").textContent())!;
  expect(plain).toMatch(/^ambit_[0-9A-Za-z]{43}$/);
  await expect(page.getByTestId("agent-mcp-url")).toHaveText("https://mcp.example.com/mcp");
  await expect(page.getByTestId("agent-header")).toContainText(`Authorization: Bearer ${plain}`);
  await expect(page.getByTestId("agent-header")).toContainText(`x-api-key: ${plain}`);
  await snap(page, "agents-key-shown");
  await shown.getByRole("button", { name: "Done" }).click();

  // gone from the page for good: the list shows the start of the key only
  await expect(page.getByTestId("new-key")).toHaveCount(0);
  expect(await page.content()).not.toContain(plain);
  const row = detail.getByRole("row", { name: /grok connector/ });
  await expect(row).toContainText("ambit_AbCdE");
  await expect(row).toContainText("203.0.113.0/24, 2001:db8::/32");

  // revoke, then delete the agent
  await row.getByRole("button", { name: "Revoke" }).click();
  await row.getByRole("button", { name: "Revoke key" }).click();
  await expect(detail.getByRole("row", { name: /grok connector/ })).toHaveCount(0);
  await detail.getByRole("button", { name: "Delete agent" }).click();
  await page.getByRole("alertdialog", { name: "Delete agent" }).getByRole("button", { name: "Delete agent" }).click();
  await expect(page.getByTestId("agent-detail")).toHaveCount(0);
  await expect(page.getByTestId("agents-empty")).toBeVisible();
});

test("a bad platform name disables Create; a console error is shown", async ({ page }) => {
  await setup(page);
  await page.goto("/agents");
  await page.getByRole("button", { name: "New agent" }).click();
  const form = page.getByRole("form", { name: "New agent" });
  await form.getByLabel("Name", { exact: true }).fill("Odd bot");
  await form.getByLabel("Declared platform").selectOption("__custom");
  await form.getByLabel("Platform name").fill("My Bot");
  await expect(form.getByRole("alert")).toContainText("lower-case letters");
  await expect(form.getByRole("button", { name: "Create agent" })).toBeDisabled();
  await form.getByLabel("Platform name").fill("my-bot");
  await expect(form.getByRole("button", { name: "Create agent" })).toBeEnabled();
  // the limit of the console: 409
  await page.route("**/api/agents", (r) => r.request().method() === "POST" ? r.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "agent limit reached" }) }) : r.fallback());
  await form.getByRole("button", { name: "Create agent" }).click();
  await expect(form.getByRole("alert")).toContainText("agent limit reached");
});

test("admins make an itself-mode agent with an account name, group and owner", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { role: "admin", seen });
  await page.goto("/agents");
  await expect(page.getByTestId("agents-empty")).toContainText("There are no agents yet.");
  await page.getByRole("button", { name: "New agent" }).click();
  const form = page.getByRole("form", { name: "New agent" });
  await form.getByLabel("Itself.", { exact: false }).check();
  await form.getByLabel("Name", { exact: true }).fill("Nightly report");
  await form.getByLabel("Declared platform").selectOption("agentcore");
  await form.getByLabel("Account name").fill("nightly-report");
  await expect(form).toContainText("nightly-report@svc.ambit.internal");
  await form.getByLabel("Group").fill("ops");
  await form.getByLabel("Owner (optional)").fill("group:platform");
  await form.getByLabel("All tools of wiki").check();
  await form.getByRole("button", { name: "Create agent" }).click();
  expect(await sent(seen, "POST", "/api/agents")).toEqual({
    mode: "itself", name: "nightly-report", displayName: "Nightly report", platform: "agentcore", scope: ["wiki"], group: "ops", owner: "group:platform" });
  await expect(page.getByTestId("agent-calls-as")).toHaveText("nightly-report@svc.ambit.internal");
  await expect(page.getByTestId("agent-kind")).toContainText("Itself (service account)");
});

test("change the scope of an agent; admins see the tools it used", async ({ page }) => {
  const seen: Setup["seen"] = [];
  const a = agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales", scope: ["crm/read_contact"], owner: "user10@example.com" }); // the admin's own agent: only an owner may change it
  await setup(page, { role: "admin", agents: [a], seen, calls: [agentCall("2026-11-24T08:30:00Z"), agentCall("2026-11-24T08:31:00Z"), agentCall("2026-11-24T08:32:00Z", { server: "wiki", tool: "search" }), agentCall("2026-11-24T08:29:00Z", { method: "tools/list", server: "", tool: "" })] });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  const detail = page.getByTestId("agent-detail");
  await expect(detail.getByTestId("tools-used")).toContainText("crm · read_contact × 2");
  await expect(detail.getByTestId("tools-used")).toContainText("wiki · search × 1");
  expect(seen.find((s) => s.path === "/api/audit")?.search).toContain("agent=grok-bot-x7k2pq");
  await detail.getByRole("link", { name: "Search Audit for this agent" }).click();
  await expect(page).toHaveURL(/\/audit\?agent=grok-bot-x7k2pq$/);
  await page.goBack();

  await detail.getByRole("button", { name: "Change" }).click();
  const form = page.getByRole("form", { name: "Change the agent" });
  await form.getByLabel("All tools of wiki").check();
  await form.getByRole("button", { name: "Save" }).click();
  expect(await sent(seen, "PATCH", "/api/agents/grok-bot-x7k2pq")).toEqual({ displayName: "Grok Bot for sales", platform: "grok", scope: ["crm/read_contact", "wiki"] });
  await expect(page.getByTestId("agent-scope")).toContainText("wiki (all tools)");
});

test("detail: a person is told that admins see the tools used; no audit trail says not known", async ({ page }) => {
  await setup(page, { agents: [agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales" })] });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByTestId("agent-detail")).toContainText("Admins can see which tools this agent used in Audit.");
  await expect(page.getByTestId("agent-scope")).toContainText("None. With an empty scope the agent can use no tool.");

  await page.unrouteAll();
  await setup(page, { role: "admin", agents: [agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales" })], auditError: "the audit store (ClickHouse) did not answer" });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByTestId("no-audit-trail")).toContainText("Not known: the audit trail did not answer");
});

test("an unknown agent in the address says so", async ({ page }) => {
  await setup(page);
  await page.goto("/agents?agent=nobody-zzzzzz");
  await expect(page.getByText("This agent does not exist, or it is not yours.")).toBeVisible();
});

test("Audit filters: agent, declared platform and entrance; the agent badge; owner moved; CSV columns", async ({ page }) => {
  const seen: Setup["seen"] = [];
  const calls = [
    agentCall("2026-11-25T09:00:00Z", { ingress: "public", agentOwnerNow: "user02@example.com" }),
    agentCall("2026-11-25T08:00:00Z", { server: "wiki", tool: "search", agentMode: "itself", agent: "nightly-report", email: "nightly-report@svc.ambit.internal", platformDeclared: "agentcore", agentOwnerAtIssue: "group:platform", agentOwnerNow: "group:platform" }),
    call("2026-11-25T07:00:00Z", { server: "wiki", tool: "search" }),
  ];
  await setup(page, { role: "admin", seen, calls });
  const params = () => new URLSearchParams(lastAudit(seen).search);
  const search = () => page.getByRole("button", { name: "Search" }).click();
  await page.goto("/audit");
  await expect(page.locator("#audit-ingress")).toBeDisabled(); // it only narrows another filter

  // one person: their own call and the call their agent made
  await page.locator("#audit-email").fill("user09@example.com");
  await search();
  const first = page.getByRole("row", { name: /tools\/call · read_contact/ });
  await expect(first.getByTestId("agent-badge")).toHaveText("agent");
  await expect(first.getByTestId("ingress-badge")).toHaveText("public");
  await expect(first.getByTestId("agent-detail")).toContainText("grok-bot-x7k2pq (person mode)");
  await expect(first.getByTestId("agent-detail")).toContainText("declared platform: Grok Bot");
  await expect(first.getByTestId("agent-detail")).toContainText("owner at issue: user09@example.com · owner now: user02@example.com");
  const plainRow = page.getByRole("row", { name: /tools\/call · search/ });
  await expect(plainRow).toHaveCount(1);
  await expect(plainRow.getByTestId("agent-badge")).toHaveCount(0);
  await snap(page, "audit-agent");

  // one agent, without an email: an itself-mode agent; the owner is shown once when it did not change
  await page.locator("#audit-email").fill("");
  await expect(page.locator("#audit-email")).toHaveAttribute("required", "");
  await page.locator("#audit-agent").fill("nightly-report");
  await expect(page.locator("#audit-email")).not.toHaveAttribute("required");
  await search();
  const second = page.getByRole("row", { name: /tools\/call · search/ });
  await expect(second).toHaveCount(1);
  await expect(second.getByTestId("agent-detail")).toContainText("nightly-report (itself mode)");
  await expect(second.getByTestId("agent-detail")).toContainText("declared platform: AWS AgentCore");
  await expect(second.getByTestId("agent-detail")).not.toContainText("owner now");
  await expect(second.getByTestId("ingress-badge")).toHaveCount(0);
  expect(params().get("email")).toBeNull();

  // a declared platform and the public entrance: the filters are ANDed by the console
  await page.locator("#audit-agent").fill("");
  await page.locator("#audit-platform").fill("grok");
  await expect(page.locator("#audit-ingress")).toBeEnabled();
  await page.locator("#audit-ingress").selectOption("public");
  await search();
  await expect(page.getByRole("row", { name: /tools\/call · read_contact/ })).toBeVisible();
  await expect(page.getByRole("row", { name: /tools\/call/ })).toHaveCount(1);
  expect([params().get("platform_declared"), params().get("ingress"), params().get("agent"), params().get("email")]).toEqual(["grok", "public", null, null]);
  await page.locator("#audit-ingress").selectOption("private");
  await search();
  await expect(page.getByText("No calls with the declared platform grok, through the private entrance in this period.")).toBeVisible();

  await page.locator("#audit-ingress").selectOption("public");
  await search();
  await expect(page.getByRole("row", { name: /tools\/call · read_contact/ })).toBeVisible();
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Export CSV" }).click()]);
  expect(download.suggestedFilename()).toMatch(/^audit-grok-/);
  const text = (await (await import("node:fs/promises")).readFile((await download.path())!, "utf8"));
  const [head, row1] = text.split("\n");
  expect(head.endsWith(",agent,agent_mode,agent_owner_at_issue,agent_owner_now,platform_declared,grant_id,token_issuer,ingress")).toBe(true);
  expect(row1.endsWith('"grok-bot-x7k2pq","person","user09@example.com","user02@example.com","grok","","","public"')).toBe(true);
});

test("Audit: switching to another search sends only what applies to it", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { role: "admin", seen, calls: [] });
  await page.goto("/audit");
  await page.locator("#audit-agent").fill("grok-bot-x7k2pq");
  await page.locator("#audit-ingress").selectOption("public");
  await page.locator("#audit-mode").selectOption("invalid");
  await page.getByRole("button", { name: "Search" }).click();
  const q = new URLSearchParams(lastAudit(seen).search);
  expect([q.get("invalidKey"), q.get("agent"), q.get("ingress"), q.get("platform_declared")]).toEqual(["true", null, null, null]);
});

test("Audit: a bad value in the link is ignored, not sent", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { role: "admin", seen, calls: [] });
  await page.goto("/audit?agent=Bad%20Name&platform=grok");
  await expect(page.locator("#audit-agent")).toHaveValue("");
  await expect(page.locator("#audit-platform")).toHaveValue("grok");
  const q = new URLSearchParams(lastAudit(seen).search);
  expect([q.get("agent"), q.get("platform_declared")]).toEqual([null, "grok"]);
});

test("Agents: a key being made cannot be closed; the list of end dates follows the key limit; a stopped agent cannot change or get keys", async ({ page }) => {
  const agents = [agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales", owner: "user10@example.com" }),
    agent({ name: "old-bot-aaaaaa", displayName: "Old bot", owner: "user10@example.com", state: "stopped" })];
  await setup(page, { role: "admin", agents, keyDelayMs: 1500, maxDays: 30 });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  const detail = page.getByTestId("agent-detail");
  await detail.getByRole("button", { name: "New key" }).click();
  const form = detail.getByRole("form", { name: "New key for the agent" });
  await expect(form.getByLabel("End date").locator("option")).toHaveText(["30 days from now"]); // apiKeys.maxDays is 30
  await form.getByLabel("Key name").fill("slow");
  await form.getByRole("button", { name: "Create key" }).click();
  await expect(form.getByRole("button", { name: "Cancel" })).toBeDisabled();
  await expect(detail.getByRole("button", { name: "Close" })).toBeDisabled();
  await expect(page.getByTestId("new-key")).toBeVisible();
  await page.getByRole("button", { name: "Done" }).click();
  await expect(detail.getByRole("button", { name: "Close" })).toBeEnabled();

  await page.goto("/agents?agent=old-bot-aaaaaa");
  await expect(page.getByTestId("agent-state")).toHaveText("Stopped");
  await expect(page.getByRole("button", { name: "Change" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New key" })).toHaveCount(0);
});

test("an admin can revoke a key of another person's agent", async ({ page }) => {
  const agents = [agent({ name: "theirs-bbbbbb", displayName: "Theirs", owner: "user02@example.com", manageable: false })];
  await setup(page, { role: "admin", agents, keys: [key({ id: "k9", agent: "theirs-bbbbbb", name: "their key" })] });
  await page.goto("/agents?agent=theirs-bbbbbb");
  const row = page.getByRole("row", { name: /their key/ });
  await row.getByRole("button", { name: "Revoke" }).click();
  await row.getByRole("button", { name: "Revoke key" }).click();
  await expect(page.getByRole("row", { name: /their key/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Change" })).toHaveCount(0); // only the owner changes a person-mode agent
});

test("Audit: a link from the Agents page searches at once; an empty answer names the filters; ingress alone is not sent", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { role: "admin", seen, calls: [] });
  await page.goto("/audit?agent=grok-bot-x7k2pq");
  await expect(page.locator("#audit-agent")).toHaveValue("grok-bot-x7k2pq");
  await expect(page.getByText("No calls of the agent grok-bot-x7k2pq in this period.")).toBeVisible();
  expect(lastAudit(seen).search).toContain("agent=grok-bot-x7k2pq");

  // an email alone still works as before, and the entrance is cleared when no filter is left for it to narrow
  await page.locator("#audit-agent").fill("");
  await page.locator("#audit-email").fill("user01@example.com");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.getByText("No calls by user01@example.com in this period.")).toBeVisible();
  const q = new URLSearchParams(lastAudit(seen).search);
  expect([q.get("email"), q.get("agent"), q.get("ingress")]).toEqual(["user01@example.com", null, null]);
});

test("Audit: without an audit trail the error is shown, not an empty list", async ({ page }) => {
  await setup(page, { role: "admin", auditError: "the audit store (ClickHouse) did not answer, so no results are shown" });
  await page.goto("/audit?agent=grok-bot-x7k2pq");
  await expect(page.getByRole("alert").filter({ hasText: "the audit store (ClickHouse) did not answer" })).toBeVisible();
  await expect(page.getByText("No calls of the agent")).toHaveCount(0);
});

const mine = (o: Partial<Agent> = {}) => agent({ name: "grok-bot-x7k2pq", displayName: "Grok Bot for sales", scope: ["crm"], ...o });

test("stop: confirmed on every gateway before it says Stopped, with the measured time; then start", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { agents: [mine()], keys: [key({ id: "k1", agent: "grok-bot-x7k2pq" })], seen });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  const detail = page.getByTestId("agent-detail");
  await detail.getByRole("button", { name: "Stop agent" }).click();
  const dialog = page.getByRole("alertdialog", { name: "Stop agent" });
  await expect(dialog).toContainText("The page shows Stopped only after every gateway has confirmed the stop.");
  await expect(dialog).toContainText("A call that is already on its way can still be accepted until then.");
  await expect(dialog).not.toContainText(/zero|no call/i);
  await snap(page, "agents-stop-confirm");
  await dialog.getByRole("button", { name: "Stop agent" }).click();

  expect(await sent(seen, "POST", "/api/agents/grok-bot-x7k2pq/stop")).toEqual({});
  const stopping = page.getByTestId("stop-note-info");
  await expect(stopping).toContainText("Stopping since 2026-11-25 10:00:00 UTC");
  await expect(stopping).toContainText("Until then a call can still be accepted.");
  await expect(page.getByTestId("agent-state")).toHaveText("Stopping");
  await expect(detail.getByRole("button", { name: "New key" })).toHaveCount(0);
  await snap(page, "agents-stopping");

  // the page asks again and shows Stopped when the console says so
  await expect(page.getByTestId("agent-state")).toHaveText("Stopped", { timeout: 15_000 });
  await expect(page.getByTestId("stop-note-ok")).toContainText("It was confirmed on every gateway 8 seconds after the request (measured).");
  await expect(page.getByTestId("stop-note-ok")).toContainText("Its keys were deleted and stay deleted.");
  await expect(detail).not.toContainText(/zero calls|no call will/i);
  await expect(detail.getByRole("row", { name: /connector/ })).toHaveCount(0);
  await expect(detail.getByRole("button", { name: "Change" })).toHaveCount(0);
  await expect(detail.getByRole("button", { name: "Stop agent" })).toHaveCount(0);
  await expect(page.getByTestId("agent-row-grok-bot-x7k2pq")).toContainText("Stopped");
  await snap(page, "agents-stopped");

  await detail.getByRole("button", { name: "Start agent" }).click();
  expect(await sent(seen, "POST", "/api/agents/grok-bot-x7k2pq/start")).toEqual({});
  await expect(page.getByTestId("agent-state")).toHaveText("Active");
  await expect(detail.getByRole("button", { name: "Stop agent" })).toBeVisible();
  await expect(detail.getByRole("button", { name: "New key" })).toBeVisible();
});

test("a stop that is not confirmed says so, and a full stop list; the list shows Stopping", async ({ page }) => {
  await setup(page, { agents: [mine({ state: "stopping", stopRequestedAt: "2026-11-25T09:58:00Z", stopNotConfirmedAt: "2026-11-25T09:59:00Z", stopUnlisted: true })] });
  await page.goto("/agents");
  await expect(page.getByTestId("agent-row-grok-bot-x7k2pq")).toContainText("Stopping");
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByTestId("stop-note-warn").first()).toContainText("Not confirmed on every gateway after 60 seconds (since 2026-11-25 09:59:00 UTC)");
  await expect(page.getByTestId("stop-note-warn").nth(1)).toContainText("The stop list is full (1,000 agents)");
  await expect(page.getByTestId("agent-state")).toHaveText("Stopping"); // stays: the mock does not confirm it
});

test("start is refused while the proxies drop the keys: the console's reason is shown", async ({ page }) => {
  await setup(page, { agents: [mine({ state: "stopped", stopRequestedAt: "2026-11-25T09:59:30Z", stoppedAt: "2026-11-25T09:59:38Z" })], startError: "wait one minute after the keys were deleted, then start the agent" });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await page.getByRole("button", { name: "Start agent" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "wait one minute after the keys were deleted" })).toBeVisible();
  await expect(page.getByTestId("agent-state")).toHaveText("Stopped");
});

test("admins stop another person's agent and get the stop evidence; a person does not see the evidence link", async ({ page }) => {
  const theirs = agent({ name: "theirs-bbbbbb", displayName: "Theirs", owner: "user02@example.com", manageable: false, state: "stopped", stopRequestedAt: "2026-11-25T09:59:30Z", stoppedAt: "2026-11-25T09:59:38Z" });
  await setup(page, { role: "admin", agents: [theirs, agent({ name: "live-aaaaaa", displayName: "Live", owner: "user02@example.com", manageable: false })] });
  await page.goto("/agents?agent=theirs-bbbbbb");
  const link = page.getByRole("link", { name: "Download the stop evidence (agent-stop.zip)" });
  await expect(link).toHaveAttribute("href", "/api/evidence/agent-stop/theirs-bbbbbb.zip");
  await expect(page.getByRole("button", { name: "Start agent" })).toBeVisible();
  await page.goto("/agents?agent=live-aaaaaa");
  await expect(page.getByRole("button", { name: "Stop agent" })).toBeVisible();

  await page.unrouteAll();
  await setup(page, { agents: [mine({ state: "stopped", stoppedAt: "2026-11-25T09:59:38Z", stopRequestedAt: "2026-11-25T09:59:30Z" })] });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByRole("button", { name: "Start agent" })).toBeVisible();
  await expect(page.getByRole("link", { name: /stop evidence/ })).toHaveCount(0);
});

test("connection consent: none ticked by default; the owner ticks one and saves; it gives nothing alone", async ({ page }) => {
  const seen: Setup["seen"] = [];
  await setup(page, { agents: [mine()], seen });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  const consent = page.getByTestId("consent");
  await expect(consent).toContainText("Nothing is ticked by default.");
  await expect(consent).toContainText("Ticking gives nothing by itself");
  const slack = consent.getByLabel("May use my Slack connection");
  const jira = consent.getByLabel("May use my Jira connection");
  await expect(slack).not.toBeChecked();
  await expect(jira).not.toBeChecked();
  await expect(consent).toContainText("(not connected yet)");
  await expect(consent.getByRole("button", { name: "Save connections" })).toBeDisabled();
  await slack.check();
  await snap(page, "agents-consent");
  await consent.getByRole("button", { name: "Save connections" }).click();
  expect(await sent(seen, "PATCH", "/api/agents/grok-bot-x7k2pq")).toEqual({ connectionConsent: ["slack"] });
  await expect(consent.getByText("Saved.")).toBeVisible();
  await expect(consent.getByLabel("May use my Slack connection")).toBeChecked();
  await consent.getByLabel("May use my Slack connection").uncheck();
  await expect(consent.getByRole("button", { name: "Save connections" })).toBeEnabled();
});

test("consent: an admin sees another person's list read-only; an itself-mode agent has none; an unknown name can be removed; no connections", async ({ page }) => {
  const theirs = agent({ name: "theirs-bbbbbb", displayName: "Theirs", owner: "user02@example.com", manageable: false, connectionConsent: ["slack"] });
  const itself = agent({ name: "nightly-report", displayName: "Nightly", mode: "itself", owner: "group:platform", identity: "nightly-report@svc.ambit.internal" });
  await setup(page, { role: "admin", agents: [theirs, itself] });
  await page.goto("/agents?agent=theirs-bbbbbb");
  await expect(page.getByTestId("consent-list")).toContainText("Allowed: slack");
  await expect(page.getByTestId("consent").getByRole("checkbox")).toHaveCount(0);
  await page.goto("/agents?agent=nightly-report");
  await expect(page.getByTestId("consent-itself")).toHaveText("An itself-mode agent gets no app connections in 0.6.0.");

  await page.unrouteAll();
  await setup(page, { agents: [mine({ connectionConsent: ["slack", "old-app"] })] });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByTestId("consent")).toContainText("not a connection of yours: old-app");
  await page.getByTestId("consent").getByRole("button", { name: "Remove them" }).click();
  await expect(page.getByTestId("consent")).not.toContainText("old-app");

  await page.unrouteAll();
  await setup(page, { agents: [mine()], connections: [] });
  await page.goto("/agents?agent=grok-bot-x7k2pq");
  await expect(page.getByTestId("consent")).toContainText("There are no apps with their own sign-in.");
});

test("owner left: the banner says so and an admin moves an itself-mode agent", async ({ page }) => {
  const seen: Setup["seen"] = [];
  const itself = agent({ name: "nightly-report", displayName: "Nightly", mode: "itself", owner: "former@example.com", identity: "nightly-report@svc.ambit.internal", orphanedAt: "2026-11-24T08:00:00Z" });
  await setup(page, { role: "admin", agents: [itself], seen });
  await page.goto("/agents?agent=nightly-report");
  await expect(page.getByTestId("orphan-banner")).toContainText("The owner of this agent left on 2026-11-24 08:00:00 UTC.");
  await expect(page.getByTestId("orphan-banner")).toContainText("Then it is stopped.");
  await page.getByLabel("New owner").fill("group:platform");
  await page.getByRole("button", { name: "Move agent" }).click();
  expect(await sent(seen, "PUT", "/api/agents/nightly-report/owner")).toEqual({ owner: "group:platform" });
});
