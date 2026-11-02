import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";
import { getJSON } from "@/api";
import { csvRow } from "@/lib/csv";
import { bytes, compact, groupRows, resend, topGroups, usd, withResend, type Savings } from "@/lib/savings";

type Response = { from: string; to: string; since: string; savings: Savings };

const th = "px-3 py-2 text-left font-mono text-label font-medium uppercase tracking-[.1em] text-muted";
const td = "px-3 py-2 tabular-nums";

// Savings: what tool-result compression and model routing saved, in tokens and dollars. Every figure is an
// estimate and says so. The numbers come from the daily summaries in ClickHouse (docs/savings.md).
export function SavingsPage() {
  const res = useQuery({ queryKey: ["savings"], queryFn: () => getJSON<Response>("/api/savings"), retry: false });
  const [later, setLater] = useState(0);
  const s = res.data?.savings;
  const since = res.data?.since ? res.data.since.slice(0, 10) : "";
  const missing = res.isError && /returned 404/.test(res.error.message);
  return (
    <Shell crumb="Evidence / Savings">
      <section className="max-w-5xl space-y-5 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Savings</h1>
          <p className="text-muted">
            What Ambit saved in the last 30 days (UTC){since && <>, counted since {since}</>}. <strong className="font-medium text-body">All figures are estimates.</strong>{" "}
            Dollars use the current Model prices. A person in two groups counts toward both groups, never twice in a total.
          </p>
        </div>
        {res.isLoading && <p className="text-muted">Loading…</p>}
        {missing && <p className="text-muted" role="status">Savings need the audit trail: set <code className="font-mono">evidence.enabled</code> in the Helm values.</p>}
        {res.isError && !missing && <p className="text-bad" role="alert">{res.error.message}</p>}
        {s && <Body s={s} later={later} setLater={setLater} />}
      </section>
    </Shell>
  );
}

function Body({ s, later, setLater }: { s: Savings; later: number; setLater: (n: number) => void }) {
  const c = withResend(s.totals.compression, later);
  const routing = s.totals.routing.dollars;
  const compDollars = c.dollars;
  const total = compDollars === null && routing === null ? null : (compDollars ?? 0) + (routing ?? 0);
  const noCompression = s.totals.compression.results === 0;
  const noRouting = s.totals.routing.requests === 0 && s.routers.length === 0;
  return (
    <>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4" aria-label="Totals">
        <Tile label="Tokens saved" value={noCompression ? "–" : `~${compact(c.tokensSaved)}`}
          how={noCompression ? "No compressed tool results." : `Estimated. Range ${compact(c.tokensLow)} to ${compact(c.tokensHigh)} (a heuristic, ±25%). Bytes removed divided by bytes per token.`} />
        <Tile label="Compression" value={noCompression ? "–" : compDollars === null ? "tokens only" : `~${usd(compDollars)}`}
          how={noCompression ? "Nothing compressed." : compDollars === null ? "No price: set a price on a Model or the reference price." : "Estimated. Saved tokens at the average input price of each group."} />
        <Tile label="Routing" value={noRouting ? "–" : routing === null ? "not priced" : `~${usd(routing)}`}
          how={noRouting ? "No request went through a ModelRouter." : "Estimated. Against the most expensive priced model each router can choose, same tokens. Gross: the classifier's own cost is not subtracted."} />
        <Tile label="Total" value={total === null ? "–" : `~${usd(total)}`} how="Estimated. Compression plus routing." strong />
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <label htmlFor="later" className="text-body">Later turns that carry each result</label>
        <select id="later" className="rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-body" value={later} onChange={(e) => setLater(Number(e.target.value))}>
          {Array.from({ length: 21 }, (_, i) => <option key={i} value={i}>{i}</option>)}
        </select>
        <span className="text-small text-muted">
          {later === 0 ? "0 = count the first send only. Ambit does not measure how often a result is sent again." : `Compression figures are shown as if each result is sent ${later} more ${later === 1 ? "time" : "times"} (your assumption, not measured).`}
        </span>
      </div>
      {s.notes.length > 0 && (
        <ul className="space-y-1 text-small text-warn" aria-label="Notes">{s.notes.map((n) => <li key={n}>{n}</li>)}</ul>
      )}
      <DayChart s={s} later={later} />
      <GroupChart s={s} later={later} />
      <GroupTable s={s} later={later} />
      <ServerTable s={s} later={later} />
      <RouterTable s={s} />
      <ul className="space-y-1 text-small text-muted" aria-label="What is not counted">
        <li>Per-group rows can overlap: someone in two groups counts in both. The tiles and the daily chart use one row per request.</li>
        <li>Totals can be incomplete when the guard cannot reach the collector (alert <code className="font-mono">AmbitGuardRecordsDropped</code>).</li>
        <li>Not counted: prompt cache discounts, the cost of a router's classifier, model fallback, and results a client sends again.</li>
        <li>Tokens come from bytes with a fixed bytes-per-token value per type, not from the tokenizer of your models.</li>
      </ul>
    </>
  );
}

