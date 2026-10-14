import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { isProposed, sendJSON, useConfig, type PresetView, type PullRequest, type Unfinished } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { Shell, Status } from "@/components/shell";
import { cn } from "@/lib/utils";

type AppView = { name: string; displayName: string; authorizationURL: string; tokenURL: string; revocationURL: string; clientID: string;
  scopes: string[]; ready: string; message: string; gitManaged: boolean; createdBy: string;
  preset?: string; newerPreset?: { version: number; changes: string[] }; bound: boolean; bindTo?: string };
type AppInput = { name: string; displayName: string; authorizationURL: string; tokenURL: string; revocationURL: string; clientID: string; clientSecret: string; scopes: string[] };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-body outline-none focus:border-accent";
const th = "px-3 py-2.5 text-left font-mono text-label font-medium uppercase tracking-[.1em] text-muted";

function AppForm({ callbackURL, onDone }: { callbackURL: string; onDone: () => void }) {
  const qc = useQueryClient();
  const repository = useConfig().data?.repository;
  const [f, setF] = useState({ name: "", displayName: "", authorizationURL: "", tokenURL: "", revocationURL: "", clientID: "", clientSecret: "", scopes: "" });
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value });
  const [pr, setPR] = useState<PullRequest | null>(null);
  const save = useMutation({
    mutationFn: () => sendJSON("POST", "/api/apps", { ...f, scopes: f.scopes.split(/[\s,]+/).filter(Boolean) } satisfies AppInput),
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["apps"] }); qc.invalidateQueries({ queryKey: ["connections"] }); if (isProposed(res)) setPR(res.pullRequest); else onDone(); },
  });
  if (pr) return <PullRequestNotice pr={pr} repository={repository} onDone={onDone} />;
  const input = (k: keyof typeof f, label: string, placeholder: string, extra?: { type?: string; required?: boolean; sans?: boolean }) => (
    <label className="block"><span className="mb-1 block text-muted">{label}</span>
      <input id={`app-${k}`} type={extra?.type ?? "text"} className={cn(field, extra?.sans && "font-sans")} value={f[k]} onChange={set(k)}
        placeholder={placeholder} required={extra?.required ?? true} autoComplete="off" /></label>
  );
  return (
    <form className="space-y-3 rounded-md border border-line p-4" onSubmit={(e) => { e.preventDefault(); save.mutate(); }} aria-label="Add an app">
      <h2 className="font-semibold">Add an app</h2>
      <p className="text-small text-muted">
        First register an OAuth app with the provider (for example GitHub, Atlassian or Salesforce) and give it this callback URL:{" "}
        <span className="break-all font-mono text-ink">{callbackURL}</span>
      </p>
      <div className="grid grid-cols-2 gap-3">
        {input("name", "Name", "github")}
        {input("displayName", "Name users see", "GitHub", { sans: true })}
      </div>
      {input("authorizationURL", "Authorization URL", "https://github.com/login/oauth/authorize")}
      {input("tokenURL", "Token URL", "https://github.com/login/oauth/access_token")}
      {input("revocationURL", "Revocation URL (optional, used when someone is offboarded)", "https://provider.example.com/oauth/revoke", { required: false })}
      <div className="grid grid-cols-2 gap-3">
        {input("clientID", "Client ID", "")}
        {input("clientSecret", "Client secret", "", { type: "password" })}
      </div>
      {input("scopes", "Scopes, separated by spaces", "repo read:org", { required: false })}
      <p className="text-small text-muted">The client secret goes straight into a Kubernetes Secret in the grants namespace. It is not shown again, not written to logs, and never goes to Git.</p>
      {save.isError && <p className="text-bad" role="alert">{save.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={save.isPending}>Add app</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onDone}>Cancel</Button>
      </div>
    </form>
  );
}

// Apps (journey U1): the SaaS apps users connect with their own accounts. Admins add them here;
// an MCP server reached by URL then names the app, and each call carries the caller's own token.
// Offboarding revokes at the provider in its own format (GitHub, Slack, Notion, HubSpot, or RFC 7009);
// see docs/offboarding.md. HubSpot can revoke only the renewal token.
function Revokes({ url }: { url?: string }) {
  if (!url) return <span className="text-warn">No: offboarding only forgets the grant</span>;
  if (url.includes("/oauth/v1/refresh-tokens")) return <>Yes, within 30 minutes (HubSpot ends only the renewal token)</>;
  return <>Yes</>;
}

