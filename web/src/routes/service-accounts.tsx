import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { sendJSON, useConfig, useGroups } from "@/api";
import { ciTabs } from "@/lib/tool-setup";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";
import { ConnectTool, CopyBlock, FirstUse, day, field, scopeName, statusText, type KeyView, type URLs } from "@/routes/keys";

// Settings → Service accounts (admins only): non-human identities <name>@svc.ambit.internal for CI jobs and agents.
// The console has each key's hash only; a new key is shown once, here, and never again.

type Settings = { enabled: boolean; maxDays: number; exposureDays: number; lastUsedAvailable: boolean };
type Account = { name: string; identity: string; group: string; description: string; created: string; createdBy: string;
  keys: KeyView[]; warnings: string[] };
type AccountList = { settings: Settings; accounts: Account[]; urls: URLs };
type Created = { key: string; view: KeyView; urls: URLs };

const DOMAIN = "@svc.ambit.internal";
const NAME_RE = /^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$/;
const GROUP_RE = /^[A-Za-z0-9][A-Za-z0-9._@/-]*$/;

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const useAccounts = () => useQuery({ queryKey: ["service-accounts"], queryFn: () => get<AccountList>("/api/admin/service-accounts") });
const live = (a: Account) => a.keys.filter((k) => k.status !== "stopped");

function NewAccount({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const groups = useGroups();
  const [name, setName] = useState("");
  const [group, setGroup] = useState("");
  const [description, setDescription] = useState("");
  const create = useMutation({
    mutationFn: () => sendJSON("POST", "/api/admin/service-accounts", { name, group: group.trim(), description: description.trim() }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["service-accounts"] }); onClose(); },
  });
  const badName = name !== "" && !NAME_RE.test(name);
  const badGroup = group.trim() !== "" && (!GROUP_RE.test(group.trim()) || ["ambit-admins", "ambit-approvers"].includes(group.trim()));
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label="New service account" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <h2 className="font-semibold">New service account</h2>
      <label className="block"><span className="mb-1 block text-muted">Name</span>
        <input className={field} value={name} maxLength={40} required aria-invalid={badName} aria-describedby="sa-identity"
          onChange={(e) => setName(e.target.value)} placeholder="ci-deploy" /></label>
      <p id="sa-identity" className="text-small text-muted">
        Identity: <span data-testid="sa-identity" className="font-mono">{(name || "<name>") + DOMAIN}</span>. Lowercase letters, digits and -, 1 to 40 characters. Names cannot be reused.
      </p>
      {badName && <p className="text-small text-bad" role="alert">Use lowercase letters, digits and -, starting and ending with a letter or digit.</p>}
      <label className="block"><span className="mb-1 block text-muted">Group</span>
        <input className={field} value={group} list="sa-groups" required aria-invalid={badGroup} onChange={(e) => setGroup(e.target.value)} placeholder="eng-ci" />
        <datalist id="sa-groups">{groups.data?.filter((g) => !["ambit-admins", "ambit-approvers"].includes(g.name)).map((g) => <option key={g.name} value={g.name} />)}</datalist></label>
      <p className="text-small text-muted">The account gets what AccessRules and Budgets give this group, and nothing that is open to everyone signed in.
        A group of its own (for example eng-ci) keeps a runaway job from using up the people's group limits.</p>
      {badGroup && <p className="text-small text-bad" role="alert">Not a usable group: use letters, digits and . _ @ / -; the console role groups ambit-admins and ambit-approvers are not allowed.</p>}
      <label className="block"><span className="mb-1 block text-muted">Description</span>
        <input className={field} value={description} maxLength={200} onChange={(e) => setDescription(e.target.value)} placeholder="GitHub Actions, repository web, release job" /></label>
      {create.isError && <p className="text-bad" role="alert">{create.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={create.isPending || badName || badGroup}>Create</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );
}

