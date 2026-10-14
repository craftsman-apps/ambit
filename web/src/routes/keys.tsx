import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { sendJSON, useModels, useMyAccess } from "@/api";
import { cn } from "@/lib/utils";
import { FIRST_USE_TIMEOUT_MS, firstUseLine, nextTabId, shellNames, toolTabs, usableModelIds, type Block, type Shell as ShellName } from "@/lib/tool-setup";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";

export type KeyView = { id: string; name: string; prefix: string; scope: "llm" | "mcp" | "both"; created: string;
  expires: string; until: string; owner?: string; lastUsed: string | null;
  status: "active" | "stops-soon" | "stopped"; stopsBecause: "inactivity" | "end-date" };
export type KeySettings = { enabled: boolean; inactivityDays: number; maxDays: number; lastUsedAvailable: boolean };
export type KeyList = { settings: KeySettings; keys: KeyView[] };
type Created = { key: string; view: KeyView; urls: { llm: string; mcp: string } };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
// Key lists only, never a plain key. The banner (on every page) asks without last use, which costs an audit query.
export const useKeys = (lastUsed = true) => useQuery({ queryKey: ["keys", lastUsed],
  queryFn: () => get<KeyList>(lastUsed ? "/api/keys" : "/api/keys?lastUsed=false"), refetchInterval: 60_000 });

const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 text-body outline-none focus:border-accent";
export const day = (iso: string) => iso.slice(0, 10);
// "inactivity": the owner's last sign-in sets the stop, before the key's end date.
export const statusText = (k: KeyView) => k.status === "stopped"
  ? (k.stopsBecause === "inactivity" ? "Stopped — owner inactive" : "Stopped — end date")
  : k.status === "stops-soon" ? "Stops soon" : "Active";
const scopeName = { llm: "LLM", mcp: "MCP", both: "LLM and MCP" } as const;

// Copy with the clipboard API; if it is blocked, select the text so the person can press Ctrl+C.
function CopyBlock({ text, testId, label }: { text: string; testId: string; label: string }) {
  const [state, setState] = useState<"" | "copied" | "select">("");
  const copy = async (el: HTMLElement | null) => {
    try { await navigator.clipboard.writeText(text); setState("copied"); }
    catch {
      if (el) { const r = document.createRange(); r.selectNodeContents(el); const s = getSelection(); s?.removeAllRanges(); s?.addRange(r); }
      setState("select");
    }
  };
  let pre: HTMLElement | null = null;
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between"><span className="text-muted">{label}</span>
        <Button type="button" variant="outline" className="w-auto py-1" aria-label={`Copy ${label}`} onClick={() => copy(pre)}>Copy</Button></div>
      <pre ref={(e) => { pre = e; }} data-testid={testId} className="overflow-x-auto whitespace-pre-wrap break-all rounded-md border border-line bg-sunken p-2 font-mono text-small select-all">{text}</pre>
      {state === "copied" && <p className="text-small text-muted" role="status">Copied.</p>}
      {state === "select" && <p className="text-small text-warn" role="status">Could not copy. The text is selected: press Ctrl+C (Cmd+C on a Mac).</p>}
    </div>
  );
}

// Shown under the snippets. The keys list already carries each key's last use (from the audit trail); poll it until
// the first request appears. The poll asks for the list only: the plain key is never sent again.
function FirstUse({ id }: { id: string }) {
  const [started] = useState(Date.now);
  const [timedOut, setTimedOut] = useState(false);
  useEffect(() => { const t = setTimeout(() => setTimedOut(true), FIRST_USE_TIMEOUT_MS); return () => clearTimeout(t); }, []);
  const q = useQuery({ queryKey: ["keys", "first-use", id], refetchInterval: (query) => (query.state.data?.lastUsed || query.state.data?.available === false || timedOut ? false : 30_000),
    queryFn: async () => { const l = await get<KeyList>("/api/keys"); return { available: l.settings.lastUsedAvailable, lastUsed: l.keys.find((k) => k.id === id)?.lastUsed ?? null }; } });
  const line = firstUseLine({ available: q.data?.available, lastUsed: q.data?.lastUsed ?? null, waitedMs: timedOut ? FIRST_USE_TIMEOUT_MS : Date.now() - started });
  if (!line) return null;
  return <p data-testid="first-use" role="status" className={line.kind === "seen" ? "text-ok" : line.kind === "timeout" ? "text-warn" : "text-muted"}>{line.text}</p>;
}

