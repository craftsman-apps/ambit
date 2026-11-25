import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { getJSON, sendJSON, useConnections, useGroups, useMe, useServers, type ServerView } from "@/api";
import { Button } from "@/components/ui/button";
import { Shell } from "@/components/shell";
import { CopyBlock, FirstUse, day, field, statusText, type KeySettings, type KeyView } from "@/routes/keys";
import {
  headerLines, knownPlatforms, lastUseByAgent, modeText, NAME_RE, parseRanges, PLATFORM_RE, platformText, scopeSummary, stateText, stopNotes, toggleName, toggleScope, toolsUsed, utcTime,
  type AgentSettings, type AgentView,
} from "@/lib/agents";

// Agents (docs/agents.md). Experimental in 0.6.x: the agent API and these screens may change in 0.7.0.
// A person makes agents for themselves; an admin also makes itself-mode agents (service accounts with an owner).
// The console has each key's hash only: a new key is shown once, here, and never again.

type AgentList = { agents: AgentView[]; settings: AgentSettings };
type AgentDetailData = AgentView & { keys?: KeyView[] };
type KeyList = { settings: KeySettings; keys: KeyView[] };
type CreatedKey = { key: string; view: KeyView; urls: { mcp: string } };
type AuditCall = { time: string; method: string; server: string; tool: string; status: string };

const agentsKey = ["agents"] as const;
const useAgents = (admin: boolean) => useQuery({ queryKey: [...agentsKey, "list", admin], refetchInterval: (q) => (q.state.data?.agents.some((a) => a.state === "stopping") ? 5_000 : 60_000),
  queryFn: () => getJSON<AgentList>(admin ? "/api/agents?all=true" : "/api/agents") });
// The keys of the agents in the list: their last use, in one request (a person's own keys; an admin's all keys).
const useKeyUse = (admin: boolean) => useQuery({ queryKey: [...agentsKey, "keys", admin],
  queryFn: () => getJSON<KeyList>(admin ? "/api/admin/keys" : "/api/keys") });
// While a stop is not confirmed, ask again every 2 seconds: the console reports Stopped only after every gateway confirmed.
const useAgent = (name?: string) => useQuery({ queryKey: [...agentsKey, "one", name], enabled: !!name, retry: false,
  refetchInterval: (q) => (q.state.data?.state === "stopping" ? 2_000 : false),
  queryFn: () => getJSON<AgentDetailData>(`/api/agents/${encodeURIComponent(name!)}`) });

const th = "py-1.5 pr-3 font-normal";
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
export const Experimental = () => <span className="ml-2 rounded border border-line px-1 align-middle font-sans text-small font-normal text-muted"
  title="The agent API and these screens may change in 0.7.0, with a note in Upgrading. They do not change inside 0.6.x without a note.">Experimental</span>;

// The tools an agent may use: a whole server, or single tools. Empty means no tool.
function ScopePicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const servers = useServers();
  const list: ServerView[] = servers.data ?? [];
  const known = new Set(list.flatMap((s) => [s.name, ...s.tools.map((t) => `${s.name}/${t.name}`)]));
  const unknown = value.filter((e) => !known.has(e)); // kept, so saving does not drop an entry the list cannot show
  return (
    <fieldset className="space-y-2" aria-label="Tools the agent may use">
      <legend className="mb-1 text-muted">Tools it may use</legend>
      <p className="text-small text-muted">The agent can use only what AccessRules allow for it (for an itself-mode agent, its service account's group) <strong>and</strong> this list names. In audit-only mode the AccessRules are not enforced, but this list is. Nothing ticked means no tool. A scope never gives more than the owner has.</p>
      {servers.isError && <p className="text-bad" role="alert">{servers.error.message}</p>}
      {servers.data && list.length === 0 && <p className="text-muted">There are no MCP servers yet.</p>}
      <div className="space-y-1">
        {list.map((s) => {
          const whole = value.includes(s.name);
          const picked = value.filter((e) => e.startsWith(`${s.name}/`)).length;
          return (
            <details key={s.name} open={picked > 0} className="rounded-md border border-line-soft px-2.5 py-1.5">
              <summary className="flex cursor-pointer items-center gap-2">
                <input type="checkbox" aria-label={`All tools of ${s.name}`} checked={whole} onClick={(e) => e.stopPropagation()} onChange={() => onChange(toggleScope(value, s.name))} />
                <span className="font-mono">{s.name}</span>
                <span className="text-small text-muted">{whole ? "all tools" : picked ? plural(picked, "tool") : ""}</span>
              </summary>
              <div className="ml-6 mt-1 space-y-0.5">
                {s.tools.length === 0 && <p className="text-small text-muted">No tools are known yet. Tick the server to allow all of its tools.</p>}
                {s.tools.map((t) => (
                  <label key={t.name} className="flex items-center gap-2">
                    <input type="checkbox" disabled={whole} checked={whole || value.includes(`${s.name}/${t.name}`)} onChange={() => onChange(toggleScope(value, `${s.name}/${t.name}`))} />
                    <span className="font-mono text-small">{t.name}</span>
                  </label>))}
              </div>
            </details>);
        })}
      </div>
      {unknown.length > 0 && <p className="text-small text-muted">Also in the scope, not in the list above: <span className="font-mono">{unknown.join(", ")}</span>.{" "}
        <button type="button" className="underline" onClick={() => onChange(value.filter((e) => known.has(e)))}>Remove them</button></p>}
    </fieldset>
  );
}

