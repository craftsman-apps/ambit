// Regenerates the console screenshots the documentation uses. No cluster and no sign-in: the Vite dev
// server serves web/, and Playwright answers every /api call with made-up data (people at acme.test).
//   cd web && npm ci && npx vite --port 5173 &      (then, from web/)
//   BASE=http://localhost:5173 node docs-screenshots.mjs      (writes into ../docs/assets/console and ../site/static/img)
// Images: docs/assets/console/server-request-{form,approve}.png (1280x800, docs/guides/request-an-app.md),
// site/static/img/console-{light,dark}.png (the Audit page, 2x, site/src/pages/index.tsx).
import { chromium } from "@playwright/test";
import { fileURLToPath } from "node:url";

const BASE = process.env.BASE ?? "http://localhost:5173";
const root = (p) => fileURLToPath(new URL(`../${p}`, import.meta.url));
const t = (d, h, m = 0, s = 0) => new Date(Date.UTC(2026, 9, 13 - d, h, m, s)).toISOString();

const user = (email, role) => ({ email, name: email.split("@")[0], role, groups: ["eng"], bootstrap: false });
const timeImage = "ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2000000000000000000000000000000000000000000000000abcd";
const req = (o) => ({ displayName: "", image: "", url: "", connection: "", transport: "stdio", reason: "", decision: "", decidedBy: "", decidedAt: null, comment: "", pullRequest: "", ...o });
const requests = [
  req({ name: "req-jira", serverName: "jira", displayName: "Jira", url: "https://mcp.atlassian.example/v1/mcp", connection: "atlassian", transport: "streamable-http",
    reason: "Engineering triages Jira tickets from Claude Code. Each person uses their own Atlassian account.", requestedBy: "marta@acme.test", requestedAt: t(1, 9) }),
  req({ name: "req-sentry", serverName: "sentry", displayName: "Sentry", image: "ghcr.io/acme/mcp-sentry@sha256:77c1a0b9d8e7f6a5b4c3d2e1f00a1b2c3d4e5f60718293a4b5c6d7e8f9012345",
    reason: "On-call needs error context during incidents.", requestedBy: "sam@acme.test", requestedAt: t(1, 11) }),
  req({ name: "req-time", serverName: "time", displayName: "Time", image: timeImage, requestedBy: "lena@acme.test", requestedAt: t(9, 10),
    decision: "Approved", decidedBy: "priya@acme.test", decidedAt: t(8, 10), comment: "Fine for engineering." }),
  req({ name: "req-shell", serverName: "shell-exec", image: "ghcr.io/acme/mcp-shell@sha256:001122334455667788990011223344556677889900112233445566778899aabb", requestedBy: "kim@acme.test", requestedAt: t(12, 10),
    decision: "Denied", decidedBy: "priya@acme.test", decidedAt: t(11, 10), comment: "Too broad. Ask for a read-only server." }),
];
const servers = [{ name: "time", displayName: "Time", image: timeImage, url: "", connection: "", transport: "stdio", ready: "True", message: "",
  tools: [{ name: "get_current_time", description: "Get the current time" }, { name: "convert_time", description: "Convert a time between time zones" }],
  requestedBy: "lena@acme.test", approvedBy: "priya@acme.test", gitManaged: false, rules: ["engineering-time"], changedTools: [] }];
const connections = [{ name: "atlassian", displayName: "Atlassian", state: "not-connected", revocable: false, connectURL: "/auth/connect/atlassian", servers: [] }];
const calls = [[7, 8], [5, 119], [5, 119], [3, 194], [0, 229]].map(([s, out], i) => ({
  time: t(0, 7, 47, [8, 5, 5, 3, 0][i]), email: "alice@acme.test", route: "/v1/chat/completions llm", method: "", server: "", tool: "", model: "nvidia/nemotron-3-super-120b-a12b:free",
  status: "200", error: "", inputTokens: 22, outputTokens: [137, 119, 119, 194, 229][i], router: "", label: "", labelReason: "" }));

async function open(browser, { me, viewport, scale = 1, dark = false }, path) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: scale, colorScheme: dark ? "dark" : "light" });
  const page = await ctx.newPage();
  const unmocked = [];
  await page.route("**/api/**", (route) => {
    const p = new URL(route.request().url()).pathname;
    const body = { "/api/me": me, "/api/config": { mode: "direct" }, "/api/servers": servers, "/api/server-requests": requests,
      "/api/connections": connections, "/api/audit": { calls, limited: false }, "/api/changes": [] }[p];
    if (body === undefined) { unmocked.push(p); return route.fulfill({ status: 404, json: { error: "not mocked" } }); }
    return route.fulfill({ json: body });
  });
  await page.goto(BASE + path);
  await page.waitForLoadState("networkidle");
  return { ctx, page, unmocked };
}

const browser = await chromium.launch({ channel: process.env.CI ? undefined : "chrome" });

{ // The form, signed in as a viewer.
  const { ctx, page, unmocked } = await open(browser, { me: user("marta@acme.test", "viewer"), viewport: { width: 1280, height: 800 } }, "/servers");
  await page.getByRole("button", { name: "Request a server" }).click();
  await page.fill("#req-name", "jira");
  await page.fill("#req-display", "Jira");
  await page.getByLabel("Already running elsewhere, reached by URL").check();
  await page.fill("#req-url", "https://mcp.atlassian.example/v1/mcp");
  await page.selectOption("#req-connection", "atlassian");
  await page.fill("#req-reason", requests[0].reason);
  await page.screenshot({ path: root("docs/assets/console/server-request-form.png") });
  console.log("form unmocked:", unmocked.join(",") || "none");
  await ctx.close();
}
{ // Waiting requests, signed in as an approver.
  const { ctx, page, unmocked } = await open(browser, { me: user("priya@acme.test", "approver"), viewport: { width: 1280, height: 800 } }, "/servers");
  await page.getByText("Waiting for an approver").waitFor();
  await page.screenshot({ path: root("docs/assets/console/server-request-approve.png") });
  console.log("approve unmocked:", unmocked.join(",") || "none");
  await ctx.close();
}
// The Audit page for the landing page: the content area (without the left navigation and top bar), 2x.
for (const dark of [false, true]) {
  const { ctx, page, unmocked } = await open(browser, { me: user("priya@acme.test", "admin"), viewport: { width: 1443, height: 900 }, scale: 2, dark }, "/audit");
  await page.fill("#audit-email", "alice@acme.test");
  await page.fill("#audit-from", "2026-09-13");
  await page.fill("#audit-to", "2026-10-13");
  await page.getByRole("button", { name: "Search" }).click();
  await page.locator("tbody tr").first().waitFor();
  const box = await page.locator("section h1").boundingBox();
  const table = await page.locator("table").boundingBox();
  const y = box.y - 6; // 7px margin at the left and right, the table's last row at the bottom
  await page.screenshot({ path: root(`site/static/img/console-${dark ? "dark" : "light"}.png`), clip: { x: box.x - 7, y, width: 1205, height: table.y + table.height + 12 - y } });
  console.log("audit unmocked:", unmocked.join(",") || "none");
  await ctx.close();
}
await browser.close();
