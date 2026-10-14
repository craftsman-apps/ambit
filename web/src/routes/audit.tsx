import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";
import { csvRow } from "@/lib/csv";

type Call = { time: string; email: string; route: string; method: string; server: string; tool: string; model: string; status: string; error: string; inputTokens: number; outputTokens: number; router: string; label: string; labelReason: string };
// A routed call shows the router and the model that answered, e.g. "auto → mock/echo (coding)".
const target = (c: Call) => c.router ? `${c.router} → ${c.model}${c.label || c.labelReason ? ` (${c.label || c.labelReason})` : ""}` : (c.server || c.model);
type Usage = { day: string; group: string; requests: number; inputTokens: number; outputTokens: number; cost?: number; unpricedModels?: string[] };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const field = "rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-body outline-none focus:border-accent";
const th = "px-3 py-2 text-left font-mono text-label font-medium uppercase tracking-[.1em] text-muted";
const today = () => new Date().toISOString().slice(0, 10);
const monthAgo = () => new Date(Date.now() - 30 * 864e5).toISOString().slice(0, 10);
// A refusal by policy: no or bad token, a rule or offboarding said no, over budget, or a tool the
// person may not use (the gateway calls it unknown) or that changed since it was approved.
// A 429 is a refusal only when the gateway's budget said so ("rate limit exceeded"); without that
// reason it came from the model provider (its own limit), which is a failure, not a policy decision.
const refused = (c: Call) => ["401", "403"].includes(c.status) || (c.status === "429" && /rate limit/i.test(c.error))
  || /Unknown tool|changed since it was approved/.test(c.error);
const failed = (c: Call) => !refused(c) && (c.error !== "" || Number(c.status) >= 400);
// What an auditor asks about: tool calls, model requests, and refusals before the MCP method is known.
const isCall = (c: Call) => c.method === "tools/call" || c.method === "";
const utc = (t: string) => new Date(t).toISOString().replace("T", " ").slice(0, 19);
const what = (c: Call) => c.method ? `${c.method}${c.tool ? ` · ${c.tool}` : ""}` : c.model || /llm$/.test(c.route) ? "LLM request" : c.route;
const outcome = (c: Call) => refused(c) ? "Refused by policy" : failed(c) ? "Failed" : "OK";

function csv(calls: Call[]) {
  const rows = calls.map((c) => csvRow([utc(c.time), c.email, what(c), target(c), outcome(c), c.status, c.error, c.inputTokens, c.outputTokens]));
  return ["time_utc,email,what,server_or_model,outcome,status,reason,input_tokens,output_tokens", ...rows].join("\n") + "\n";
}
function download(name: string, text: string) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

// Audit (journey A5): every gateway call one person made in a period, refused ones included.
export function AuditPage() {
  const [form, setForm] = useState({ email: "", from: monthAgo(), to: today() });
  const [q, setQ] = useState<typeof form | null>(null);
  const [protocol, setProtocol] = useState(false);
  const res = useQuery({
    queryKey: ["audit", q], enabled: !!q, retry: false,
    queryFn: () => get<{ calls: Call[]; limited: boolean }>(`/api/audit?${new URLSearchParams({ email: q!.email, from: q!.from, to: nextDay(q!.to) })}`),
  });
  const all = res.data?.calls ?? [];
  const shown = protocol ? all : all.filter(isCall);
  const search = (e: React.FormEvent) => {
    e.preventDefault();
    // The same search again must ask again (after an error, or for new calls), not reuse the answer.
    if (JSON.stringify(form) === JSON.stringify(q)) res.refetch(); else setQ({ ...form });
  };
  return (
    <Shell crumb="Evidence / Audit">
      <section className="space-y-4 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Audit</h1>
          <p className="text-muted">Every call a person made through the gateway, including the ones it refused. Kept for a year. Dates and times are UTC.</p>
        </div>
        <form className="flex flex-wrap items-end gap-2" onSubmit={search}>
          <label><span className="mb-1 block text-muted">Email</span><input id="audit-email" className={field} value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="alice@acme.com" required /></label>
          <label><span className="mb-1 block text-muted">From (UTC)</span><input id="audit-from" type="date" className={field} value={form.from} onChange={(e) => setForm({ ...form, from: e.target.value })} /></label>
          <label><span className="mb-1 block text-muted">To (UTC, inclusive)</span><input id="audit-to" type="date" className={field} value={form.to} onChange={(e) => setForm({ ...form, to: e.target.value })} /></label>
          <Button type="submit" className="w-auto" disabled={res.isFetching}>{res.isFetching ? "Searching…" : "Search"}</Button>
        </form>
        {res.isError && !res.isFetching && <p className="text-bad" role="alert">{res.error.message}</p>}
        {res.data && !res.isError && (
          <>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-small text-muted">
              <span>
                {all.filter(isCall).length} tool and model calls: {all.filter((c) => isCall(c) && refused(c)).length} refused by policy, {all.filter((c) => isCall(c) && failed(c)).length} failed
                {res.data.limited && " — showing the newest 1,000; narrow the period"}.
              </span>
              <label className="inline-flex items-center gap-1.5"><input id="audit-protocol" type="checkbox" checked={protocol} onChange={(e) => setProtocol(e.target.checked)} />
                Show protocol messages ({all.length - all.filter(isCall).length})</label>
              <Button variant="outline" className="ml-auto w-auto" disabled={all.length === 0}
                onClick={() => download(`audit-${q!.email}-${q!.from}-${q!.to}.csv`, csv(all))}>Export CSV</Button>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-body">
                <thead><tr className="border-b border-line"><th className={th}>Time (UTC)</th><th className={th}>What</th><th className={th}>Server / model</th><th className={th}>Outcome</th><th className={th}>Tokens</th></tr></thead>
                <tbody>
                  {shown.map((c, i) => (
                    <tr key={i} className="border-b border-line-soft align-top">
                      <td className="whitespace-nowrap px-3 py-2 font-mono text-small">{utc(c.time)}</td>
                      <td className="px-3 py-2 font-mono">{what(c)}</td>
                      <td className="px-3 py-2 font-mono text-small">{target(c)}</td>
                      <td className="px-3 py-2">{refused(c) ? <Status ready="False" text={`Refused by policy (${c.status})`} />
                        : failed(c) ? <Status ready="Unknown" text={`Failed (${c.status})`} /> : <Status ready="True" text={`OK (${c.status})`} />}
                        {c.error && <span className="block text-small text-muted">{c.error}</span>}</td>
                      <td className="px-3 py-2 font-mono text-small tabular-nums">{c.inputTokens || c.outputTokens ? `${c.inputTokens} in / ${c.outputTokens} out` : ""}</td>
                    </tr>
                  ))}
                  {shown.length === 0 && <tr><td colSpan={5} className="px-3 py-6 text-muted">No calls by {q!.email} in this period.</td></tr>}
                </tbody>
              </table>
            </div>
          </>
        )}
      </section>
    </Shell>
  );
}