// The platform the agent says it is. It is never checked: no rule, limit or bill uses it.
function PlatformField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const isKnown = knownPlatforms.some(([v]) => v === value);
  const [custom, setCustom] = useState(!isKnown);
  const bad = value !== "" && !PLATFORM_RE.test(value);
  return (
    <div className="space-y-1">
      <label className="block"><span className="mb-1 block text-muted">Declared platform</span>
        <select className={field} value={custom ? "__custom" : value} onChange={(e) => { if (e.target.value === "__custom") { setCustom(true); onChange(""); } else { setCustom(false); onChange(e.target.value); } }}>
          {knownPlatforms.map(([v, t]) => <option key={v} value={v}>{t}</option>)}
          <option value="__custom">Another platform…</option>
        </select></label>
      {custom && <label className="block"><span className="mb-1 block text-muted">Platform name</span>
        <input className={field} value={value} maxLength={40} aria-invalid={bad} onChange={(e) => onChange(e.target.value)} placeholder="my-platform" required /></label>}
      {bad && <p className="text-small text-bad" role="alert">Use lower-case letters, digits and . _ - (at most 40 characters), starting with a letter or digit.</p>}
      <p className="text-small text-muted">What the owner says the agent is. Ambit does not check it, and no rule, limit or bill uses it. Audit shows it as “declared”.</p>
    </div>
  );
}

