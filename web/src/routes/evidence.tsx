import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";

type ExportInfo = { name: "access" | "offboarding" | "audit"; available: boolean; reason?: string };
type HistoryRow = { at: string; admin: string; export: string; period: string; email?: string; rows: number; zipSha256: string; result: string; reason?: string };
type Evidence = {
  exports: ExportInfo[]; running: boolean; maxDays: number; maxRows: number;
  retention: string; retentionFrom: "table" | "helm"; oldestRecord?: string | null; snapshotsSince?: string | null;
  history: HistoryRow[];
};

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}

const field = "rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-body outline-none focus:border-accent";
const th = "px-3 py-2 text-left font-mono text-label font-medium uppercase tracking-[.1em] text-muted";
const iso = (d: Date) => d.toISOString().slice(0, 10);
// The last full quarter (UTC): its first and last day.
function lastQuarter(now = new Date()): { from: string; to: string } {
  const q = Math.floor(now.getUTCMonth() / 3);
  const start = new Date(Date.UTC(now.getUTCFullYear(), q * 3 - 3, 1));
  const end = new Date(Date.UTC(now.getUTCFullYear(), q * 3, 0));
  return { from: iso(start), to: iso(end) };
}

const what: Record<ExportInfo["name"], { title: string; proves: string }> = {
  access: { title: "Access review", proves: "Which groups and people the access rules allowed to use which models, MCP servers and tools at the end of a date, which keys were alive, and who was seen using each group. Group membership comes from your identity provider: add its dated member list." },
  offboarding: { title: "Offboarding", proves: "Whether people who left lost access, and when: each offboarding record, the calls the gateway accepted after it (should be none), the app grants revoked and the key history." },
  audit: { title: "Audit log", proves: "Every call through the gateway in a period, by whom, with what result, refused calls and wrong keys included, with the records per day and the MCP app requests and decisions." },
};

async function sha256(b: Blob): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", await b.arrayBuffer());
  return [...new Uint8Array(d)].map((x) => x.toString(16).padStart(2, "0")).join("");
}

