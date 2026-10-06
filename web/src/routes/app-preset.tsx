import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { isProposed, sendJSON, useConfig, type EffectiveAccess, type PresetAddInput, type PresetView, type PullRequest, type ServerView } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { Shell } from "@/components/shell";

const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-[13px] outline-none focus:border-accent";
async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
// getRandomValues works on plain HTTP; randomUUID needs a secure context.
const newOperation = () => Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, "0")).join("");

function CopyLine({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-center gap-2">
      <code id="callback-url" className="min-w-0 flex-1 break-all rounded-md border border-line bg-sunken px-2.5 py-1.5 font-mono text-[12.5px]">{text}</code>
      <Button variant="outline" className="w-auto" onClick={() => navigator.clipboard.writeText(text).then(() => setCopied(true), () => {
        const r = document.createRange(); r.selectNodeContents(document.getElementById("callback-url")!); getSelection()?.removeAllRanges(); getSelection()?.addRange(r);
      })}>{copied ? "Copied" : "Copy"}</Button>
    </div>
  );
}

function SetupForm({ p, onAdded }: { p: PresetView; onAdded: (app: string) => void }) {
  const qc = useQueryClient();
  const repository = useConfig().data?.repository;
  const operation = useMemo(newOperation, []); // one per form: a retry reuses it
  const [f, setF] = useState({ name: p.id, displayName: p.displayName, clientID: "", clientSecret: "" });
  const [values, setValues] = useState<Record<string, string>>({});
  const [serverURL, setServerURL] = useState(p.serverURL);
  const [scopes, setScopes] = useState((p.scopes ?? []).join(" "));
  const [pr, setPR] = useState<PullRequest | null>(null);
  const custom = serverURL !== p.serverURL || scopes !== (p.scopes ?? []).join(" ");
  const add = useMutation({
    mutationFn: () => sendJSON("POST", "/api/apps/from-preset", {
      operation, preset: p.id, version: p.version, ...f, values,
      overrides: custom ? { serverURL: serverURL !== p.serverURL ? serverURL : undefined, scopes: scopes.split(/\s+/).filter(Boolean) } : undefined,
    } satisfies PresetAddInput),
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["apps"] }); if (isProposed(res)) setPR(res.pullRequest); else onAdded(f.name); },
  });
  if (pr) return <PullRequestNotice pr={pr} repository={repository} onDone={() => onAdded(f.name)} />;
  const steps = (p.setup ?? "").split(/\n(?=\d+\.\s)/).map((s) => s.replace(/^\d+\.\s*/, ""));
  const callback = p.setup.match(/https?:\/\/\S+\/connections\/callback/)?.[0] ?? "";
  return (
    <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); add.mutate(); }} aria-label={`Add ${p.displayName}`}>
      <section className="space-y-2 rounded-md border border-line p-4">
        <h2 className="font-semibold">At {p.displayName}</h2>
        <ol className="list-decimal space-y-1 pl-5">{steps.map((s, i) => <li key={i}>{s}</li>)}</ol>
        {callback && <CopyLine text={callback} />}
      </section>
      <section className="space-y-3 rounded-md border border-line p-4">
        <h2 className="font-semibold">In Ambit</h2>
        {(p.placeholders ?? []).map((ph) => (
          <label key={ph.name} className="block"><span className="mb-1 block text-muted">{ph.label}</span>
            <input id={`ph-${ph.name}`} className={field} placeholder={ph.example} required value={values[ph.name] ?? ""}
              onChange={(e) => setValues({ ...values, [ph.name]: e.target.value })} /></label>
        ))}
        <div className="grid grid-cols-2 gap-3">
          <label className="block"><span className="mb-1 block text-muted">Name</span>
            <input id="preset-name" className={field} value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} required /></label>
          <label className="block"><span className="mb-1 block text-muted">Name users see</span>
            <input id="preset-display" className={field + " font-sans"} value={f.displayName} onChange={(e) => setF({ ...f, displayName: e.target.value })} required /></label>
          <label className="block"><span className="mb-1 block text-muted">{p.clientIDLabel}</span>
            <input id="preset-client-id" aria-label="Client ID" className={field} value={f.clientID} onChange={(e) => setF({ ...f, clientID: e.target.value })} required /></label>
          <label className="block"><span className="mb-1 block text-muted">{p.clientSecretLabel}</span>
            <input id="preset-client-secret" aria-label="Client secret" type="password" className={field} value={f.clientSecret} onChange={(e) => setF({ ...f, clientSecret: e.target.value })} required /></label>
        </div>
        <details>
          <summary className="cursor-pointer text-accent-text">Advanced: server URL and scopes</summary>
          <div className="mt-3 space-y-3">
            <label className="block"><span className="mb-1 block text-muted">MCP server URL</span>
              <input id="preset-server-url" className={field} value={serverURL} onChange={(e) => setServerURL(e.target.value)} /></label>
            <label className="block"><span className="mb-1 block text-muted">Scopes, separated by spaces</span>
              <input id="preset-scopes" className={field} value={scopes} onChange={(e) => setScopes(e.target.value)} /></label>
            {custom && <p className="text-warn">Changed values make this a custom app: it is no longer the tested preset.</p>}
          </div>
        </details>
        <p className={p.revokes ? "" : "text-warn"}>
          {p.revokes ? `When someone leaves, Ambit revokes their access at ${p.displayName}.`
            : `${p.displayName} offers no way to revoke a user's access. Ambit deletes its copy; access ends when the token expires.`}
        </p>
        <p className="text-[12px] text-muted">The secret goes straight into a Kubernetes Secret. It is never shown again and never goes to Git.</p>
        {add.isError && <p className="text-bad" role="alert">{add.error.message}</p>}
        <Button type="submit" className="w-auto" disabled={add.isPending}>Add {p.displayName}</Button>
      </section>
    </form>
  );
}