// New key, or rotation when the account has one live key: after the new key is shown, offer to revoke the old one.
function NewKey({ account, settings, onClose }: { account: Account; settings: Settings; onClose: () => void }) {
  const qc = useQueryClient();
  const options = [30, 90, 365].filter((d) => d <= settings.maxDays);
  const old = live(account).length === 1 ? live(account)[0] : null;
  const [name, setName] = useState("");
  const [scope, setScope] = useState<"llm" | "mcp" | "both">("llm");
  const [days, setDays] = useState(String(options.includes(90) ? 90 : options[options.length - 1] ?? 30));
  const [created, setCreated] = useState<Created | null>(null); // the only place the plain key lives
  const [oldGone, setOldGone] = useState(false);
  const refresh = () => { qc.invalidateQueries({ queryKey: ["service-accounts"] }); qc.invalidateQueries({ queryKey: ["admin-keys"] }); };
  const create = useMutation({
    gcTime: 0, // the plain key must not stay in the mutation cache after it is dismissed
    mutationFn: () => sendJSON("POST", `/api/admin/service-accounts/${encodeURIComponent(account.name)}/keys`, { name: name.trim(), scope, days: Number(days) }) as Promise<Created>,
    onSuccess: (c) => { setCreated(c); refresh(); },
  });
  const revokeOld = useMutation({
    mutationFn: () => sendJSON("DELETE", `/api/admin/keys/${encodeURIComponent(old!.id)}`),
    onSuccess: () => { setOldGone(true); refresh(); },
  });
  if (created) {
    return (
      <div className="space-y-3 rounded-md border border-line p-4">
        <h3 className="font-semibold">Key ‘{created.view.name}’ created for {account.identity}</h3>
        <p className="font-medium">This is the only time the key is shown. Put it in the CI system's secret store now.</p>
        <CopyBlock text={created.key} testId="new-key" label="Key" />
        <ConnectTool urls={created.urls} keyText={created.key} scope={created.view.scope} tabsFor={ciTabs} title="Use it in a job" pickModel={false} />
        <FirstUse id={created.view.id} admin />
        {old && !oldGone && (
          <div className="space-y-2 rounded-md bg-warn-soft p-3" data-testid="rotate-old">
            <p>When the job uses the new key, revoke the old key ‘{old.name}’ ({old.prefix}). Its last use: {old.lastUsed ? day(old.lastUsed) : settings.lastUsedAvailable ? "not yet" : "unknown"}.</p>
            {revokeOld.isError && <p className="text-bad" role="alert">{revokeOld.error.message}</p>}
            <Button variant="outline" className="w-auto py-1" disabled={revokeOld.isPending} onClick={() => revokeOld.mutate()}>Revoke the old key now</Button>
          </div>
        )}
        {oldGone && <p role="status" className="text-ok">The old key is revoked.</p>}
        <Button className="w-auto" onClick={() => { setCreated(null); create.reset(); onClose(); }}>Done</Button>
      </div>
    );
  }
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label={old ? "Rotate key" : "New key"} onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <h3 className="font-semibold">{old ? "Rotate key: create the new key" : "New key"}</h3>
      <label className="block"><span className="mb-1 block text-muted">Key name</span>
        <input className={field} value={name} maxLength={100} onChange={(e) => setName(e.target.value)} placeholder="gha-2026-12" required /></label>
      <div className="grid grid-cols-2 gap-3">
        <label className="block"><span className="mb-1 block text-muted">Scope</span>
          <select className={field} value={scope} onChange={(e) => setScope(e.target.value as "llm" | "mcp" | "both")}>
            <option value="llm">LLM</option><option value="mcp">MCP</option><option value="both">LLM and MCP</option></select></label>
        <label className="block"><span className="mb-1 block text-muted">End date</span>
          <select className={field} value={days} onChange={(e) => setDays(e.target.value)}>
            {options.map((d) => <option key={d} value={d}>{d} days from now</option>)}</select></label>
      </div>
      {create.isError && <p className="text-bad" role="alert">{create.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={create.isPending}>Create key</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );
}

function AccountCard({ a, settings }: { a: Account; settings: Settings }) {
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [confirmKey, setConfirmKey] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const refresh = () => { qc.invalidateQueries({ queryKey: ["service-accounts"] }); qc.invalidateQueries({ queryKey: ["admin-keys"] }); qc.invalidateQueries({ queryKey: ["admin-key-events"] }); };
  const revoke = useMutation({
    mutationFn: (id: string) => sendJSON("DELETE", `/api/admin/keys/${encodeURIComponent(id)}`),
    onSuccess: () => { setConfirmKey(null); refresh(); },
  });
  // 503 "try again": the account is deleted but a key delete failed; one retry after the Retry-After time
  const remove = useMutation({
    mutationFn: async () => {
      try { return await sendJSON("DELETE", `/api/admin/service-accounts/${encodeURIComponent(a.name)}`); }
      catch (e) {
        if (!(e instanceof Error) || !/try again/.test(e.message)) throw e;
        await new Promise((r) => setTimeout(r, 15_000));
        return sendJSON("DELETE", `/api/admin/service-accounts/${encodeURIComponent(a.name)}`);
      }
    },
    onSuccess: refresh,
  });
  const n = live(a).length;
  return (
    <section aria-label={`Service account ${a.identity}`} data-testid={`sa-${a.name}`} className="space-y-3 border-b border-line py-4">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h2 className="font-mono font-medium">{a.identity}</h2>
        <span className="text-muted">group <span className="font-mono text-ink">{a.group}</span> · created {day(a.created)} by {a.createdBy}</span>
      </div>
      {a.description && <p>{a.description}</p>}
      {a.warnings.map((w) => <p key={w} role="status" className="rounded-md bg-warn-soft px-2 py-1 text-warn">! {w}</p>)}
      <div className="flex items-center justify-between">
        <h3 className="text-muted">Keys</h3>
        {settings.enabled && !adding && n < 2 && (
          <Button variant="outline" className="w-auto py-1" onClick={() => setAdding(true)}>{n === 1 ? "Rotate key" : "New key"}</Button>
        )}
      </div>
      {adding && <NewKey account={a} settings={settings} onClose={() => setAdding(false)} />}
      {revoke.isError && <p className="text-bad" role="alert">{revoke.error.message}</p>}
      {a.keys.length === 0 ? <p className="text-muted">No keys.</p> : (
        <table className="w-full border-collapse text-left">
          <thead><tr className="border-b border-line text-muted"><th className="py-1.5 font-normal">Starts with</th><th className="font-normal">Name</th>
            <th className="font-normal">Scope</th><th className="font-normal">Status</th><th className="font-normal">Stops on</th><th className="font-normal">Last used</th>
            <th className="font-normal">Issued by</th><th /></tr></thead>
          <tbody>
            {a.keys.map((k) => (
              <tr key={k.id} className="border-b border-line-soft">
                <td className="py-1.5 font-mono text-small">{k.prefix}</td><td>{k.name}</td><td>{scopeName[k.scope]}</td><td>{statusText(k)}</td>
                <td>{day(k.until)}</td><td>{k.lastUsed ? day(k.lastUsed) : settings.lastUsedAvailable ? "not yet" : "unknown"}</td><td>{k.issuedBy}</td>
                <td className="text-right">
                  {confirmKey === k.id ? (
                    <span className="inline-flex gap-2" role="alert">
                      <Button className="w-auto py-1" disabled={revoke.isPending} onClick={() => revoke.mutate(k.id)}>Revoke key</Button>
                      <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmKey(null)}>Keep it</Button>
                    </span>
                  ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmKey(k.id)}>Revoke</Button>}
                </td>
              </tr>))}
          </tbody>
        </table>
      )}
      <div className="flex justify-end">
        {confirmDelete ? (
          <div role="alertdialog" aria-label="Delete service account" className="space-y-2 rounded-md border border-bad p-3">
            <p>Delete <span className="font-mono">{a.identity}</span> and its {n} live {n === 1 ? "key" : "keys"}? Jobs that use them stop within a second. The name cannot be used again.</p>
            {remove.isError && <p className="text-bad" role="alert">{remove.error.message}</p>}
            <div className="flex gap-2">
              <Button className="w-auto py-1" disabled={remove.isPending} onClick={() => remove.mutate()}>Delete service account</Button>
              <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmDelete(false)}>Keep it</Button>
            </div>
          </div>
        ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmDelete(true)}>Delete service account</Button>}
      </div>
    </section>
  );
}

