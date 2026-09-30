import { useMemo, useState } from "react";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createColumnHelper, createSortedRowModel, rowSortingFeature, sortFn_text, tableFeatures, useTable } from "@tanstack/react-table";
import { isProposed, sendJSON, useChanges, useConfig, useMe, useModels, useRule, useRules, useServers, type PullRequest, type RuleInput, type RuleView, type ServerAccess } from "@/api";
import { PullRequestNotice } from "@/components/pull-request";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";
import { cn } from "@/lib/utils";

// TanStack Table v9: features are declared up front; only sorting is used here.
const features = tableFeatures({ rowSortingFeature, sortedRowModel: createSortedRowModel(), sortFns: { text: sortFn_text } });
const col = createColumnHelper<typeof features, RuleView>();
const who = (r: RuleView) => (r.allSignedIn ? "Everyone signed in" : r.groups.join(", "));
// "time: get_current_time" or "everything: all tools"
const serverText = (sa: ServerAccess) => `${sa.name}: ${sa.tools?.length ? sa.tools.join(", ") : "all tools"}`;
const canUse = (r: RuleView) => [...r.models, ...r.servers.map(serverText)].join("; ");
// A client works best with about 40 tools or fewer (journey U5).
const TOOL_BUDGET = 40;

export function AccessRulesPage() {
  const { rule: selected, edit } = useSearch({ from: "/access-rules" });
  const navigate = useNavigate({ from: "/access-rules" });
  const me = useMe();
  const rules = useRules();
  const config = useConfig();
  const gitops = config.data?.mode === "gitops";
  const changes = useChanges(gitops);
  const [filter, setFilter] = useState<"all" | "attention">("all");
  const isAdmin = me.data?.role === "admin";

  const columns = useMemo(() => col.columns([
    col.accessor("name", { header: "Rule", cell: (c) => (
      <><span className="whitespace-nowrap font-mono">{c.getValue()}</span>
        {c.row.original.ready === "False" && <span className="block text-[12px] text-muted">{c.row.original.message}</span>}</>) }),
    col.accessor(who, { id: "who", header: "Who" }),
    col.accessor(canUse, { id: "models", header: "Can use" }),
    col.accessor((r) => (r.gitManaged ? "Git" : r.createdBy ? "Console" : "Cluster"), { id: "source", header: "Source",
      cell: (c) => <span className="font-mono text-[12.5px]">{c.getValue()}</span> }),
    col.accessor("ready", { header: "Status", cell: (c) => <Status ready={c.getValue()} /> }),
  ]), []);
  const data = useMemo(() => (rules.data ?? []).filter((r) => filter === "all" || r.ready !== "True"), [rules.data, filter]);
  const table = useTable({ features, columns, data }, (state) => state);
  const attention = (rules.data ?? []).filter((r) => r.ready !== "True").length;

  return (
    <Shell crumb="Govern / Access rules">
      <div className="grid min-h-full grid-cols-[1fr_380px]">
        <section className="min-w-0 p-5">
          <div className="mb-4 flex items-end justify-between gap-3">
            <div>
              <h1 className="text-xl font-semibold tracking-tight">Access rules</h1>
              <p className="text-muted">Who may use which models and MCP tools. Anything no rule names is closed to everyone.</p>
              {gitops && <p className="text-[12.5px] text-muted">GitOps mode: changes open pull requests in <span className="font-mono">{config.data?.repository}</span>.</p>}
            </div>
            {isAdmin && <Button className="w-auto" onClick={() => navigate({ search: { edit: "new" } })}>New rule</Button>}
          </div>
          <div className="mb-3 flex gap-1.5 text-[12.5px]">
            {([["all", `All ${rules.data?.length ?? 0}`], ["attention", `Needs attention ${attention}`]] as const).map(([k, label]) => (
              <button key={k} type="button" onClick={() => setFilter(k)} aria-pressed={filter === k}
                className={cn("rounded-md border px-2.5 py-0.5", filter === k ? "border-ink text-ink" : "border-line text-muted")}>{label}</button>
            ))}
          </div>
          {gitops && !!changes.data?.length && (
            <div className="mb-4 rounded-md border border-line p-3" data-testid="pending-changes">
              <p className="mb-1 font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Waiting for review in Git</p>
              {changes.data.map((pr) => (
                <p key={pr.number}><a className="text-accent-text underline" href={pr.url} target="_blank" rel="noopener noreferrer">#{pr.number}</a> {pr.title}
                  {pr.author && <span className="text-[12px] text-muted"> · by {pr.author}</span>}</p>
              ))}
            </div>
          )}
          {changes.isError && <p className="mb-3 text-bad">{changes.error.message}</p>}
          {rules.isError && <p className="text-bad">{rules.error.message}</p>}
          <table className="w-full border-collapse">
            <thead>
              {table.getHeaderGroups().map((hg) => (
                <tr key={hg.id} className="border-b border-line">
                  {hg.headers.map((h) => (
                    <th key={h.id} className="px-3 py-2.5 text-left font-mono text-[10.5px] font-medium uppercase tracking-[.1em] text-muted">
                      <button type="button" onClick={h.column.getToggleSortingHandler()} className="uppercase tracking-[.1em]">
                        <table.FlexRender header={h} />
                        {{ asc: " ↑", desc: " ↓" }[h.column.getIsSorted() as string] ?? ""}
                      </button>
                    </th>
                  ))}
                </tr>
              ))}
            </thead>
            <tbody>
              {table.getRowModel().rows.map((row) => (
                <tr key={row.id} onClick={() => navigate({ search: { rule: row.original.name } })}
                  className={cn("cursor-pointer border-b border-line-soft align-top hover:bg-sunken",
                    selected === row.original.name && "bg-selected shadow-[inset_2px_0_0_var(--color-accent)]")}>
                  {row.getAllCells().map((c) => <td key={c.id} className="px-3 py-2.5"><table.FlexRender cell={c} /></td>)}
                </tr>
              ))}
              {rules.data?.length === 0 && (
                <tr><td colSpan={5} className="px-3 py-6 text-muted">No access rules yet, so no model can be used. {isAdmin && "Create one with New rule."}</td></tr>
              )}
            </tbody>
          </table>
        </section>
        <aside className="border-l border-line bg-sunken p-5">
          {edit ? <RuleForm name={edit === "new" ? undefined : edit} repository={config.data?.repository} onDone={(n) => navigate({ search: n ? { rule: n } : {} })} />
            : selected ? <RuleDetailPanel name={selected} isAdmin={isAdmin} gitops={gitops} repository={config.data?.repository}
                onEdit={() => navigate({ search: { edit: selected } })} onDeleted={() => navigate({ search: {} })} />
            : <p className="text-muted">Select a rule to see who it lets in and the policy the gateway enforces.</p>}
        </aside>
      </div>
    </Shell>
  );
}

