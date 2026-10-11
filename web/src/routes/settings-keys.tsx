import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { sendJSON } from "@/api";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";
import { day, type KeyList } from "@/routes/keys";

type KeyEvent = { at: string; actor: string; action: string; keyId: string; email: string; detail: string };
async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}

// Admin view (read-only settings; they are Helm values), every key, and the history of key changes.
export function SettingsKeysPage() {
  const keys = useQuery({ queryKey: ["admin-keys"], queryFn: () => get<KeyList>("/api/admin/keys") });
  const events = useQuery({ queryKey: ["admin-key-events"], queryFn: () => get<{ events: KeyEvent[] | null }>("/api/admin/key-events") });
  const qc = useQueryClient();
  const [confirm, setConfirm] = useState<string | null>(null);
  const revoke = useMutation({
    mutationFn: (id: string) => sendJSON("DELETE", `/api/admin/keys/${encodeURIComponent(id)}`),
    onSuccess: () => { setConfirm(null); qc.invalidateQueries({ queryKey: ["admin-keys"] }); qc.invalidateQueries({ queryKey: ["admin-key-events"] }); qc.invalidateQueries({ queryKey: ["keys"] }); },
  });
  const s = keys.data?.settings;
  return (
    <Shell crumb="Settings / Keys">
      <section className="max-w-4xl space-y-5 p-5">
        <h1 className="text-xl font-semibold tracking-tight">Keys</h1>
        {s && (
          <div className="space-y-1">
            <dl className="grid grid-cols-[170px_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-muted">Personal keys</dt><dd>{s.enabled ? "On" : "Off"}</dd>
              <dt className="text-muted">Inactivity limit</dt><dd>{s.inactivityDays} days</dd>
              <dt className="text-muted">Longest end date</dt><dd>{s.maxDays} days</dd>
            </dl>
            <p className="text-[12.5px] text-muted">Set in the Helm values (apiKeys.*)</p>
          </div>
        )}
        {keys.isError && <p className="text-bad" role="alert">{keys.error.message}</p>}
        {revoke.isError && <p className="text-bad" role="alert">{revoke.error.message}</p>}
        <h2 className="font-semibold">All keys</h2>
        {keys.data?.keys.length === 0 && <p className="text-muted">Nobody has a key.</p>}
        {!!keys.data?.keys.length && (
          <table className="w-full border-collapse text-left">
            <thead><tr className="border-b border-line text-muted"><th className="py-1.5 font-normal">Owner</th><th className="font-normal">Name</th>
              <th className="font-normal">Starts with</th><th className="font-normal">Stops on</th><th className="font-normal">Last used</th><th /></tr></thead>
            <tbody>
              {keys.data.keys.map((k) => (
                <tr key={k.id} className="border-b border-line-soft">
                  <td className="py-1.5">{k.owner}</td><td>{k.name}</td><td className="font-mono text-[12px]">{k.prefix}</td>
                  <td>{day(k.until)}</td><td>{k.lastUsed ? day(k.lastUsed) : "unknown"}</td>
                  <td className="text-right">
                    {confirm === k.id ? (
                      <span className="inline-flex gap-2" role="alert">
                        <Button className="w-auto py-1" disabled={revoke.isPending} onClick={() => revoke.mutate(k.id)}>Revoke key</Button>
                        <Button variant="outline" className="w-auto py-1" onClick={() => setConfirm(null)}>Keep it</Button>
                      </span>
                    ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirm(k.id)}>Revoke</Button>}
                  </td>
                </tr>))}
            </tbody>
          </table>
        )}
        <h2 className="font-semibold">History</h2>
        {events.isError && <p className="text-bad" role="alert">{events.error.message}</p>}
        {!events.data?.events?.length ? <p className="text-muted">No changes recorded.</p> : (
          <table className="w-full border-collapse text-left">
            <thead><tr className="border-b border-line text-muted"><th className="py-1.5 font-normal">When</th><th className="font-normal">Who</th>
              <th className="font-normal">Action</th><th className="font-normal">Owner</th><th className="font-normal">Detail</th></tr></thead>
            <tbody>
              {events.data.events.map((e, i) => (
                <tr key={i} className="border-b border-line-soft">
                  <td className="py-1.5 font-mono text-[12px]">{e.at.replace("T", " ").slice(0, 16)}</td><td>{e.actor}</td><td>{e.action}</td><td>{e.email}</td><td>{e.detail}</td>
                </tr>))}
            </tbody>
          </table>
        )}
      </section>
    </Shell>
  );
}