export function ServiceAccountsPage() {
  const q = useAccounts();
  const config = useConfig();
  const [creating, setCreating] = useState(false);
  const s = q.data?.settings;
  return (
    <Shell crumb="Settings / Service accounts">
      <section className="max-w-5xl space-y-4 p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="space-y-1">
            <h1 className="text-h1 font-semibold tracking-tight">Service accounts</h1>
            <p className="text-muted">Identities for CI jobs and background agents. A service account belongs to one group, has up to 2 keys, and keeps working when a person leaves.
              {" "}<a className="underline" href="https://ambitproject.dev/docs/service-accounts" target="_blank" rel="noreferrer">How service accounts work</a></p>
          </div>
          {s?.enabled && !creating && <Button className="w-auto shrink-0" onClick={() => setCreating(true)}>New service account</Button>}
        </div>
        {s && (
          <div className="space-y-1">
            <dl className="grid grid-cols-[210px_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-muted">Service accounts</dt><dd>{s.enabled ? "On" : "Off"}</dd>
              <dt className="text-muted">Longest end date</dt><dd>{s.maxDays} days</dd>
              <dt className="text-muted">After the issuer is offboarded</dt><dd>{s.exposureDays === 0 ? "the key stops at once" : `the key stops within ${s.exposureDays} days`}</dd>
            </dl>
            <p className="text-small text-muted">Set in the Helm values (serviceAccounts.*)</p>
          </div>
        )}
        {config.data?.mode === "gitops" && <p className="text-muted">Service accounts and their keys are written by the console directly, not through Git. What they may use comes from AccessRules and Budgets, which go through pull requests.</p>}
        {q.data && !s?.enabled && <p className="text-muted">Service accounts are switched off. Existing keys are kept and refused by the gateways; revoking and deleting still work.</p>}
        {creating && <NewAccount onClose={() => setCreating(false)} />}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {q.data?.accounts.length === 0 && <p className="text-muted">No service accounts.</p>}
        <div>{q.data?.accounts.map((a) => <AccountCard key={a.name} a={a} settings={q.data.settings} />)}</div>
      </section>
    </Shell>
  );
}