function PresetGrid({ onCustom }: { onCustom: () => void }) {
  const q = useQuery({ queryKey: ["presets"], queryFn: () => get<PresetView[]>("/api/presets") });
  const [search, setSearch] = useState("");
  const [category, setCategory] = useState("");
  const all = q.data ?? [];
  const categories = [...new Set(all.map((p) => p.category))];
  const shown = all.filter((p) => (!category || p.category === category) && (p.displayName + " " + p.serverHost).toLowerCase().includes(search.toLowerCase()));
  return (
    <div className="space-y-3 rounded-md border border-line p-4">
      <div className="flex flex-wrap items-center gap-2">
        <input className={cn(field, "max-w-xs font-sans")} placeholder="Search apps" aria-label="Search apps" value={search} onChange={(e) => setSearch(e.target.value)} />
        {categories.map((c) => (
          <Button key={c} variant={category === c ? "primary" : "outline"} className="w-auto" aria-pressed={category === c} onClick={() => setCategory(category === c ? "" : c)}>{c}</Button>
        ))}
        <Button variant="outline" className="ml-auto w-auto" onClick={onCustom}>Custom app…</Button>
      </div>
      {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
      <div className="grid grid-cols-3 gap-3">
        {shown.map((p) => (
          <Link key={p.id} to="/settings/apps/add/$id" params={{ id: p.id }} className="block rounded-md border border-line p-3 hover:bg-sunken">
            <span className="block font-medium">{p.displayName}</span>
            <span className="block font-mono text-small text-muted">{p.serverHost}</span>
            <span className="mt-1 flex flex-wrap gap-x-2 text-small text-muted">
              {p.testedOn && <span>tested {p.testedOn}</span>}
              {p.preview && <span className="text-warn">preview</span>}
              {p.requires && <span>needs: {p.requires}</span>}
            </span>
          </Link>
        ))}
      </div>
      {q.data && shown.length === 0 && <p className="text-muted">No preset matches. Use Custom app… to enter the details yourself.</p>}
    </div>
  );
}

function UnfinishedAdditions() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["unfinished"], queryFn: () => get<Unfinished[]>("/api/apps/unfinished") });
  const [confirm, setConfirm] = useState<string | null>(null);
  const done = () => { qc.invalidateQueries({ queryKey: ["unfinished"] }); qc.invalidateQueries({ queryKey: ["apps"] }); };
  const finish = useMutation({ mutationFn: (op: string) => sendJSON("POST", `/api/apps/unfinished/${encodeURIComponent(op)}/finish`, {}), onSuccess: done });
  const remove = useMutation({ mutationFn: (op: string) => sendJSON("DELETE", `/api/apps/unfinished/${encodeURIComponent(op)}`), onSuccess: () => { setConfirm(null); done(); } });
  if (!q.data?.length) return null;
  return (
    <div className="space-y-2 rounded-md border border-line p-4">
      <h2 className="font-semibold">Unfinished additions</h2>
      <p className="text-small text-muted">These apps were only partly added. Finish them, or remove what was created.</p>
      {(finish.isError || remove.isError) && <p className="text-bad" role="alert">{(finish.error ?? remove.error)?.message}</p>}
      {q.data.map((u) => (
        <div key={u.operation} className="flex flex-wrap items-center gap-3" data-testid={`unfinished-${u.operation}`}>
          <span className="font-mono">{u.app}</span>
          <span className="text-muted">missing: {u.missing.join(", ")}</span>
          {u.pullRequest && <a className="underline" href={u.pullRequest.url} target="_blank" rel="noreferrer">pull request #{u.pullRequest.number}</a>}
          <span className="ml-auto inline-flex gap-2">
            {confirm === u.operation ? <>
              <Button className="w-auto" disabled={remove.isPending} onClick={() => remove.mutate(u.operation)}>Remove addition</Button>
              <Button variant="outline" className="w-auto" onClick={() => setConfirm(null)}>Keep it</Button>
            </> : <>
              <Button className="w-auto" disabled={finish.isPending} onClick={() => finish.mutate(u.operation)}>Finish</Button>
              <Button variant="outline" className="w-auto" onClick={() => setConfirm(u.operation)}>Remove</Button>
            </>}
          </span>
        </div>
      ))}
    </div>
  );
}