function Tile({ label, value, how, strong }: { label: string; value: string; how: string; strong?: boolean }) {
  return (
    <div className={`rounded-md border p-3 ${strong ? "border-accent" : "border-line"} bg-surface`}>
      <div className="font-mono text-label uppercase tracking-[.1em] text-muted">{label} <span className="normal-case tracking-normal">(estimated)</span></div>
      <div className="mt-1 text-h1 font-semibold tabular-nums">{value}</div>
      <div className="mt-1 text-small text-muted">{how}</div>
    </div>
  );
}

const COMPRESSION = "var(--color-accent)";
const ROUTING = "var(--color-warn)";

function Legend() {
  return (
    <p className="flex gap-4 text-small text-muted">
      <span><i className="mr-1.5 inline-block size-2.5 align-middle" style={{ background: COMPRESSION }} aria-hidden />Compression</span>
      <span><i className="mr-1.5 inline-block size-2.5 align-middle" style={{ background: ROUTING }} aria-hidden />Routing</span>
    </p>
  );
}

function DayChart({ s, later }: { s: Savings; later: number }) {
  const days = s.days.map((d) => ({ day: d.day, comp: resend(d.compressionDollars, later) ?? 0, route: d.routingDollars ?? 0, tokens: Math.round(d.compressionTokens * (1 + later)) }));
  const priced = days.some((d) => d.comp > 0 || d.route > 0);
  const max = Math.max(0.01, ...days.map((d) => d.comp + d.route));
  const W = 640, H = 200, L = 52, B = 24, T = 8, plot = H - B - T;
  const bw = days.length ? Math.min(28, (W - L - 8) / days.length - 3) : 0;
  const x = (i: number) => L + 6 + i * ((W - L - 14) / Math.max(days.length, 1));
  const y = (v: number) => T + plot - (v / max) * plot;
  return (
    <div className="space-y-2">
      <h2 className="text-h2 font-semibold">Dollars saved per day (estimated)</h2>
      {days.length === 0 ? <p className="text-muted">No data in this period.</p> : (
        <>
          <Legend />
          {!priced && <p className="text-small text-muted">No dollar figure: no price. The table shows tokens.</p>}
          <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Estimated dollars saved per day, compression and routing. The table below has the same numbers." className="w-full max-w-3xl">
            {[0, 0.5, 1].map((f) => (
              <g key={f}>
                <line x1={L} x2={W - 4} y1={y(max * f)} y2={y(max * f)} stroke="var(--color-line)" strokeWidth="1" />
                <text x={L - 6} y={y(max * f) + 4} textAnchor="end" fontSize="11" fill="var(--color-muted)">{usd(max * f)}</text>
              </g>
            ))}
            {days.map((d, i) => (
              <g key={d.day}>
                <title>{`${d.day}: compression ~${usd(d.comp)}, routing ~${usd(d.route)} (estimated)`}</title>
                <rect x={x(i)} width={bw} y={y(d.comp)} height={Math.max(0, T + plot - y(d.comp))} fill={COMPRESSION} />
                <rect x={x(i)} width={bw} y={y(d.comp + d.route)} height={Math.max(0, y(d.comp) - y(d.comp + d.route))} fill={ROUTING} />
                {i % Math.ceil(days.length / 6) === 0 && <text x={x(i) + bw / 2} y={H - 6} textAnchor="middle" fontSize="11" fill="var(--color-muted)">{d.day.slice(5)}</text>}
              </g>
            ))}
          </svg>
        </>
      )}
      {days.length > 0 && (
        <table className="w-full max-w-3xl border-collapse text-small" aria-label="Dollars saved per day">
          <thead><tr className="border-b border-line"><th className={th}>Day</th><th className={th}>Tokens saved</th><th className={th}>Compression $</th><th className={th}>Routing $</th></tr></thead>
          <tbody>{days.map((d) => (
            <tr key={d.day} className="border-b border-line-soft"><td className="px-3 py-1 font-mono">{d.day}</td><td className={td}>{d.tokens.toLocaleString()}</td>
              <td className={td}>{priced ? `~${usd(d.comp)}` : "–"}</td><td className={td}>{priced ? `~${usd(d.route)}` : "–"}</td></tr>
          ))}</tbody>
        </table>
      )}
    </div>
  );
}

