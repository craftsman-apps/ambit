import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { isProposed, sendJSON, useConfig, type PullRequest } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { Shell, Status } from "@/components/shell";
import { cn } from "@/lib/utils";

type AppView = { name: string; displayName: string; authorizationURL: string; tokenURL: string; revocationURL: string; clientID: string;
  scopes: string[]; ready: string; message: string; gitManaged: boolean; createdBy: string };
type AppInput = { name: string; displayName: string; authorizationURL: string; tokenURL: string; revocationURL: string; clientID: string; clientSecret: string; scopes: string[] };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-[13px] outline-none focus:border-accent";
const th = "px-3 py-2.5 text-left font-mono text-[10.5px] font-medium uppercase tracking-[.1em] text-muted";

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
      <p className="text-[12.5px] text-muted">
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
      <p className="text-[12px] text-muted">The client secret goes straight into a Kubernetes Secret in the grants namespace. It is not shown again, not written to logs, and never goes to Git.</p>
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
export function AppsPage() {
  const q = useQuery({ queryKey: ["apps"], queryFn: () => get<{ callbackURL: string; apps: AppView[] }>("/api/apps") });
  const qc = useQueryClient();
  const gitops = useConfig().data?.mode === "gitops";
  const repository = useConfig().data?.repository;
  const [adding, setAdding] = useState(false);
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
            <h1 className="text-xl font-semibold tracking-tight">Apps</h1>
            <p className="text-muted">SaaS apps people connect with their own accounts. The agent then acts as that person, never with a shared account.</p>
          </div>
          {!adding && <Button className="w-auto shrink-0" onClick={() => setAdding(true)}>Add an app</Button>}
        </div>
        {adding && q.data && <AppForm callbackURL={q.data.callbackURL} onDone={() => setAdding(false)} />}
        {pr && <PullRequestNotice pr={pr} repository={repository} onDone={() => setPR(null)} />}
        {q.isLoading && <p className="text-muted">Loading…</p>}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {remove.isError && <p className="text-bad" role="alert">{remove.error.message}</p>}
        {q.data && (
          <table className="w-full border-collapse text-[13px]">
            <thead><tr className="border-b border-line"><th className={th}>App</th><th className={th}>Token URL</th><th className={th}>Scopes</th><th className={th}>Revokes on offboarding</th><th className={th}>Status</th><th className={th} /></tr></thead>
            <tbody>
              {q.data.apps.map((a) => (
                <tr key={a.name} className="border-b border-line-soft align-top" data-testid={`app-${a.name}`}>
                  <td className="px-3 py-2.5"><span className="font-medium">{a.displayName}</span><span className="block font-mono text-[12px] text-muted">{a.name}</span></td>
                  <td className="break-all px-3 py-2.5 font-mono text-[12px]">{a.tokenURL}</td>
                  <td className="px-3 py-2.5 font-mono text-[12px]">{a.scopes.join(" ") || <span className="font-sans text-muted">none</span>}</td>
                  <td className="px-3 py-2.5">{a.revocationURL ? "Yes" : <span className="text-warn">No: offboarding only forgets the grant</span>}</td>
                  <td className="px-3 py-2.5"><Status ready={a.ready} text={a.ready === "Unknown" ? "Added" : undefined} />{a.message && <span className="block text-[12px] text-muted">{a.message}</span>}</td>
                  <td className="px-3 py-2.5 text-right">
                    {a.gitManaged && !gitops ? <span className="text-[12px] text-muted">Managed in Git</span>
                      : confirm === a.name
                        ? <span className="inline-flex flex-wrap justify-end gap-2" role="alert">
                            <span className="text-[12.5px]">Users can no longer use {a.displayName}.</span>
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
