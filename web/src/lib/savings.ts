// Savings page: types of GET /api/savings and the pure rules the page applies in the browser. No browser needed to test them.

export type Compression = {
  results: number; bytesIn: number; bytesOut: number; tokensSaved: number; tokensLow: number; tokensHigh: number;
  dollars: number | null; dollarsLow: number | null; dollarsHigh: number | null; priceSource: string;
};
export type Routing = { requests: number; dollars: number | null };
export type Savings = {
  estimated: boolean;
  settings: { referenceInputPrice: number | null };
  totals: { compression: Compression; routing: Routing };
  days: { day: string; compressionDollars: number | null; routingDollars: number | null; compressionTokens: number }[];
  groups: { group: string; compression: Compression; routing: Routing }[];
  servers: { server: string; type: string; results: number; bytesIn: number; bytesOut: number; tokensSaved: number; omitted: number }[];
  routers: { router: string; baselineModels: string[]; requests: number; dollars: number | null; unpricedModels: string[]; missing: boolean;
    classifierRequests: number; classifierDollars: number | null }[];
  notes: string[];
};

// Small amounts keep two significant digits ($0.000048), not a misleading $0.00 (as on the Usage page). Routing can be
// negative (a classifier that costs more than routing saves).
export function usd(n: number): string {
  return Math.abs(n) >= 0.01 || n === 0
    ? n.toLocaleString("en-US", { style: "currency", currency: "USD" })
    : n.toLocaleString("en-US", { style: "currency", currency: "USD", maximumSignificantDigits: 2 });
}

export function compact(n: number): string {
  const a = Math.abs(n);
  if (a >= 1e9) return `${(n / 1e9).toFixed(1)} B`;
  if (a >= 1e6) return `${(n / 1e6).toFixed(1)} M`;
  if (a >= 1e4) return `${(n / 1e3).toFixed(1)} k`;
  return Math.round(n).toLocaleString("en-US");
}

export function bytes(n: number): string {
  if (n >= 2 ** 30) return `${(n / 2 ** 30).toFixed(1)} GiB`;
  if (n >= 2 ** 20) return `${(n / 2 ** 20).toFixed(1)} MiB`;
  if (n >= 2 ** 10) return `${(n / 2 ** 10).toFixed(1)} KiB`;
  return `${n} B`;
}

export const add = (a: number | null, b: number | null): number | null => (a === null && b === null ? null : (a ?? 0) + (b ?? 0));

// The admin's assumption "later turns that carry each result": a result sent again N times costs N more reads.
// It applies to compression only. Routing is a price difference per request and is never multiplied.
export const resend = (v: number | null, later: number): number | null => (v === null ? null : v * (1 + later));

export function withResend(c: Compression, later: number): Compression {
  const k = 1 + later;
  return { ...c, tokensSaved: Math.round(c.tokensSaved * k), tokensLow: Math.round(c.tokensLow * k), tokensHigh: Math.round(c.tokensHigh * k),
    dollars: resend(c.dollars, later), dollarsLow: resend(c.dollarsLow, later), dollarsHigh: resend(c.dollarsHigh, later) };
}

export type GroupRow = { group: string; tokens: number; comp: number; route: number; total: number; requests: number; hasCompPrice: boolean; hasRoutePrice: boolean };

export function groupRows(s: Savings, later: number): GroupRow[] {
  return s.groups.map((g) => {
    const c = withResend(g.compression, later);
    const comp = c.dollars ?? 0, route = g.routing.dollars ?? 0;
    return { group: g.group, tokens: c.tokensSaved, comp, route, total: comp + route, requests: g.routing.requests, hasCompPrice: c.dollars !== null, hasRoutePrice: g.routing.dollars !== null };
  }).sort((a, b) => b.total - a.total || b.tokens - a.tokens || a.group.localeCompare(b.group));
}

// The chart shows the top groups only. The rest are in the table; they are not summed into "others", because groups overlap.
export const topGroups = (rows: GroupRow[], n: number): GroupRow[] => rows.filter((r) => r.total > 0).slice(0, n);

// A group's part of the total, as "34%" ("<1%" under one percent). Groups overlap, so the shares of all groups add up to more than 100%.
export function share(part: number, total: number): string {
  const p = (part / total) * 100;
  return p > 0 && p < 1 ? "<1%" : `${Math.round(p)}%`;
}