function PickTools({ app }: { app: string }) {
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => get<ServerView[]>("/api/servers"), refetchInterval: 3000 });
  const access = useQuery({ queryKey: ["access", app], queryFn: () => get<EffectiveAccess>(`/api/servers/${encodeURIComponent(app)}/access`) });
  const [asked, setAsked] = useState<number | null>(null);
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const [groups, setGroups] = useState("");
  const read = useMutation({
    mutationFn: () => sendJSON("PUT", `/api/servers/${encodeURIComponent(app)}/tool-reader`, {}),
    onSuccess: () => { setAsked(Date.now()); qc.invalidateQueries({ queryKey: ["servers"] }); },
  });
  const grant = useMutation({
    mutationFn: () => sendJSON("POST", "/api/access-rules", { name: `${app}-users`, groups: groups.split(/[\s,]+/).filter(Boolean), allSignedIn: false,
      servers: [{ name: app, tools: Object.keys(checked).filter((k) => checked[k]) }] }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["access", app] }),
  });
  const srv = servers.data?.find((s) => s.name === app);
  const a = access.data;
  const waitedTooLong = asked !== null && Date.now() - asked > 30_000 && !srv?.tools.length;
  return (
    <section className="space-y-3 rounded-md border border-line p-4">
      <p>{!a || (!a.allSignedIn && a.groups.length === 0) ? "Nobody can use its tools yet."
        : `Groups: ${a.groups.join(", ") || "none"} · all signed-in users: ${a.allSignedIn ? "yes" : "no"} · a rule grants all tools: ${a.allTools ? "yes" : "no"}`}</p>
      {a?.allTools && <p className="text-warn">An existing access rule already grants every tool of this server, including tools it adds later.</p>}
      <h2 className="font-semibold">Pick tools</h2>
      <p className="text-muted">Ambit can read this server's tools only with someone's account. Your account is used for that alone, and every admin can see the tool list it returns.</p>
      <div className="flex flex-wrap gap-2">
        <a className="underline" href={`/connections/${encodeURIComponent(app)}/connect`}>Connect your account</a>
        <Button className="w-auto" disabled={read.isPending} onClick={() => read.mutate()}>Use my account to read tools</Button>
      </div>
      {read.isError && <p className="text-bad" role="alert">{read.error.message}</p>}
      {waitedTooLong && <p className="text-muted">Still reading; come back to this page later.</p>}
      {!!srv?.tools.length && (
        <table className="w-full text-[13px]"><tbody>
          {srv.tools.map((t) => (
            <tr key={t.name} className="border-b border-line-soft">
              <td className="px-2 py-1.5"><input type="checkbox" aria-label={t.name} checked={!!checked[t.name]} onChange={(e) => setChecked({ ...checked, [t.name]: e.target.checked })} /></td>
              <td className="px-2 py-1.5 font-mono">{t.name}</td>
              <td className="px-2 py-1.5">{t.description}</td>
              <td className="px-2 py-1.5 text-[12px]">
                {t.destructiveHint && <span className="text-bad">the server says: can delete or change data</span>}
                {t.readOnlyHint && !t.destructiveHint && <span className="text-muted">the server says: read-only</span>}
              </td>
            </tr>
          ))}
        </tbody></table>
      )}
      {!!srv?.tools.length && (
        <div className="flex flex-wrap items-end gap-2">
          <label className="block"><span className="mb-1 block text-muted">Groups</span>
            <input id="grant-groups" className={field} value={groups} onChange={(e) => setGroups(e.target.value)} /></label>
          <Button className="w-auto" disabled={grant.isPending || !Object.values(checked).some(Boolean)} onClick={() => grant.mutate()}>Create access rule</Button>
          <Link to="/settings/apps" className="underline">Skip, do it later</Link>
        </div>
      )}
      {grant.isError && <p className="text-bad" role="alert">{grant.error.message}</p>}
    </section>
  );
}

export function AppPresetPage() {
  const { id } = useParams({ strict: false }) as { id: string };
  const search = useSearch({ strict: false }) as { app?: string; step?: string };
  const q = useQuery({ queryKey: ["presets"], queryFn: () => get<PresetView[]>("/api/presets") });
  const [added, setAdded] = useState<string | null>(search.step === "tools" && search.app ? search.app : null);
  const p = q.data?.find((x) => x.id === id);
  return (
    <Shell crumb={`Settings / Apps / ${p?.displayName ?? id}`}>
      <section className="max-w-4xl space-y-4 p-5">
        <h1 className="text-xl font-semibold tracking-tight">{p?.displayName ?? id}</h1>
        {p && <p className="text-muted">Preset {p.id}, version {p.version}{p.testedOn ? ` · tested ${p.testedOn}` : ""}{p.preview ? " · preview" : ""}
          {p.requires ? ` · needs: ${p.requires}` : ""} · <a className="underline" href={p.docs} target="_blank" rel="noreferrer">vendor guide</a></p>}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {p && !added && <SetupForm p={p} onAdded={setAdded} />}
        {added && <PickTools app={added} />}
      </section>
    </Shell>
  );
}