function nextDay(d: string) { const t = new Date(d + "T00:00:00Z"); t.setUTCDate(t.getUTCDate() + 1); return t.toISOString().slice(0, 10); }

// Usage (journey A3): LLM requests (and tokens, once the gateway reports them) per group per day.
export function UsagePage() {
  const res = useQuery({ queryKey: ["usage"], queryFn: () => get<{ usage: Usage[] }>("/api/usage") });
  const usage = res.data?.usage ?? [];
  const tokens = usage.some((u) => u.inputTokens || u.outputTokens);
  const costs = usage.some((u) => u.cost !== undefined);
  const unpriced = [...new Set(usage.flatMap((u) => u.unpricedModels ?? []))];
  // Small amounts keep two significant digits ($0.000048), not a misleading $0.00.
  const usd = (n: number) => n >= 0.01 || n === 0
    ? n.toLocaleString("en-US", { style: "currency", currency: "USD" })
    : n.toLocaleString("en-US", { style: "currency", currency: "USD", maximumSignificantDigits: 2 });
  return (
    <Shell crumb="Evidence / Usage">
      <section className="max-w-4xl space-y-4 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Usage</h1>
          <p className="text-muted">LLM requests, tokens and cost per group per day (UTC), last 30 days. Someone in two groups counts toward both. Cost comes from each model's price.</p>
          {res.data && !tokens && <p className="text-small text-muted">No token usage was recorded in this period.</p>}
          {unpriced.length > 0 && <p className="text-small text-warn">No price set for {unpriced.join(", ")}: their tokens are counted but not in the cost. Set a price on the Model.</p>}
        </div>
        {res.isLoading && <p className="text-muted">Loading…</p>}
        {res.isError && <p className="text-bad" role="alert">{res.error.message}</p>}
        {res.data && (
          <table className="w-full border-collapse text-body">
            <thead><tr className="border-b border-line"><th className={th}>Day</th><th className={th}>Group</th><th className={th}>Requests</th>
              {tokens && <><th className={th}>Input tokens</th><th className={th}>Output tokens</th></>}{costs && <th className={th}>Cost (USD)</th>}</tr></thead>
            <tbody>
              {usage.map((u) => (
                <tr key={u.day + u.group} className="border-b border-line-soft">
                  <td className="px-3 py-2 font-mono">{u.day}</td><td className="px-3 py-2">{u.group}</td>
                  <td className="px-3 py-2 tabular-nums">{u.requests}</td>
                  {tokens && <><td className="px-3 py-2 tabular-nums">{u.inputTokens.toLocaleString()}</td><td className="px-3 py-2 tabular-nums">{u.outputTokens.toLocaleString()}</td></>}
                  {costs && <td className="px-3 py-2 tabular-nums">{u.cost !== undefined ? usd(u.cost) : <span className="text-muted">–</span>}</td>}
                </tr>
              ))}
              {usage.length === 0 && <tr><td colSpan={6} className="px-3 py-6 text-muted">No LLM requests in the last 30 days.</td></tr>}
            </tbody>
          </table>
        )}
      </section>
    </Shell>
  );
}