function GroupChart({ s, later }: { s: Savings; later: number }) {
  const rows = groupRows(s, later);
  const top = topGroups(rows, 10);
  const max = Math.max(0.01, ...top.map((r) => r.total));
  const W = 640, row = 24, L = 150, H = top.length * row + 8;
  return (
    <div className="space-y-2">
      <h2 className="text-h2 font-semibold">Dollars saved per group (estimated)</h2>
      {top.length === 0 ? <p className="text-muted">No data in this period.</p> : (
        <>
          <Legend />
          <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Estimated dollars saved for the top groups, compression and routing. The table below has the same numbers." className="w-full max-w-3xl">
            {top.map((r, i) => (
              <g key={r.group}>
                <title>{`${r.group}: compression ~${usd(r.comp)}, routing ~${usd(r.route)} (estimated)`}</title>
                <text x={L - 8} y={i * row + 16} textAnchor="end" fontSize="12" fill="var(--color-ink)">{r.group.length > 22 ? r.group.slice(0, 21) + "…" : r.group}</text>
                <rect x={L} y={i * row + 4} height={16} width={(r.comp / max) * (W - L - 70)} fill={COMPRESSION} />
                <rect x={L + (r.comp / max) * (W - L - 70)} y={i * row + 4} height={16} width={(r.route / max) * (W - L - 70)} fill={ROUTING} />
                <text x={L + (r.total / max) * (W - L - 70) + 6} y={i * row + 16} fontSize="11" fill="var(--color-muted)">~{usd(r.total)}</text>
              </g>
            ))}
          </svg>
          {rows.length > top.length && <p className="text-small text-muted">Top {top.length} of {rows.length} groups. A person in two groups counts in both, so group bars do not add up to the total.</p>}
        </>
      )}
    </div>
  );
}

