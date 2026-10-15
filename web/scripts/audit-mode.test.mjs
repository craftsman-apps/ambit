// Audit-only wording and thresholds. Pure functions, so they run without a browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { daysSince, dayCount, modeBanner, pendingBanner, pendingPage, outcomeLabel, isStale, switchCheck } from "../src/lib/audit-mode.ts";

const now = new Date("2026-10-15T10:00:00Z");

test("days since the mode started", () => {
  assert.equal(daysSince("2026-10-12T10:00:00Z", now), 3);
  assert.equal(daysSince("2026-10-15T09:59:00Z", now), 0);
  assert.equal(daysSince("2026-10-16T10:00:00Z", now), 0); // a clock ahead is not negative
  assert.equal(dayCount(0), "today");
  assert.equal(dayCount(1), "1 day");
  assert.equal(dayCount(3), "3 days");
  assert.equal(modeBanner("2026-10-12T10:00:00Z", now), "Audit-only mode since 2026-10-12 (3 days).");
  assert.equal(modeBanner(undefined, now), "Audit-only mode."); // before the controller published a time
});

test("a change that agentgateway has not applied", () => {
  const p = { mode: "audit", rulesVersion: "bbbbbbbbbbbb", step: "authorization", error: "waiting for agentgateway to apply AgentgatewayModel/m generation 3" };
  assert.equal(pendingBanner(p, "enforce"), "Mode switch to audit is not complete: waiting for agentgateway to apply AgentgatewayModel/m generation 3");
  assert.equal(pendingBanner({ ...p, mode: "enforce", error: "" }, "enforce"), "The latest access-rule change is not active yet: waiting for agentgateway (authorization)");
});

test("a rejected Model: the banner text names it, and the Models page is linked", () => {
  const error = "agentgateway rejected Model/mistral-x: bad key. Check the provider secret of Model/mistral-x, or remove the Model.";
  const p = { mode: "enforce", rulesVersion: "bbbbbbbbbbbb", step: "authorization", error, objects: ["Model/mistral-x"] };
  assert.equal(pendingBanner(p, "enforce"), `The latest access-rule change is not active yet: ${error}`);
  assert.deepEqual(pendingPage(p), { to: "/models", label: "Open the Models page" });
  assert.equal(pendingPage({ ...p, objects: ["Model/a", "Model/b"] })?.to, "/models");
  assert.equal(pendingPage({ ...p, objects: ["ModelRouter/fast"] }), undefined); // no page for a router
  assert.equal(pendingPage({ ...p, objects: [] }), undefined);
  assert.equal(pendingPage({ ...p, objects: undefined }), undefined);
});

test("outcome labels: a real refusal, a verdict, and a missing mark are told apart", () => {
  assert.equal(outcomeLabel("refused", "403")?.text, "Refused (403)");
  assert.equal(outcomeLabel("would-refuse", "200")?.text, "Would be refused (200)");
  assert.match(outcomeLabel("other-model", "200").text, /unless a later route matched/);
  assert.equal(outcomeLabel("mark-missing", "200")?.text, "Mark missing");
  for (const o of ["allow", "n/a", undefined]) assert.equal(outcomeLabel(o, "200"), undefined);
});

test("stale after one hour, and with no record at all", () => {
  assert.equal(isStale("2026-10-15T09:00:01Z", now), false);
  assert.equal(isStale("2026-10-15T08:59:59Z", now), true);
  assert.equal(isStale(null, now), true);
});

test("the switch check needs 7 days of the current rules and nothing counted", () => {
  const s = { from: "", to: "", rules: "current", requests: 0, people: 0, markMissing: 0, refused: 0, newest: null, groups: [], limited: false };
  assert.match(switchCheck(s, "2026-10-08T10:00:00Z", now), /would refuse nothing seen since 2026-10-08 10:00 UTC/);
  assert.match(switchCheck(s, "2026-10-14T16:20:00Z", now), /rules have been in force for less than 1 day\. Wait until they have been in force for 7 days/);
  assert.equal(switchCheck({ ...s, requests: 1 }, "2026-10-08T10:00:00Z", now), undefined);
  assert.equal(switchCheck({ ...s, markMissing: 1 }, "2026-10-08T10:00:00Z", now), undefined); // a missing mark is not "nothing"
  assert.equal(switchCheck({ ...s, rules: "all" }, "2026-10-08T10:00:00Z", now), undefined);
  assert.equal(switchCheck(s, undefined, now), undefined);
});

test("the pages say who is let through, by mode", async () => {
  const { closedWording } = await import("../src/lib/audit-mode.ts");
  const enforce = closedWording(false), audit = closedWording(true);
  assert.equal(enforce.rules, "Who may use which models and MCP tools. Anything no rule names is closed to everyone.");
  assert.equal(enforce.noRules, "No access rules yet, so no model can be used.");
  assert.equal(enforce.count(4), "4 closed to everyone");
  assert.equal(enforce.noServerRule, "None: closed to everyone");
  assert.match(audit.rules, /^Who may use which models and MCP tools\. Audit-only mode: anything no rule names is let through and marked as would be refused\.$/);
  assert.equal(audit.count(4), "4 named by no rule (let through)");
  for (const k of ["rules", "noRules", "models", "servers", "noServerRule"]) {
    assert.doesNotMatch(audit[k], /closed to everyone|no model can be used/, k);
    assert.match(audit[k], /let through|would be refused/, k);
  }
  assert.doesNotMatch(audit.count(1), /closed to everyone/);
});