function NewAgent({ admin, settings, onClose }: { admin: boolean; settings: AgentSettings; onClose: (created?: string) => void }) {
  const qc = useQueryClient();
  const groups = useGroups(admin);
  const [mode, setMode] = useState<"person" | "itself">("person");
  const [displayName, setDisplayName] = useState("");
  const [platform, setPlatform] = useState("other");
  const [scope, setScope] = useState<string[]>([]);
  const [name, setName] = useState("");
  const [group, setGroup] = useState("");
  const [owner, setOwner] = useState("");
  const itself = mode === "itself";
  const create = useMutation({
    mutationFn: () => sendJSON("POST", "/api/agents", itself
      ? { mode, name, displayName: displayName.trim(), platform, scope, group: group.trim(), owner: owner.trim() }
      : { displayName: displayName.trim(), platform, scope }) as Promise<AgentView>,
    onSuccess: (a) => { qc.invalidateQueries({ queryKey: agentsKey }); onClose(a.name); },
  });
  const badName = itself && name !== "" && !NAME_RE.test(name);
  const noPlatform = !PLATFORM_RE.test(platform);
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label="New agent" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <h2 className="font-semibold">New agent</h2>
      {admin && (
        <fieldset className="space-y-1" aria-label="Kind of agent">
          <legend className="mb-1 text-muted">Kind</legend>
          <label className="flex items-start gap-2"><input type="radio" name="agent-mode" className="mt-1" checked={!itself} onChange={() => setMode("person")} />
            <span><strong>Person.</strong> An agent for you. It acts as you, with your access.</span></label>
          <label className="flex items-start gap-2"><input type="radio" name="agent-mode" className="mt-1" checked={itself} onChange={() => setMode("itself")} />
            <span><strong>Itself.</strong> A shared agent that acts as a service account, with an owner who is accountable for it. Only admins make these.</span></label>
        </fieldset>
      )}
      <label className="block"><span className="mb-1 block text-muted">Name</span>
        <input className={field} value={displayName} maxLength={100} onChange={(e) => setDisplayName(e.target.value)} placeholder="Grok Bot for sales" required /></label>
      <PlatformField value={platform} onChange={setPlatform} />
      {itself && (
        <div className="space-y-3">
          <label className="block"><span className="mb-1 block text-muted">Account name</span>
            <input className={field} value={name} maxLength={40} required aria-invalid={badName} onChange={(e) => setName(e.target.value)} placeholder="nightly-report" /></label>
          <p className="text-small text-muted">Calls are recorded as <span className="font-mono">{(name || "<name>") + "@svc.ambit.internal"}</span>. Lower-case letters, digits and -. The name cannot be used again.</p>
          {badName && <p className="text-small text-bad" role="alert">Use lower-case letters, digits and -, starting and ending with a letter or digit.</p>}
          <label className="block"><span className="mb-1 block text-muted">Group</span>
            <input className={field} value={group} list="agent-groups" required onChange={(e) => setGroup(e.target.value)} placeholder="ops" />
            <datalist id="agent-groups">{groups.data?.filter((g) => !["ambit-admins", "ambit-approvers"].includes(g.name)).map((g) => <option key={g.name} value={g.name} />)}</datalist></label>
          <p className="text-small text-muted">The agent gets what AccessRules and Budgets give this group, and then only what its scope names.</p>
          <label className="block"><span className="mb-1 block text-muted">Owner (optional)</span>
            <input className={field} value={owner} onChange={(e) => setOwner(e.target.value)} placeholder="a person's email, or group:platform. Empty: you" /></label>
        </div>
      )}
      <ScopePicker value={scope} onChange={setScope} />
      <p className="text-small text-muted">You can make up to {settings.maxPerPerson} agents for yourself. A key for the agent comes next.</p>
      {create.isError && <p className="text-bad" role="alert">{create.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={create.isPending || badName || noPlatform}>Create agent</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={() => onClose()}>Cancel</Button>
      </div>
    </form>
  );
}

function KeyShown({ created, admin, onDone }: { created: CreatedKey; admin: boolean; onDone: () => void }) {
  return (
    <div className="space-y-3 rounded-md border border-line p-4" data-testid="agent-key-created">
      <h3 className="font-semibold">Key ‘{created.view.name}’ created</h3>
      <p className="font-medium">This is the only time the key is shown. Put it into the platform's connector settings now.</p>
      <CopyBlock text={created.key} testId="new-key" label="Key" />
      <CopyBlock text={created.urls.mcp} testId="agent-mcp-url" label="MCP URL" />
      <CopyBlock text={headerLines(created.key).join("\n")} testId="agent-header" label="Header (send one of the two lines)" />
      <p className="text-small text-muted">Give the platform the MCP URL and one of the header lines. Where a platform asks for a header name and a value, use <span className="font-mono">Authorization</span> with <span className="font-mono">Bearer {"<key>"}</span>, or <span className="font-mono">x-api-key</span> with the key.</p>
      <FirstUse id={created.view.id} admin={admin} />
      <Button className="w-auto" onClick={onDone}>Done</Button>
    </div>
  );
}

function NewKey({ a, maxDays, admin, onClose, onBusy }: { a: AgentView; maxDays: number; admin: boolean; onClose: () => void; onBusy: (b: boolean) => void }) {
  const qc = useQueryClient();
  const options = [30, 90, 365].filter((d) => d <= maxDays);
  const [name, setName] = useState("");
  const [days, setDays] = useState(String(options.includes(90) ? 90 : options[options.length - 1] ?? 30));
  const [ranges, setRanges] = useState("");
  const [created, setCreated] = useState<CreatedKey | null>(null); // the only place the plain key lives
  const create = useMutation({
    gcTime: 0, // the plain key must not stay in the mutation cache after it is dismissed
    mutationFn: () => sendJSON("POST", `/api/agents/${encodeURIComponent(a.name)}/keys`, { name: name.trim(), days: Number(days), sourceRanges: parseRanges(ranges) }) as Promise<CreatedKey>,
    onMutate: () => onBusy(true),
    onSuccess: (c) => { setCreated(c); qc.invalidateQueries({ queryKey: agentsKey }); },
    onSettled: () => onBusy(false),
  });
  // The key is shown only here: while it is being made, the form cannot be closed (the key would be lost).
  if (created) return <KeyShown created={created} admin={admin} onDone={() => { setCreated(null); create.reset(); onClose(); }} />;
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label="New key for the agent" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <h3 className="font-semibold">New key</h3>
      <div className="grid grid-cols-2 gap-3">
        <label className="block"><span className="mb-1 block text-muted">Key name</span>
          <input className={field} value={name} maxLength={100} onChange={(e) => setName(e.target.value)} placeholder="grok connector" required /></label>
        <label className="block"><span className="mb-1 block text-muted">End date</span>
          <select className={field} value={days} onChange={(e) => setDays(e.target.value)}>
            {options.map((d) => <option key={d} value={d}>{d} days from now</option>)}</select></label>
      </div>
      <label className="block"><span className="mb-1 block text-muted">Source address ranges (optional)</span>
        <textarea className={field} rows={2} value={ranges} onChange={(e) => setRanges(e.target.value)} placeholder="203.0.113.0/24" /></label>
      <p className="text-small text-muted">One range per line, at most 10. With ranges, the key works only from an address inside one of them. Set them only when you know the address the gateway sees: behind a load balancer that hides the client address, every request looks like one address.</p>
      {create.isError && <p className="text-bad" role="alert">{create.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={create.isPending}>Create key</Button>
        <Button type="button" variant="outline" className="w-auto" disabled={create.isPending} onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );
}

function EditAgent({ a, onClose }: { a: AgentView; onClose: () => void }) {
  const qc = useQueryClient();
  const [displayName, setDisplayName] = useState(a.displayName);
  const [platform, setPlatform] = useState(a.platform);
  const [scope, setScope] = useState(a.scope);
  const save = useMutation({
    mutationFn: () => sendJSON("PATCH", `/api/agents/${encodeURIComponent(a.name)}`, { displayName: displayName.trim(), platform, scope }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: agentsKey }); onClose(); },
  });
  return (
    <form className="space-y-3 rounded-md border border-line p-4" aria-label="Change the agent" onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
      <h3 className="font-semibold">Change the agent</h3>
      <label className="block"><span className="mb-1 block text-muted">Name</span>
        <input className={field} value={displayName} maxLength={100} onChange={(e) => setDisplayName(e.target.value)} required /></label>
      <PlatformField value={platform} onChange={setPlatform} />
      <ScopePicker value={scope} onChange={setScope} />
      <p className="text-small text-muted">A new scope is written into the agent's keys. The gateway uses it about a second after it has read the changed keys. If the answer is “try again”, the change may already be saved and a key may still carry the old scope: save again to finish it.</p>
      {save.isError && <p className="text-bad" role="alert">{save.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={save.isPending || !PLATFORM_RE.test(platform)}>Save</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );
}

// Admins only: the calls of the last 30 days, from the audit trail.
function ToolsUsed({ name }: { name: string }) {
  const from = new Date(Date.now() - 30 * 864e5).toISOString().slice(0, 10);
  const q = useQuery({ queryKey: [...agentsKey, "calls", name], retry: false,
    queryFn: () => getJSON<{ calls: AuditCall[]; limited: boolean }>(`/api/audit?${new URLSearchParams({ agent: name, from })}`) });
  const used = toolsUsed(q.data?.calls ?? []);
  return (
    <div className="space-y-1" data-testid="tools-used">
      <h3 className="text-muted">Tools used, last 30 days</h3>
      {q.isPending && <p className="text-muted">Loading…</p>}
      {q.isError && <p className="text-muted" role="status" data-testid="no-audit-trail">Not known: the audit trail did not answer. {q.error.message}</p>}
      {q.data && used.length === 0 && <p className="text-muted">No tool call by this agent in the last 30 days.</p>}
      {used.length > 0 && <ul className="space-y-0.5">{used.map(([t, n]) => <li key={t}><span className="font-mono">{t}</span> <span className="text-muted">× {n}</span></li>)}</ul>}
      {q.data?.limited && <p className="text-small text-muted">Showing the newest 1,000 calls.</p>}
      <p><Link to="/audit" search={{ agent: name }} className="text-accent underline">Search Audit for this agent</Link></p>
    </div>
  );
}

// Stop and start. A stop is complete only when every gateway has confirmed it; the page shows what the console measured and
// never promises that no call is accepted after the request.
function StopPanel({ a, admin }: { a: AgentView; admin: boolean }) {
  const qc = useQueryClient();
  const [confirm, setConfirm] = useState(false);
  const refresh = () => qc.invalidateQueries({ queryKey: agentsKey });
  const stop = useMutation({ mutationFn: () => sendJSON("POST", `/api/agents/${encodeURIComponent(a.name)}/stop`, {}), onSuccess: () => { setConfirm(false); refresh(); } });
  const start = useMutation({ mutationFn: () => sendJSON("POST", `/api/agents/${encodeURIComponent(a.name)}/start`, {}), onSuccess: refresh });
  const mayAct = a.manageable || admin; // the owner (person mode) or an admin
  const notes = stopNotes(a);
  return (
    <div className="space-y-2" data-testid="stop-panel">
      {notes.map((n) => <p key={n.text} role="status" data-testid={`stop-note-${n.tone}`}
        className={n.tone === "warn" ? "rounded-md bg-warn-soft px-2 py-1 text-warn" : n.tone === "ok" ? "text-ok" : "text-muted"}>{n.text}</p>)}
      {mayAct && admin && (a.state === "stopping" || a.state === "stopped") && (
        <p><a className="text-accent underline" href={`/api/evidence/agent-stop/${encodeURIComponent(a.name)}.zip`} download>Download the stop evidence (agent-stop.zip)</a>
          <span className="block text-small text-muted">Times of each step, the confirmation of each gateway, and the calls that were accepted after the request, counted. It needs evidence to be switched on.</span></p>)}
      {start.isError && <p className="text-bad" role="alert">{start.error.message}</p>}
      {mayAct && a.state === "stopped" && <Button variant="outline" className="w-auto py-1" disabled={start.isPending} onClick={() => start.mutate()}>Start agent</Button>}
      {mayAct && a.state === "active" && (confirm ? (
        <div role="alertdialog" aria-label="Stop agent" aria-describedby="stop-agent-text" className="space-y-2 rounded-md border border-bad p-3">
          <p id="stop-agent-text">Stop <strong>{a.displayName}</strong>? Its keys are deleted now and cannot be brought back; Start gives the agent new keys. The page shows Stopped only after every gateway has confirmed the stop. A call that is already on its way can still be accepted until then.</p>
          {stop.isError && <p className="text-bad" role="alert">{stop.error.message}</p>}
          <div className="flex gap-2">
            <Button className="w-auto py-1" disabled={stop.isPending} onClick={() => stop.mutate()}>Stop agent</Button>
            <Button variant="outline" className="w-auto py-1" autoFocus onClick={() => setConfirm(false)}>Keep it running</Button>
          </div>
        </div>
      ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirm(true)}>Stop agent</Button>)}
    </div>
  );
}

// The owner left (offboarding or a change at the identity provider): the agent keeps working for a limited time (Helm value
// agents.orphanDays, 7 by default), then it is stopped. An admin moves it to a new owner.
function OrphanBanner({ a, admin }: { a: AgentView; admin: boolean }) {
  const qc = useQueryClient();
  const [owner, setOwner] = useState("");
  const move = useMutation({ mutationFn: () => sendJSON("PUT", `/api/agents/${encodeURIComponent(a.name)}/owner`, { owner: owner.trim() }),
    onSuccess: () => { setOwner(""); qc.invalidateQueries({ queryKey: agentsKey }); } });
  if (!a.orphanedAt) return null;
  return (
    <div role="status" data-testid="orphan-banner" className="space-y-2 rounded-md bg-warn-soft p-3 text-warn">
      <p>The owner of this agent left on {utcTime(a.orphanedAt)}. The agent keeps working for a limited time (the Helm value agents.orphanDays, 7 by default). Then it is stopped. An admin can move it to a new owner.</p>
      {admin && a.mode === "itself" && (
        <form className="flex flex-wrap items-end gap-2 text-ink" aria-label="Owner change" onSubmit={(e) => { e.preventDefault(); move.mutate(); }}>
          <label><span className="mb-1 block text-muted">New owner</span><input className={field} value={owner} required onChange={(e) => setOwner(e.target.value)} placeholder="a person's email, or group:platform" /></label>
          <Button type="submit" className="w-auto" disabled={move.isPending}>Move agent</Button>
        </form>)}
      {move.isError && <p className="text-bad" role="alert">{move.error.message}</p>}
    </div>
  );
}

// Which of the owner's app connections the agent may use. Unticked by default. Only the owner changes it: the connection is the owner's own
// sign-in to the app. Ticking gives nothing alone: the owner must have connected the app, and the agent still needs the server in its
// scope and in the owner's AccessRules.
function ConsentSection({ a, canChange }: { a: AgentView; canChange: boolean }) {
  const qc = useQueryClient();
  const conns = useConnections();
  const [picked, setPicked] = useState(a.connectionConsent);
  const save = useMutation({ mutationFn: () => sendJSON("PATCH", `/api/agents/${encodeURIComponent(a.name)}`, { connectionConsent: picked }),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentsKey }) });
  if (a.mode === "itself") return <p className="text-small text-muted" data-testid="consent-itself">An itself-mode agent gets no app connections in 0.6.0.</p>;
  const known = new Set((conns.data ?? []).map((c) => c.name));
  const gone = picked.filter((n) => !known.has(n));
  const changed = JSON.stringify([...picked].sort()) !== JSON.stringify([...a.connectionConsent].sort());
  return (
    <div className="space-y-1" data-testid="consent">
      <h3 className="text-muted">Apps with the owner's own sign-in</h3>
      <p className="text-small text-muted">Some MCP apps use a person's own login to that app (a connection). This agent may use {canChange ? "your" : "its owner's"} connection to an app only if it is ticked here. Nothing is ticked by default. Ticking gives nothing by itself: the app must be connected under My connections, the agent needs the app's server in its tools, and AccessRules must allow it.</p>
      {!canChange && <p data-testid="consent-list">{a.connectionConsent.length ? <>Allowed: <span className="font-mono">{a.connectionConsent.join(", ")}</span></> : "No connection is allowed."}</p>}
      {canChange && conns.isError && <p className="text-bad" role="alert">{conns.error.message}</p>}
      {canChange && conns.data && conns.data.length === 0 && <p className="text-muted">There are no apps with their own sign-in.</p>}
      {canChange && conns.data?.map((c) => (
        <label key={c.name} className="flex items-center gap-2">
          <input type="checkbox" checked={picked.includes(c.name)} onChange={() => setPicked(toggleName(picked, c.name))} />
          <span>May use my {c.displayName || c.name} connection</span>
          <span className="text-small text-muted">{c.state === "connected" ? "(connected)" : c.state === "broken" ? "(connection broken)" : "(not connected yet)"}</span>
        </label>))}
      {canChange && gone.length > 0 && <p className="text-small text-muted">Also allowed, but not a connection of yours: <span className="font-mono">{gone.join(", ")}</span>.{" "}
        <button type="button" className="underline" onClick={() => setPicked(picked.filter((n) => known.has(n)))}>Remove them</button></p>}
      {save.isError && <p className="text-bad" role="alert">{save.error.message}</p>}
      {canChange && <div className="flex items-center gap-2"><Button variant="outline" className="w-auto py-1" disabled={!changed || save.isPending} onClick={() => save.mutate()}>Save connections</Button>
        {save.isSuccess && !changed && <span role="status" className="text-small text-ok">Saved.</span>}</div>}
    </div>
  );
}

