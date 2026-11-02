// Savings page rules that run in the browser: the "later turns" control and the number formats. Pure functions.
import { test } from "node:test";
import assert from "node:assert/strict";
import { withResend, resend, groupRows, topGroups, share, usd, compact, bytes, add } from "../src/lib/savings.ts";

const comp = { results: 40, bytesIn: 1_000_000, bytesOut: 400_000, tokensSaved: 206_897, tokensLow: 155_172, tokensHigh: 258_621,
  dollars: 0.62, dollarsLow: 0.47, dollarsHigh: 0.78, priceSource: "llm" };

test("0 later turns is the headline: nothing changes", () => {
  assert.deepEqual(withResend(comp, 0), comp);
});

test("N later turns multiply tokens and dollars of compression by N + 1, the range with them", () => {
  const c = withResend(comp, 3);
  assert.equal(c.tokensSaved, 827_588);
  assert.equal(c.tokensLow, 620_688);
  assert.equal(c.tokensHigh, 1_034_484);
  assert.ok(Math.abs(c.dollars - 2.48) < 1e-9);
  assert.equal(c.bytesIn, comp.bytesIn); // bytes are measured, never multiplied
  assert.equal(c.results, 40);
});

test("a missing price stays missing: tokens only", () => {
  assert.equal(resend(null, 5), null);
  assert.equal(withResend({ ...comp, dollars: null, dollarsLow: null, dollarsHigh: null }, 5).dollars, null);
  assert.equal(add(null, null), null);
  assert.equal(add(null, 2), 2);
});

test("routing is not multiplied by the control", () => {
  const s = { groups: [{ group: "eng", compression: comp, routing: { requests: 12400, dollars: 182 } }] };
  const [g0] = groupRows(s, 0), [g5] = groupRows(s, 5);
  assert.equal(g0.route, 182);
  assert.equal(g5.route, 182);
  assert.ok(Math.abs(g5.comp - g0.comp * 6) < 1e-9);
});

test("the chart lists the top groups by dollars and skips groups without a dollar figure", () => {
  const g = (group, dollars, tokens) => ({ group, compression: { ...comp, dollars, tokensSaved: tokens }, routing: { requests: 0, dollars: null } });
  const rows = groupRows({ groups: [g("a", 1, 10), g("b", 5, 10), g("c", null, 999), ...Array.from({ length: 12 }, (_, i) => g(`x${i}`, 2 + i / 100, 1))] }, 0);
  assert.equal(rows[0].group, "b");
  const top = topGroups(rows, 10);
  assert.equal(top.length, 10);
  assert.ok(!top.some((r) => r.group === "c"));
});

test("number formats", () => {
  assert.equal(usd(29), "$29.00");
  assert.equal(usd(0.000048), "$0.000048");
  assert.equal(usd(0), "$0.00");
  assert.equal(compact(9_800_000), "9.8 M");
  assert.equal(compact(12_400), "12.4 k");
  assert.equal(compact(950), "950");
  assert.equal(bytes(41 * 2 ** 20), "41.0 MiB");
  assert.equal(bytes(512), "512 B");
});

test("a group's share of the total; overlapping groups add up to more than 100%", () => {
  assert.equal(share(79, 158.59), "50%");
  assert.equal(share(0.5, 158.59), "<1%");
  assert.equal(share(158.59, 158.59), "100%");
  const shares = [79, 60, 40].map((d) => (d / 158.59) * 100);
  assert.ok(shares.reduce((a, b) => a + b) > 100); // a person in two groups counts in both
});