// Settings → Evidence (issue #89): one ZIP per export (CSV files and manifest.json with the SHA-256 of each file),
// for an auditor. Admins only.
export function EvidencePage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["evidence"], queryFn: () => get<Evidence>("/api/evidence") });
  const quarter = lastQuarter();
  const [form, setForm] = useState({ date: quarter.to, email: "", offboarding: quarter, audit: quarter });
  const [busy, setBusy] = useState<string | null>(null);
  const [done, setDone] = useState<{ name: string; file: string; sha: string } | null>(null);
  const [err, setErr] = useState<{ name: string; text: string } | null>(null);
  const e = q.data;
  const oldest = e?.oldestRecord ? iso(new Date(e.oldestRecord)) : null;
  const firstSnapshot = e?.snapshotsSince ? iso(new Date(e.snapshotsSince)) : null;
  const beforeRetention = !!e && (!oldest || form.audit.from < oldest);

  async function download(name: ExportInfo["name"]) {
    const params = new URLSearchParams(name === "access" ? { date: form.date } : form[name]);
    if (name === "audit" && form.email.trim()) params.set("email", form.email.trim());
    setBusy(name); setErr(null); setDone(null);
    try {
      const r = await fetch(`/api/evidence/${name}.zip?${params}`, { credentials: "same-origin" });
      if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `the export returned ${r.status}`);
      const blob = await r.blob(); // fails when the console stopped the file early: it has no manifest then
      const file = /filename="([^"]+)"/.exec(r.headers.get("Content-Disposition") ?? "")?.[1] ?? `ambit-${name}.zip`;
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = file;
      a.click();
      URL.revokeObjectURL(a.href);
      setDone({ name, file, sha: await sha256(blob) });
    } catch (x) {
      setErr({ name, text: x instanceof Error ? x.message : String(x) });
    } finally {
      setBusy(null);
      qc.invalidateQueries({ queryKey: ["evidence"] });
    }
  }

  return (
    <Shell crumb="Settings / Evidence">
      <section className="max-w-5xl space-y-5 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Evidence</h1>
          <p className="text-muted">Download one ZIP file per export for your auditor. Each holds CSV files and a manifest.json with the number of rows and the SHA-256 of each file, so the auditor can check each file against it. A file the console could not finish has no manifest and does not open. Dates are whole UTC days, both included.</p>
          <p className="mt-2 text-small text-warn" data-testid="personal-data">This file contains personal data. Store and share it under your company's data rules. Ambit's retention does not apply to downloaded files.</p>
        </div>
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        <div className="grid gap-4 lg:grid-cols-3">
          {e?.exports.map((x) => {
            const pk = x.name === "access" ? "audit" : x.name; // the period inputs' key (access has a date instead)
            const noSnapshot = x.name === "access" && (!firstSnapshot || form.date < firstSnapshot);
            const disabled = !x.available || busy !== null || e.running || noSnapshot;
            return (
              <div key={x.name} className="flex flex-col gap-3 rounded-md border border-line p-4" data-testid={`export-${x.name}`}>
                <h2 className="font-semibold">{what[x.name].title}</h2>
                <p className="text-small text-muted">{what[x.name].proves}</p>
                {x.name === "access" ? (
                  <label className="flex flex-col gap-1 text-small">Date (end of the UTC day)
                    <input type="date" className={field} value={form.date} onChange={(v) => setForm({ ...form, date: v.target.value })} aria-label="Access review date" /></label>
                ) : (
                  <div className="grid grid-cols-2 gap-2">
                    <label className="flex min-w-0 flex-col gap-1 text-small">From
                      <input type="date" className={`${field} w-full min-w-0`} value={form[pk].from} onChange={(v) => setForm({ ...form, [pk]: { ...form[pk], from: v.target.value } })} aria-label={`${what[x.name].title} from`} /></label>
                    <label className="flex min-w-0 flex-col gap-1 text-small">To
                      <input type="date" className={`${field} w-full min-w-0`} value={form[pk].to} onChange={(v) => setForm({ ...form, [pk]: { ...form[pk], to: v.target.value } })} aria-label={`${what[x.name].title} to`} /></label>
                  </div>
                )}
                {x.name === "audit" && (
                  <p className={`text-small ${beforeRetention ? "text-bad" : "text-muted"}`} data-testid="retention">
                    Audit records are kept for {e.retention} ({e.retentionFrom === "table" ? "from the table" : "the Helm value"}); {oldest ? `the oldest is from ${oldest}.` : "there are no records yet."}
                    {beforeRetention && " The audit log period starts before it: records before that date are not in the export, and their absence does not mean there was no activity."}
                  </p>
                )}
                {x.name === "audit" && (
                  <label className="flex flex-col gap-1 text-small">One person only (optional)
                    <input className={field} placeholder="ana@example.com" value={form.email} onChange={(v) => setForm({ ...form, email: v.target.value })} aria-label="Audit log email" /></label>
                )}
                {!x.available && <p className="text-small text-bad">{x.reason}</p>}
                {x.available && noSnapshot && (
                  <p className="text-small text-bad" data-testid="no-snapshot">No snapshot for this date. {firstSnapshot ? `Snapshots exist from ${firstSnapshot} (the upgrade to 0.5.0).` : "The controller has not recorded the access rules yet."}</p>
                )}
                <Button className="mt-auto w-auto" disabled={disabled} onClick={() => download(x.name)}>{busy === x.name ? "Exporting…" : "Download"}</Button>
                {err?.name === x.name && <p className="text-small text-bad" role="alert">{err.text}</p>}
                {done?.name === x.name && (
                  <p className="text-small" data-testid={`done-${x.name}`}>Saved {done.file}. ZIP SHA-256: <span className="break-all font-mono">{done.sha}</span>. Send this value to the auditor with the file.</p>
                )}
              </div>
            );
          })}
        </div>
        {e?.running && <p className="text-muted">An export is running; one runs at a time.</p>}
        <div>
          <h2 className="mb-2 font-mono text-label uppercase tracking-[.12em] text-muted">Export history</h2>
          {e?.history.length === 0 && <p className="text-muted">No exports yet.</p>}
          {!!e?.history.length && (
            <div className="overflow-x-auto rounded-md border border-line">
              <table className="w-full text-body" data-testid="history">
                <thead><tr><th className={th}>Time</th><th className={th}>Admin</th><th className={th}>Export</th><th className={th}>Period</th><th className={th}>Rows</th><th className={th}>ZIP SHA-256</th><th className={th}>Result</th></tr></thead>
                <tbody>
                  {e.history.map((h, i) => (
                    <tr key={h.at + i} className="border-t border-line">
                      <td className="whitespace-nowrap px-3 py-2 font-mono text-small">{h.at.replace("T", " ").slice(0, 19)}</td>
                      <td className="px-3 py-2">{h.admin}</td>
                      <td className="px-3 py-2">{h.export}{h.email ? ` (${h.email})` : ""}</td>
                      <td className="whitespace-nowrap px-3 py-2 font-mono text-small">{h.period}</td>
                      <td className="px-3 py-2 font-mono">{h.result === "started" ? "" : h.rows}</td>
                      <td className="break-all px-3 py-2 font-mono text-small">{h.zipSha256}</td>
                      <td className={`px-3 py-2 ${h.result === "failed" ? "text-bad" : ""}`} title={h.reason}>{h.result}{h.reason ? `: ${h.reason}` : ""}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>
    </Shell>
  );
}