function AgentDetail({ name, list, onClose }: { name: string; list: AgentList | undefined; onClose: () => void }) {
  const me = useMe().data;
  const admin = me?.role === "admin";
  const qc = useQueryClient();
  const q = useAgent(name);
  const [editing, setEditing] = useState(false);
  const [addingKey, setAddingKey] = useState(false);
  const [confirmKey, setConfirmKey] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [keyBusy, setKeyBusy] = useState(false);
  const limits = useQuery({ queryKey: [...agentsKey, "key-limits", name], enabled: !!q.data,
    queryFn: async () => {
      const k = await getJSON<KeySettings>("/api/settings/keys");
      const sa = admin && q.data?.mode === "itself" ? await getJSON<{ settings: { maxDays: number } }>("/api/admin/service-accounts") : null;
      return Math.min(k.maxDays, sa?.settings.maxDays ?? Infinity);
    } });
  const revoke = useMutation({
    mutationFn: (id: string) => sendJSON("DELETE", `${admin ? "/api/admin/keys/" : "/api/keys/"}${encodeURIComponent(id)}`),
    onSuccess: () => { setConfirmKey(null); qc.invalidateQueries({ queryKey: agentsKey }); },
  });
  const remove = useMutation({
    mutationFn: () => sendJSON("DELETE", `/api/agents/${encodeURIComponent(name)}`),
    onSuccess: () => { qc.invalidateQueries({ queryKey: agentsKey }); onClose(); },
  });
  // The list shows the state too: when the detail learns of a new state (Stopped, after the poll), the list asks again.
  const seenState = q.data?.state;
  useEffect(() => { if (seenState) qc.invalidateQueries({ queryKey: [...agentsKey, "list"] }); }, [seenState, qc]);
  if (q.isError) return <p className="text-muted" role="status">This agent does not exist, or it is not yours.</p>;
  const a = q.data;
  if (!a) return <p className="text-muted">Loading…</p>;
  const keys = a.keys ?? [];
  const live = keys.filter((k) => k.status !== "stopped");
  // The longest end date: the smallest of agents.keyMaxDays, apiKeys.maxDays and (itself mode) serviceAccounts.maxDays.
  const maxDays = Math.min(list?.settings.keyMaxDays ?? 90, limits.data ?? Infinity);
  const usable = a.state === "active" && (list?.settings.enabled ?? true);
  const facts: [string, string][] = [
    ["Owner", a.owner], ["Kind", modeText(a.mode)], ["Declared platform", `${platformText(a.platform)} (declared, not proved)`], ["State", stateText[a.state] ?? a.state],
    ...(a.identity ? [["Calls as", a.identity] as [string, string]] : []), ...(a.group ? [["Group", a.group] as [string, string]] : []),
    ["Created", `${day(a.created)} by ${a.createdBy}`],
  ];
  return (
    <section aria-label={`Agent ${a.displayName}`} data-testid="agent-detail" className="space-y-3 rounded-md border border-line p-4">
      <div className="flex items-start justify-between gap-4">
        <h2 className="font-semibold">{a.displayName} <span className="font-mono text-small font-normal text-muted">{a.name}</span></h2>
        <Button variant="outline" className="w-auto py-1" disabled={keyBusy} onClick={onClose}>Close</Button>
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        {facts.map(([t, v]) => <div key={t} className="contents"><dt className="text-muted">{t}</dt>
          <dd data-testid={`agent-${t.toLowerCase().replace(/ /g, "-")}`}>{v}</dd></div>)}
      </dl>
      <OrphanBanner a={a} admin={admin} />
      <StopPanel a={a} admin={admin} />
      {a.state === "pending" && <p role="status" className="rounded-md bg-warn-soft px-2 py-1 text-warn">The service account of this agent is not ready yet. Keys can be made when the state is Active.</p>}
      <div className="space-y-1">
        <div className="flex items-center justify-between"><h3 className="text-muted">Tools it may use</h3>
          {a.manageable && usable && !editing && <Button variant="outline" className="w-auto py-1" onClick={() => setEditing(true)}>Change</Button>}</div>
        {a.scope.length === 0
          ? <p data-testid="agent-scope">None. With an empty scope the agent can use no tool. Change it to add servers or tools.</p>
          : <ul data-testid="agent-scope" className="space-y-0.5">{a.scope.map((e) => <li key={e} className="font-mono text-small">{e.includes("/") ? e : `${e} (all tools)`}</li>)}</ul>}
      </div>
      {editing && <EditAgent a={a} onClose={() => setEditing(false)} />}
      <ConsentSection a={a} canChange={a.manageable && a.mode === "person" && usable} />
      <div className="space-y-2">
        <div className="flex items-center justify-between"><h3 className="text-muted">Keys</h3>
          {a.manageable && usable && !addingKey && live.length < 2 && <Button variant="outline" className="w-auto py-1" onClick={() => setAddingKey(true)}>New key</Button>}</div>
        {addingKey && <NewKey a={a} maxDays={maxDays} admin={admin} onClose={() => setAddingKey(false)} onBusy={setKeyBusy} />}
        {revoke.isError && <p className="text-bad" role="alert">{revoke.error.message}</p>}
        {keys.length === 0 ? <p className="text-muted">No keys. The agent cannot call anything until it has a key.</p> : (
          <table className="w-full border-collapse text-left">
            <thead><tr className="border-b border-line text-muted"><th className={th}>Name</th><th className={th}>Starts with</th><th className={th}>Status</th><th className={th}>Stops on</th>
              <th className={th}>Last used</th><th className={th}>Source ranges</th><th /></tr></thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id} className="border-b border-line-soft align-top">
                  <td className="py-1.5 pr-3">{k.name}</td><td className="pr-3 font-mono text-small">{k.prefix}</td><td className="pr-3">{statusText(k)}</td><td className="pr-3">{day(k.until)}</td>
                  <td className="pr-3">{k.lastUsed ? day(k.lastUsed) : "not yet or unknown"}</td>
                  <td className="pr-3 font-mono text-small">{k.sourceRanges?.length ? k.sourceRanges.join(", ") : "any address"}</td>
                  <td className="text-right">
                    {(a.manageable || admin) && (confirmKey === k.id ? (
                      <span className="inline-flex gap-2" role="alert">
                        <Button className="w-auto py-1" disabled={revoke.isPending} onClick={() => revoke.mutate(k.id)}>Revoke key</Button>
                        <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmKey(null)}>Keep it</Button>
                      </span>
                    ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmKey(k.id)}>Revoke</Button>)}
                  </td>
                </tr>))}
            </tbody>
          </table>
        )}
      </div>
      {admin ? <ToolsUsed name={a.name} /> : <p className="text-small text-muted">Admins can see which tools this agent used in Audit.</p>}
      {(a.manageable || admin) && (
        <div className="flex justify-end">
          {confirmDelete ? (
            <div role="alertdialog" aria-label="Delete agent" aria-describedby="delete-agent-text" className="space-y-2 rounded-md border border-bad p-3">
              <p id="delete-agent-text">Delete <strong>{a.displayName}</strong> and its {plural(live.length, "live key")}? Platforms that use them stop within a second. {a.mode === "itself" ? "Its service account is deleted too, and the name cannot be used again." : ""}</p>
              {remove.isError && <p className="text-bad" role="alert">{remove.error.message}</p>}
              <div className="flex gap-2">
                <Button className="w-auto py-1" disabled={remove.isPending} onClick={() => remove.mutate()}>Delete agent</Button>
                <Button variant="outline" className="w-auto py-1" autoFocus onClick={() => setConfirmDelete(false)}>Keep it</Button>
              </div>
            </div>
          ) : <Button variant="outline" className="w-auto py-1" onClick={() => setConfirmDelete(true)}>Delete agent</Button>}
        </div>
      )}
    </section>
  );
}

