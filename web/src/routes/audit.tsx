import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";

type Call = { time: string; email: string; route: string; method: string; server: string; tool: string; model: string; status: string; error: string; inputTokens: number; outputTokens: number };
type Usage = { day: string; group: string; requests: number; inputTokens: number; outputTokens: number };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const field = "rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-[13px] outline-none focus:border-accent";
const th = "px-3 py-2 text-left font-mono text-[10.5px] font-medium uppercase tracking-[.1em] text-muted";
const today = () => new Date().toISOString().slice(0, 10);
const monthAgo = () => new Date(Date.now() - 30 * 864e5).toISOString().slice(0, 10);
const denied = (c: Call) => c.error !== "" || Number(c.status) >= 400;

// Audit (journey A5): every gateway call one person made in a period, denied ones included.
export function AuditPage() {
  const [form, setForm] = useState({ email: "", from: monthAgo(), to: today() });
  const [q, setQ] = useState<typeof form | null>(null);
  const res = useQuery({
    queryKey: ["audit", q], enabled: !!q,
    queryFn: () => get<{ calls: Call[]; limited: boolean }>(`/api/audit?${new URLSearchParams({ email: q!.email, from: q!.from, to: nextDay(q!.to) })}`),
  });
  return (
    <Shell crumb="Evidence / Audit">
      <section className="space-y-4 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Audit</h1>
          <p className="text-muted">Every call a person made through the gateway, including the ones it refused. Kept for a year.</p>
        </div>
        <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => { e.preventDefault(); setQ(form); }}>
          <label><span className="mb-1 block text-muted">Email</span><input id="audit-email" className={field} value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="alice@acme.com" required /></label>
          <label><span className="mb-1 block text-muted">From</span><input id="audit-from" type="date" className={field} value={form.from} onChange={(e) => setForm({ ...form, from: e.target.value })} /></label>
          <label><span className="mb-1 block text-muted">To (inclusive)</span><input id="audit-to" type="date" className={field} value={form.to} onChange={(e) => setForm({ ...form, to: e.target.value })} /></label>
          <Button type="submit" className="w-auto">Search</Button>
        </form>
        {res.isError && <p className="text-bad">{res.error.message}</p>}
        {res.data && (
          <>
            <p className="text-[12.5px] text-muted">{res.data.calls.length} calls, {res.data.calls.filter(denied).length} refused{res.data.limited && " — showing the newest 1,000; narrow the period"}.</p>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-[13px]">
                <thead><tr className="border-b border-line"><th className={th}>Time</th><th className={th}>What</th><th className={th}>Server / model</th><th className={th}>Outcome</th><th className={th}>Tokens</th></tr></thead>
                <tbody>
                  {res.data.calls.map((c, i) => (
                    <tr key={i} className="border-b border-line-soft align-top">
                      <td className="whitespace-nowrap px-3 py-2 font-mono text-[12px]">{new Date(c.time).toLocaleString()}</td>
                      <td className="px-3 py-2">{c.method ? <><span className="font-mono">{c.method}</span>{c.tool && <> · <span className="font-mono">{c.tool}</span></>}</> : c.model ? "LLM request" : c.route}</td>
                      <td className="px-3 py-2 font-mono text-[12px]">{c.server || c.model}</td>
                      <td className="px-3 py-2">{denied(c) ? <Status ready="False" text={`Denied (${c.status})`} /> : <Status ready="True" text={`OK (${c.status})`} />}
                        {c.error && <span className="block text-[12px] text-muted">{c.error}</span>}</td>
                      <td className="px-3 py-2 font-mono text-[12px] tabular-nums">{c.inputTokens || c.outputTokens ? `${c.inputTokens} in / ${c.outputTokens} out` : ""}</td>
                    </tr>
                  ))}
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

// Usage (journey A3): LLM tokens per group per day — budgets in step 8 build on this.
export function UsagePage() {
  const res = useQuery({ queryKey: ["usage"], queryFn: () => get<{ usage: Usage[] }>("/api/usage") });
  return (
    <Shell crumb="Evidence / Usage">
      <section className="max-w-4xl space-y-4 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Usage</h1>
          <p className="text-muted">LLM requests and tokens per group per day, last 30 days. Someone in two groups counts toward both.</p>
          <p className="text-[12.5px] text-muted">Token counts show once the gateway reports them: agentgateway v1.5.0 does not log tokens for model routes yet. Request counts are exact.</p>
        </div>
        {res.isError && <p className="text-bad">{res.error.message}</p>}
        <table className="w-full border-collapse text-[13px]">
          <thead><tr className="border-b border-line"><th className={th}>Day</th><th className={th}>Group</th><th className={th}>Requests</th><th className={th}>Input tokens</th><th className={th}>Output tokens</th></tr></thead>
          <tbody>
            {res.data?.usage.map((u) => (
              <tr key={u.day + u.group} className="border-b border-line-soft">
                <td className="px-3 py-2 font-mono">{u.day}</td><td className="px-3 py-2">{u.group}</td>
                <td className="px-3 py-2 tabular-nums">{u.requests}</td><td className="px-3 py-2 tabular-nums">{u.inputTokens}</td><td className="px-3 py-2 tabular-nums">{u.outputTokens}</td>
              </tr>
            ))}
            {res.data?.usage.length === 0 && <tr><td colSpan={5} className="px-3 py-6 text-muted">No LLM requests in the last 30 days.</td></tr>}
          </tbody>
        </table>
      </section>
    </Shell>
  );
}