function RuleDetailPanel({ name, isAdmin, gitops, repository, onEdit, onDeleted }:
  { name: string; isAdmin: boolean; gitops: boolean; repository?: string; onEdit: () => void; onDeleted: () => void }) {
  const q = useRule(name);
  const qc = useQueryClient();
  const [confirm, setConfirm] = useState(false);
  const [pr, setPR] = useState<PullRequest | null>(null);
  const del = useMutation({
    mutationFn: () => sendJSON("DELETE", `/api/access-rules/${encodeURIComponent(name)}`),
    onSuccess: (res) => { qc.invalidateQueries(); if (isProposed(res)) setPR(res.pullRequest); else onDeleted(); },
  });
  if (q.isPending) return null;
  if (q.isError) return <p className="text-bad">{q.error.message}</p>;
  const r = q.data;
  return (
    <div className="space-y-5">
      <div>
        <h2 className="font-mono text-[15px] font-medium">{r.name}</h2>
        <p className="text-[12.5px] text-muted">AccessRule · {r.gitManaged ? "managed in Git" : r.createdBy ? `created in the console by ${r.createdBy}` : "applied directly to the cluster (for example with kubectl)"}</p>
      </div>
      <dl className="grid grid-cols-[76px_1fr] gap-x-3 gap-y-2">
        <dt className="text-muted">Who</dt><dd>{r.allSignedIn ? "Everyone with a valid token" : <>Members of <b>{r.groups.join(", ")}</b></>}</dd>
        <dt className="text-muted">Can use</dt>
        <dd>{r.models.join(", ")}{r.servers.map((sa) => <span key={sa.name} className="block">MCP {serverText(sa)}</span>)}</dd>
        <dt className="text-muted">Status</dt><dd><Status ready={r.ready} /><span className="block text-[12.5px] text-muted">{r.message}</span></dd>
      </dl>
      <div>
        <p className="mb-2 font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Policy the gateway enforces</p>
        {r.effective.map((e) => (
          <div key={e.model} className="mb-2">
            <p className="mb-1 text-[12.5px] text-muted">{e.model} · <span className="font-mono">{e.modelName}</span></p>
            <pre className="whitespace-pre-wrap break-all rounded-md border border-line-soft bg-surface p-3 font-mono text-[12.5px] leading-relaxed text-accent-text">{e.cel}</pre>
          </div>
        ))}
        {r.effectiveServers.map((e) => (
          <div key={e.server} className="mb-2">
            <p className="mb-1 text-[12.5px] text-muted">MCP server {e.server}</p>
            <pre className="whitespace-pre-wrap break-all rounded-md border border-line-soft bg-surface p-3 font-mono text-[12.5px] leading-relaxed text-accent-text">{e.cel}</pre>
          </div>
        ))}
        <p className="text-[12px] text-muted">Merged from every rule that names the model or server.</p>
      </div>
      {pr && <PullRequestNotice pr={pr} repository={repository} onDone={() => { setPR(null); setConfirm(false); }} />}
      {isAdmin && (!r.gitManaged || gitops) && !pr && (
        <div className="flex gap-2">
          <Button variant="outline" className="w-auto" onClick={onEdit}>Edit</Button>
          {confirm
            ? <Button className="w-auto bg-bad! border-bad!" onClick={() => del.mutate()} disabled={del.isPending}>Delete {r.name}</Button>
            : <Button variant="outline" className="w-auto" onClick={() => setConfirm(true)}>Delete</Button>}
        </div>
      )}
      {r.gitManaged && !gitops && <p className="text-[12.5px] text-muted">Change this rule with a pull request to your configuration repository.</p>}
      {del.isError && <p className="text-bad">{del.error.message}</p>}
    </div>
  );
}