export function AppsPage() {
  const q = useQuery({ queryKey: ["apps"], queryFn: () => get<{ callbackURL: string; apps: AppView[] }>("/api/apps") });
  const qc = useQueryClient();
  const gitops = useConfig().data?.mode === "gitops";
  const repository = useConfig().data?.repository;
  const [adding, setAdding] = useState(false);
  const [custom, setCustom] = useState(false);
  const close = () => { setAdding(false); setCustom(false); };
  const bind = useMutation({
    mutationFn: (name: string) => sendJSON("PUT", `/api/apps/${encodeURIComponent(name)}/bind`, {}),
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["apps"] }); if (isProposed(res)) setPR(res.pullRequest); },
  });
  const [confirm, setConfirm] = useState<string | null>(null);
  const [pr, setPR] = useState<PullRequest | null>(null);
  const remove = useMutation({
    mutationFn: (name: string) => sendJSON("DELETE", `/api/apps/${encodeURIComponent(name)}`),
    onSuccess: (res) => { setConfirm(null); qc.invalidateQueries({ queryKey: ["apps"] }); if (isProposed(res)) setPR(res.pullRequest); },
  });
  return (
    <Shell crumb="Settings / Apps">
      <section className="max-w-5xl space-y-4 p-5">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-h1 font-semibold tracking-tight">Apps</h1>
            <p className="text-muted">SaaS apps people connect with their own accounts. The agent then acts as that person, never with a shared account.</p>
          </div>
          {!adding && <Button className="w-auto shrink-0" onClick={() => setAdding(true)}>Add an app</Button>}
        </div>
        {adding && !custom && <PresetGrid onCustom={() => setCustom(true)} />}
        {adding && custom && q.data && <AppForm callbackURL={q.data.callbackURL} onDone={close} />}
        <UnfinishedAdditions />
        {bind.isError && <p className="text-bad" role="alert">{bind.error.message}</p>}
        {pr && <PullRequestNotice pr={pr} repository={repository} onDone={() => setPR(null)} />}
        {q.isLoading && <p className="text-muted">Loading…</p>}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {remove.isError && <p className="text-bad" role="alert">{remove.error.message}</p>}
        {q.data && (
          <table className="w-full border-collapse text-body">
            <thead><tr className="border-b border-line"><th className={th}>App</th><th className={th}>Token URL</th><th className={th}>Scopes</th><th className={th}>Revokes on offboarding</th><th className={th}>Status</th><th className={th} /></tr></thead>
            <tbody>
              {q.data.apps.map((a) => (
                <tr key={a.name} className="border-b border-line-soft align-top" data-testid={`app-${a.name}`}>
                  <td className="px-3 py-2.5"><span className="font-medium">{a.displayName}</span><span className="block font-mono text-small text-muted">{a.name}</span>
                    {a.preset && <span className="block text-small text-muted">preset {a.preset}</span>}
                    {a.newerPreset && <span className="block text-small text-warn">A newer preset exists (version {a.newerPreset.version}): {a.newerPreset.changes.join("; ")}</span>}
                    {!a.bound && a.bindTo && <span className="block text-small">Not bound to a server <Button variant="outline" className="ml-1 w-auto" disabled={bind.isPending} onClick={() => bind.mutate(a.name)}>Bind to its server</Button>
                      <span className="mt-1 block break-all font-mono text-muted">{a.bindTo}</span>
                      {gitops && <span className="mt-1 block text-muted">After the change is merged, people who already connected this app must connect it again once.</span>}</span>}</td>
                  <td className="break-all px-3 py-2.5 font-mono text-small">{a.tokenURL}</td>
                  <td className="px-3 py-2.5 font-mono text-small">{a.scopes.join(" ") || <span className="font-sans text-muted">none</span>}</td>
                  <td className="px-3 py-2.5"><Revokes url={a.revocationURL} /></td>
                  <td className="px-3 py-2.5"><Status ready={a.ready} text={a.ready === "Unknown" ? "Added" : undefined} />{a.message && <span className="block text-small text-muted">{a.message}</span>}</td>
                  <td className="px-3 py-2.5 text-right">
                    {a.gitManaged && !gitops ? <span className="text-small text-muted">Managed in Git</span>
                      : confirm === a.name
                        ? <span className="inline-flex flex-wrap justify-end gap-2" role="alert">
                            <span className="text-small">Users can no longer use {a.displayName}.</span>
                            <Button className="w-auto" disabled={remove.isPending} onClick={() => remove.mutate(a.name)}>Remove app</Button>
                            <Button variant="outline" className="w-auto" onClick={() => setConfirm(null)}>Keep it</Button>
                          </span>
                        : <Button variant="outline" className="w-auto" onClick={() => setConfirm(a.name)}>Remove</Button>}
                  </td>
                </tr>
              ))}
              {q.data.apps.length === 0 && <tr><td colSpan={6} className="px-3 py-6 text-muted">No apps yet. Add one, then request an MCP server reached by URL that uses it.</td></tr>}
            </tbody>
          </table>
        )}
      </section>
    </Shell>
  );
}