export function AgentsPage() {
  const me = useMe().data;
  const admin = me?.role === "admin";
  const q = useAgents(admin);
  const use = useKeyUse(admin);
  const { agent: shown } = useSearch({ from: "/agents" });
  const navigate = useNavigate({ from: "/agents" });
  const show = (name?: string) => navigate({ search: name ? { agent: name } : {} });
  const [creating, setCreating] = useState(false);
  const lastUse = lastUseByAgent(use.data?.keys ?? []);
  const auditTrail = use.data?.settings.lastUsedAvailable;
  const s = q.data?.settings;
  const agents = q.data?.agents ?? [];
  return (
    <Shell crumb="You / Agents">
      <section className="max-w-6xl space-y-4 p-5">
        <div className="flex items-start justify-between gap-4">
          <div className="space-y-1">
            <h1 className="text-h1 font-semibold tracking-tight">Agents<Experimental /></h1>
            <p className="text-muted">An agent is a program or hosted assistant, such as Grok Bot, a Copilot Studio agent or AWS AgentCore, that calls MCP tools with a key of its own. Each agent has a scope: the servers and tools it may use. Calls it makes are recorded in Audit with the agent's name, when the audit trail is set up.{" "}
              <a className="underline" href="https://ambitproject.dev/docs/agents" target="_blank" rel="noreferrer">How agents work</a></p>
          </div>
          {s?.enabled && !creating && <Button className="w-auto shrink-0" onClick={() => { setCreating(true); show(); }}>New agent</Button>}
        </div>
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {s && !s.enabled && <p className="text-muted" role="status">Agents are switched off by your administrator (Helm values agents.enabled and apiKeys.enabled).</p>}
        {creating && s && <NewAgent admin={admin} settings={s} onClose={(created) => { setCreating(false); if (created) show(created); }} />}
        {shown && !creating && <AgentDetail key={shown} name={shown} list={q.data} onClose={() => show()} />}
        {s?.enabled && agents.length === 0 && !creating && (
          <div className="space-y-1 rounded-md border border-dashed border-line p-4" data-testid="agents-empty">
            <p className="font-medium">{admin ? "There are no agents yet." : "You have no agents yet."}</p>
            <p className="text-muted">Make an agent when a hosted assistant should call your MCP tools. You choose which servers and tools it may use, and you get a key for it. Audit then shows every call under the agent's name.</p>
          </div>
        )}
        {agents.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-left" data-testid="agents-table">
              <thead><tr className="border-b border-line text-muted">
                <th className={th}>Agent</th><th className={th}>Owner</th>
                <th className={th} title="What the owner says the agent is. Ambit does not check it.">Declared platform</th><th className={th}>Kind</th>
                <th className={th}>Last used</th><th className={th}>Tools it may use</th><th className={th}>State</th></tr></thead>
              <tbody>
                {agents.map((a) => (
                  <tr key={a.name} className="border-b border-line-soft align-top" data-testid={`agent-row-${a.name}`}>
                    <td className="py-1.5 pr-3"><button type="button" className="text-left text-accent underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-accent"
                      aria-current={a.name === shown ? "true" : undefined} onClick={() => { setCreating(false); show(a.name); }}>{a.displayName}</button>
                      <span className="block font-mono text-small text-muted">{a.name}</span></td>
                    <td className="pr-3 font-mono text-small">{a.owner}</td>
                    <td className="pr-3">{platformText(a.platform)}</td>
                    <td className="pr-3">{a.mode === "itself" ? "Itself" : "Person"}</td>
                    <td className="pr-3">{lastUse.get(a.name) ? day(lastUse.get(a.name)!) : auditTrail === false ? "unknown" : use.data ? "not yet" : ""}</td>
                    <td className="pr-3">{scopeSummary(a.scope)}</td>
                    <td>{stateText[a.state] ?? a.state}</td>
                  </tr>))}
              </tbody>
            </table>
          </div>
        )}
        {agents.length > 0 && auditTrail === false && <p className="text-muted" role="status" data-testid="no-audit-trail-note">The console has no audit trail (ClickHouse is not set up), so the last use of an agent is unknown and Audit has no calls to show.</p>}
        {agents.length > 0 && <p className="text-small text-muted">Last used is the newest call made with a key of the agent that still exists. A revoked key is not counted. “Declared platform” is what the owner said; Ambit does not check it.</p>}
      </section>
    </Shell>
  );
}