function KeyCreated({ created, onDone }: { created: Created; onDone: () => void }) {
  const models = useModels();
  const access = useMyAccess();
  const ids = usableModelIds(models.data, access.data);
  const [chosen, setChosen] = useState("");
  const model = chosen || ids[0] || "";
  const tabs = toolTabs({ ...created.urls, key: created.key, model }, created.view.scope);
  const [active, setActive] = useState(tabs[0].id);
  const [shell, setShell] = useState<ShellName>("bash");
  const tab = tabs.find((t) => t.id === active) ?? tabs[0];
  const firstShells = tab.blocks.findIndex((b) => b.shells);
  const picker = <div role="group" aria-label="Shell" className="flex items-center gap-1"><span className="mr-1 text-muted">Shell</span>
    {(Object.keys(shellNames) as ShellName[]).map((k) => <Button key={k} type="button" variant={k === shell ? "primary" : "outline"} aria-pressed={k === shell}
      className="w-auto py-1" onClick={() => setShell(k)}>{shellNames[k]}</Button>)}</div>;
  const block = (b: Block, i: number) => <div key={`${tab.id}-${i}-${b.shells ? shell : ""}`} className="space-y-3">{i === firstShells && picker}
    <CopyBlock text={b.shells ? b.shells[shell] : b.text} testId={`snippet-${tab.id}-${i}`} label={b.label} /></div>;
  return (
    <div className="space-y-3 rounded-md border border-line p-4">
      <h2 className="font-semibold">Key ‘{created.view.name}’ created</h2>
      <p className="font-medium">This is the only time the key is shown. Store it now.</p>
      <CopyBlock text={created.key} testId="new-key" label="Key" />
      <h3 className="pt-1 font-semibold">Connect a tool</h3>
      {created.view.scope !== "mcp" && (
        <label className="flex items-center gap-2"><span className="text-muted">Model name</span>
          {ids.length ? <select className={cn(field, "w-auto")} value={model} onChange={(e) => setChosen(e.target.value)}>
            {ids.map((m) => <option key={m}>{m}</option>)}</select>
            : models.data && access.data && <span className="text-muted">No model is open to you. The snippets show {"<model-id>"}: replace it with a provider model name once you have access.</span>}</label>
      )}
      <div role="tablist" aria-label="Tool" className="flex flex-wrap gap-1 border-b border-line"
        onKeyDown={(e) => { const n = nextTabId(tabs.map((t) => t.id), tab.id, e.key); if (!n) return; e.preventDefault(); setActive(n); document.getElementById(`tab-${n}`)?.focus(); }}>
        {tabs.map((t) => <button key={t.id} id={`tab-${t.id}`} role="tab" type="button" aria-selected={t.id === tab.id} aria-controls="tool-panel"
          tabIndex={t.id === tab.id ? 0 : -1} onClick={() => setActive(t.id)}
          className={cn("-mb-px border-b-2 px-3 py-1.5 text-body focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent", t.id === tab.id ? "border-accent font-medium text-ink" : "border-transparent text-muted hover:text-ink")}>{t.title}</button>)}
      </div>
      <div role="tabpanel" id="tool-panel" aria-labelledby={`tab-${tab.id}`} tabIndex={0} className="space-y-3 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent">
        {tab.warning && <p role="alert" data-testid="tab-warning" className="rounded-md bg-warn-soft p-2 text-warn">{tab.warning}</p>}
        <ol className="list-decimal space-y-1 pl-5">{tab.steps.map((s) => <li key={s}>{s}</li>)}</ol>
        {tab.blocks.map(block)}
      </div>
      <FirstUse id={created.view.id} />
      <Button className="w-auto" onClick={onDone}>Done</Button>
    </div>
  );
}