function RuleForm({ name, repository, onDone }: { name?: string; repository?: string; onDone: (name?: string) => void }) {
  const [pr, setPR] = useState<PullRequest | null>(null);
  const models = useModels();
  const servers = useServers();
  const existing = useRule(name);
  const qc = useQueryClient();
  const init = existing.data;
  const [ruleName, setRuleName] = useState("");
  const [everyone, setEveryone] = useState<boolean | null>(null);
  const [groups, setGroups] = useState<string | null>(null);
  const [picked, setPicked] = useState<string[] | null>(null);
  const allSignedIn = everyone ?? init?.allSignedIn ?? false;
  const groupText = groups ?? init?.groups.join(", ") ?? "";
  const chosen = picked ?? init?.models ?? [];
  const [pickedServers, setPickedServers] = useState<ServerAccess[] | null>(null);
  const chosenServers = pickedServers ?? init?.servers ?? [];
  const setServer = (name: string, sa: ServerAccess | null) =>
    setPickedServers([...chosenServers.filter((x) => x.name !== name), ...(sa ? [sa] : [])].sort((a, b) => a.name.localeCompare(b.name)));
  const toolCount = chosenServers.reduce((n, sa) =>
    n + (sa.tools?.length ?? servers.data?.find((x) => x.name === sa.name)?.tools.length ?? 0), 0);
  const save = useMutation({
    mutationFn: (body: RuleInput) =>
      name ? sendJSON("PUT", `/api/access-rules/${encodeURIComponent(name)}`, body) : sendJSON("POST", "/api/access-rules", body),
    onSuccess: (res) => { qc.invalidateQueries(); if (isProposed(res)) setPR(res.pullRequest); else onDone(name ?? ruleName); },
  });
  if (name && existing.isPending) return null;
  if (pr) return <PullRequestNotice pr={pr} repository={repository} onDone={() => onDone(name)} />;
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    save.mutate({ ...(name ? {} : { name: ruleName }), allSignedIn,
      groups: allSignedIn ? [] : groupText.split(",").map((g) => g.trim()).filter(Boolean), models: chosen, servers: chosenServers });
  };
  const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-[13px] outline-none focus:border-accent";
  return (
    <form onSubmit={submit} className="space-y-4">
      <h2 className="text-[15px] font-semibold">{name ? `Edit ${name}` : "New access rule"}</h2>
      {!name && (
        <label className="block"><span className="mb-1 block text-muted">Name</span>
          <input id="rule-name" className={field} value={ruleName} onChange={(e) => setRuleName(e.target.value)} placeholder="finance-payments" required /></label>
      )}
      <fieldset className="space-y-1.5">
        <legend className="mb-1 text-muted">Who</legend>
        <label className="flex items-center gap-2"><input id="who-groups" type="radio" checked={!allSignedIn} onChange={() => setEveryone(false)} /> Members of groups</label>
        {!allSignedIn && <input id="rule-groups" aria-label="Group names, separated by commas" className={field} value={groupText} onChange={(e) => setGroups(e.target.value)} placeholder="ml-research, finance" />}
        <label className="flex items-center gap-2"><input id="who-everyone" type="radio" checked={allSignedIn} onChange={() => setEveryone(true)} /> Everyone signed in</label>
      </fieldset>
      <fieldset>
        <legend className="mb-1 text-muted">Can use</legend>
        {models.data?.map((m) => (
          <label key={m.name} className="flex items-center gap-2 py-0.5">
            <input id={`model-${m.name}`} type="checkbox" checked={chosen.includes(m.name)}
              onChange={(e) => setPicked(e.target.checked ? [...chosen, m.name] : chosen.filter((x) => x !== m.name))} />
            <span className="whitespace-nowrap font-mono">{m.name}</span> <span className="text-[12px] text-muted">{m.model}</span>
          </label>
        ))}
      </fieldset>
      <fieldset>
        <legend className="mb-1 text-muted">MCP tools</legend>
        {servers.data?.length === 0 && <p className="text-[12.5px] text-muted">No MCP servers yet.</p>}
        {servers.data?.map((srv) => {
          const sa = chosenServers.find((x) => x.name === srv.name);
          return (
            <div key={srv.name} className="py-0.5">
              <label className="flex items-center gap-2">
                <input id={`server-${srv.name}`} type="checkbox" checked={!!sa} onChange={(e) => setServer(srv.name, e.target.checked ? { name: srv.name } : null)} />
                <span className="font-mono">{srv.name}</span> <span className="text-[12px] text-muted">{srv.tools.length} tools</span>
              </label>
              {sa && (
                <div className="ml-6 mt-1 space-y-0.5 text-[13px]">
                  <label className="flex items-center gap-2"><input id={`server-${srv.name}-all`} type="radio" checked={!sa.tools}
                    onChange={() => setServer(srv.name, { name: srv.name })} /> All tools, including ones it adds later</label>
                  <label className="flex items-center gap-2"><input id={`server-${srv.name}-some`} type="radio" checked={!!sa.tools}
                    onChange={() => setServer(srv.name, { name: srv.name, tools: [] })} /> Only these tools</label>
                  {sa.tools && srv.tools.map((t) => (
                    <label key={t.name} className="ml-6 flex items-center gap-2">
                      <input id={`tool-${srv.name}-${t.name}`} type="checkbox" checked={sa.tools!.includes(t.name)}
                        onChange={(e) => setServer(srv.name, { name: srv.name, tools: e.target.checked ? [...sa.tools!, t.name] : sa.tools!.filter((x) => x !== t.name) })} />
                      <span className="font-mono text-[12.5px]">{t.name}</span>
                    </label>
                  ))}
                  {sa.tools && srv.tools.length === 0 && <p className="ml-6 text-muted">Tools appear once the server is running.</p>}
                </div>
              )}
            </div>
          );
        })}
        {toolCount > TOOL_BUDGET && (
          <p className="mt-1 rounded-md border border-warn bg-warn-soft p-2 text-[12.5px]">This rule grants {toolCount} tools. Agents choose tools less reliably above about {TOOL_BUDGET}; consider only the tools this group needs.</p>
        )}
      </fieldset>
      {save.isError && <p className="rounded-md border border-bad p-2 text-bad">{save.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={save.isPending}>{name ? "Save changes" : "Create rule"}</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={() => onDone(name)}>Cancel</Button>
      </div>
      <p className="text-[12px] text-muted">{repository ? "Saving opens a pull request; the change applies once it is merged." : "The change reaches the gateway within seconds."}</p>
    </form>
  );
}