function GroupTable({ s, later }: { s: Savings; later: number }) {
  const rows = groupRows(s, later);
  const csv = () => {
    const lines = [csvRow(["group", "tokens saved (estimated)", "compression USD (estimated)", "routing requests", "routing USD (estimated)"]),
      ...rows.map((r) => csvRow([r.group, r.tokens, r.comp.toFixed(4), r.requests, r.route.toFixed(4)]))];
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([lines.join("\n") + "\n"], { type: "text/csv" }));
    a.download = "savings-by-group.csv";
    a.click();
    URL.revokeObjectURL(a.href);
  };
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <h2 className="text-h2 font-semibold">By group (estimated)</h2>
        {rows.length > 0 && <Button variant="outline" onClick={csv} className="w-auto py-1.5">Export CSV</Button>}
      </div>
      {rows.length === 0 ? <p className="text-muted">No data in this period.</p> : (
        <table className="w-full border-collapse text-small" aria-label="Savings by group">
          <thead><tr className="border-b border-line"><th className={th}>Group</th><th className={th}>Tokens saved</th><th className={th}>Compression $</th><th className={th}>Routed requests</th><th className={th}>Routing $</th></tr></thead>
          <tbody>{rows.map((r) => (
            <tr key={r.group} className="border-b border-line-soft"><td className="px-3 py-1">{r.group}</td><td className={td}>{r.tokens.toLocaleString()}</td>
              <td className={td}>{r.hasCompPrice ? `~${usd(r.comp)}` : <span className="text-muted">tokens only</span>}</td>
              <td className={td}>{r.requests.toLocaleString()}</td><td className={td}>{r.hasRoutePrice ? `~${usd(r.route)}` : <span className="text-muted">–</span>}</td></tr>
          ))}</tbody>
        </table>
      )}
    </div>
  );
}

function ServerTable({ s, later }: { s: Savings; later: number }) {
  return (
    <div className="space-y-2">
      <h2 className="text-h2 font-semibold">Compression by server and type</h2>
      {s.servers.length === 0 ? (
        <p className="text-muted">No tool result was compressed in this period. Turn on <code className="font-mono">resultCompression</code> on a Server (needs <code className="font-mono">protection.enabled</code> and <code className="font-mono">evidence.enabled</code>).</p>
      ) : (
        <table className="w-full border-collapse text-small" aria-label="Compression by server and type">
          <thead><tr className="border-b border-line"><th className={th}>Server</th><th className={th}>Type</th><th className={th}>Texts</th><th className={th}>Bytes in</th><th className={th}>Bytes out</th><th className={th}>Smaller</th><th className={th}>Tokens saved (est.)</th></tr></thead>
          <tbody>{s.servers.map((x) => (
            <tr key={x.server + x.type} className="border-b border-line-soft"><td className="px-3 py-1">{x.server}</td><td className="px-3 py-1 font-mono">{x.type}</td>
              <td className={td}>{x.results.toLocaleString()}</td><td className={td}>{bytes(x.bytesIn)}</td><td className={td}>{bytes(x.bytesOut)}</td>
              <td className={td}>{x.bytesIn ? Math.round((1 - x.bytesOut / x.bytesIn) * 100) : 0}%</td><td className={td}>{Math.round(x.tokensSaved * (1 + later)).toLocaleString()}</td></tr>
          ))}</tbody>
        </table>
      )}
    </div>
  );
}

function RouterTable({ s }: { s: Savings }) {
  return (
    <div className="space-y-2">
      <h2 className="text-h2 font-semibold">Routing by router</h2>
      {s.routers.length === 0 ? <p className="text-muted">No request went through a ModelRouter in this period.</p> : (
        <table className="w-full border-collapse text-small" aria-label="Routing by router">
          <thead><tr className="border-b border-line"><th className={th}>Router</th><th className={th}>Requests</th><th className={th}>Baseline: the most expensive of</th><th className={th}>$ saved (est.)</th><th className={th}>Not priced</th></tr></thead>
          <tbody>{s.routers.map((r) => (
            <tr key={r.router} className="border-b border-line-soft"><td className="px-3 py-1 font-mono">{r.router}</td><td className={td}>{r.requests.toLocaleString()}</td>
              <td className="px-3 py-1">{r.missing ? <span className="text-muted">router no longer exists</span> : r.baselineModels.join(", ") || "–"}</td>
              <td className={td}>{r.dollars === null ? <span className="text-muted">not priced</span> : `~${usd(r.dollars)}`}</td>
              <td className="px-3 py-1">{r.unpricedModels.join(", ") || "–"}</td></tr>
          ))}</tbody>
        </table>
      )}
      <p className="text-small text-muted">Baseline: the most expensive model this router can choose, for anyone. Same tokens assumed. Prices and routers are read when this page opens, so a change re-prices past days.</p>
    </div>
  );
}