function CreateKey({ settings, onClose }: { settings: KeySettings; onClose: () => void }) {
  const qc = useQueryClient();
  const options = [30, 90, 365].filter((d) => d <= settings.maxDays);
  const [name, setName] = useState("");
  const [scope, setScope] = useState("both");
  const [days, setDays] = useState(String(options.includes(90) ? 90 : options[options.length - 1] ?? 30));
  const [created, setCreated] = useState<Created | null>(null); // the only place the plain key lives
  const create = useMutation({
    // gcTime 0 and reset() on Done: the plain key must not stay in TanStack's mutation cache after it is dismissed.
    // (The query client is deliberately not exposed on window to assert this in a browser test.)
    gcTime: 0,
    mutationFn: () => sendJSON("POST", "/api/keys", { name: name.trim(), scope, days: Number(days) }) as Promise<Created>,
    onSuccess: (c) => { setCreated(c); qc.invalidateQueries({ queryKey: ["keys"] }); },
  });
  if (created) return <KeyCreated created={created} onDone={() => { setCreated(null); create.reset(); onClose(); }} />;
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label="New key" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <h2 className="font-semibold">New key</h2>
      <label className="block"><span className="mb-1 block text-muted">Name</span>
        <input className={field} value={name} maxLength={100} onChange={(e) => setName(e.target.value)} placeholder="my laptop" required /></label>
      <div className="grid grid-cols-2 gap-3">
        <label className="block"><span className="mb-1 block text-muted">Scope</span>
          <select className={field} value={scope} onChange={(e) => setScope(e.target.value)}>
            <option value="llm">LLM</option><option value="mcp">MCP</option><option value="both">LLM and MCP</option></select></label>
        <label className="block"><span className="mb-1 block text-muted">End date</span>
          <select className={field} value={days} onChange={(e) => setDays(e.target.value)}>
            {options.map((d) => <option key={d} value={d}>{d} days from now</option>)}</select></label>
      </div>
      {create.isError && <p className="text-bad" role="alert">{create.error.message}</p>}
      {create.isError && create.error.message.startsWith("Sign in again") && (
        // sessions from before the upgrade carry no sign-in time; signing out and in again gives one
        <Button type="button" variant="outline" className="w-auto" onClick={async () => {
          await fetch("/auth/logout", { method: "POST", credentials: "same-origin" }).catch(() => null);
          window.location.assign("/login");
        }}>Sign out and sign in again</Button>
      )}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={create.isPending}>Create</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );
}

export function KeysPage() {
  const q = useKeys();
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [confirm, setConfirm] = useState<string | null>(null);
  const revoke = useMutation({
    mutationFn: (id: string) => sendJSON("DELETE", `/api/keys/${encodeURIComponent(id)}`),
    onSuccess: () => { setConfirm(null); qc.invalidateQueries({ queryKey: ["keys"] }); },
  });
  const enabled = q.data?.settings.enabled;
  return (
    <Shell crumb="You / Keys">
      <section className="max-w-4xl space-y-4 p-5">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-h1 font-semibold tracking-tight">Keys</h1>
            <p className="text-muted">Personal keys for tools that call the gateway, such as Claude Code or Cursor. A key acts as you, and stops on its end date.</p>
          </div>
          {enabled && !creating && <Button className="w-auto shrink-0" onClick={() => setCreating(true)}>Create key</Button>}
        </div>
        {q.data && !enabled && <p className="text-muted">Keys are switched off by your administrator.</p>}
        {creating && q.data && <CreateKey settings={q.data.settings} onClose={() => setCreating(false)} />}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {revoke.isError && <p className="text-bad" role="alert">{revoke.error.message}</p>}
        {q.data?.keys.length === 0 && <p className="text-muted">You have no keys.</p>}
        {!!q.data?.keys.length && (
          <table className="w-full border-collapse text-left">
            <thead><tr className="border-b border-line text-muted">
              <th className="py-1.5 font-normal">Name</th><th className="font-normal">Scope</th><th className="font-normal">Starts with</th>
              <th className="font-normal">Status</th><th className="font-normal">Stops on</th><th className="font-normal">Last used</th><th /></tr></thead>
            <tbody>
              {q.data.keys.map((k) => (
                <tr key={k.id} className="border-b border-line-soft">
                  <td className="py-1.5">{k.name}</td><td>{scopeName[k.scope]}</td>
                  <td className="font-mono text-small">{k.prefix}</td><td>{statusText(k)}</td><td>{day(k.until)}</td>
                  <td>{k.lastUsed ? day(k.lastUsed) : "unknown"}</td>
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
      </section>
    </Shell>
  );
}
