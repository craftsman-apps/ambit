// The agents page helpers (docs/agents.md). Pure functions, so they run without a browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { lastUseByAgent, NAME_RE, parseRanges, PLATFORM_RE, platformText, scopeSummary, toggleScope, toolsUsed } from "../src/lib/agents.ts";

test("scopeSummary: an empty scope says none; servers and tools are counted", () => {
  assert.equal(scopeSummary([]), "none");
  assert.equal(scopeSummary(["crm"]), "1 server");
  assert.equal(scopeSummary(["crm", "wiki", "jira/create_issue"]), "2 servers, 1 tool");
  assert.equal(scopeSummary(["jira/create_issue", "jira/get_issue"]), "2 tools");
});

test("toggleScope: a whole server replaces its tools; a tool is added and removed", () => {
  assert.deepEqual(toggleScope([], "crm"), ["crm"]);
  assert.deepEqual(toggleScope(["crm/read_contact", "wiki"], "crm"), ["wiki", "crm"]);
  assert.deepEqual(toggleScope(["crm"], "crm"), []);
  assert.deepEqual(toggleScope(["wiki"], "crm/read_contact"), ["wiki", "crm/read_contact"]);
  assert.deepEqual(toggleScope(["crm/read_contact", "crm/list"], "crm/list"), ["crm/read_contact"]);
});

test("parseRanges: lines, commas and spaces; empty text gives none", () => {
  assert.deepEqual(parseRanges(""), []);
  assert.deepEqual(parseRanges("203.0.113.0/24\n2001:db8::/32"), ["203.0.113.0/24", "2001:db8::/32"]);
  assert.deepEqual(parseRanges(" 10.0.0.0/8, 192.0.2.0/24 "), ["10.0.0.0/8", "192.0.2.0/24"]);
});

test("lastUseByAgent: the newest key use per agent; keys without an agent or a use are skipped", () => {
  const m = lastUseByAgent([
    { agent: "a", lastUsed: "2026-11-20T10:00:00Z" }, { agent: "a", lastUsed: "2026-11-22T10:00:00Z" }, { agent: "a", lastUsed: null },
    { agent: "b", lastUsed: null }, { lastUsed: "2026-11-25T10:00:00Z" },
  ]);
  assert.equal(m.get("a"), "2026-11-22T10:00:00Z");
  assert.equal(m.has("b"), false);
  assert.equal(m.size, 1);
});

test("toolsUsed: counts tool calls only, most used first", () => {
  const calls = [
    { method: "tools/call", server: "crm", tool: "read_contact" }, { method: "tools/call", server: "crm", tool: "read_contact" },
    { method: "tools/call", server: "wiki", tool: "search" }, { method: "tools/list", server: "", tool: "" }, { method: "", server: "", tool: "" },
  ];
  assert.deepEqual(toolsUsed(calls), [["crm · read_contact", 2], ["wiki · search", 1]]);
  assert.deepEqual(toolsUsed([]), []);
});

test("platform and name checks follow the console's patterns", () => {
  assert.equal(platformText("claude-ai"), "Claude.ai");
  assert.equal(platformText("my-bot"), "my-bot");
  assert.ok(PLATFORM_RE.test("grok") && PLATFORM_RE.test("a.b_c-1") && !PLATFORM_RE.test("Grok") && !PLATFORM_RE.test("-x") && !PLATFORM_RE.test("a".repeat(41)));
  assert.ok(NAME_RE.test("nightly-report") && !NAME_RE.test("Nightly") && !NAME_RE.test("-a") && !NAME_RE.test("a-"));
});

import { secondsBetween, stopNotes, toggleName, utcTime } from "../src/lib/agents.ts";
const agent = (o) => ({ name: "a", displayName: "A", owner: "u@example.com", mode: "person", platform: "grok", scope: [], connectionConsent: [], state: "active", rev: 1, created: "", createdBy: "", manageable: true, ...o });

test("utcTime and secondsBetween", () => {
  assert.equal(utcTime("2026-11-26T09:15:02.500Z"), "2026-11-26 09:15:02 UTC");
  assert.equal(secondsBetween("2026-11-26T09:00:00Z", "2026-11-26T09:00:08.4Z"), 8);
  assert.equal(secondsBetween("2026-11-26T09:00:10Z", "2026-11-26T09:00:00Z"), 0);
});

test("stopNotes: an active agent has none; stopping says calls may still be accepted; stopped says measured and never zero", () => {
  assert.deepEqual(stopNotes(agent({})), []);
  const s = stopNotes(agent({ state: "stopping", stopRequestedAt: "2026-11-26T09:00:00Z" }));
  assert.equal(s.length, 1);
  assert.match(s[0].text, /Stopping since 2026-11-26 09:00:00 UTC/);
  assert.match(s[0].text, /keys are being deleted/);
  assert.match(s[0].text, /only after every gateway has confirmed/);
  assert.match(s[0].text, /a call can still be accepted/);
  const w = stopNotes(agent({ state: "stopping", stopRequestedAt: "2026-11-26T09:00:00Z", stopNotConfirmedAt: "2026-11-26T09:01:00Z", stopUnlisted: true }));
  assert.deepEqual(w.map((n) => n.tone), ["info", "warn", "warn"]);
  const d = stopNotes(agent({ state: "stopped", stopRequestedAt: "2026-11-26T09:00:00Z", stoppedAt: "2026-11-26T09:00:08Z" }));
  assert.match(d[0].text, /8 seconds after the request \(measured\)/);
  assert.doesNotMatch(JSON.stringify([s, w, d]), /zero|no call|0 calls/i);
});

test("toggleName adds and removes a connection name", () => {
  assert.deepEqual(toggleName([], "slack"), ["slack"]);
  assert.deepEqual(toggleName(["slack", "jira"], "slack"), ["jira"]);
});
