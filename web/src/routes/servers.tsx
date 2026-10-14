import { Fragment, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { sendJSON, useConnections, useMe, useRequests, useServers, type RequestInput, type RequestView, type ServerView } from "@/api";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";
import { cn } from "@/lib/utils";

const th = "px-3 py-2.5 text-left font-mono text-label font-medium uppercase tracking-[.1em] text-muted";
const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-body outline-none focus:border-accent";

// "ghcr.io/org/server@sha256:ab82…" is too long for a table cell; the full digest is in the title.
const shortImage = (image: string) => {
  const [repo, digest] = image.split("@sha256:");
  return digest ? `${repo.split("/").slice(-1)[0]}@${digest.slice(0, 12)}` : image;
};
// Where a server runs: its image in the cluster, or the host of a server reached by URL.
const runs = (x: { image: string; url: string }) => x.url ? new URL(x.url).host : shortImage(x.image);

export function ServersPage() {
  const me = useMe();
  const servers = useServers();
  const requests = useRequests();
  const [open, setOpen] = useState<string | null>(null);
  const [asking, setAsking] = useState(false);
  const waiting = (requests.data ?? []).filter((q) => !q.decision);
  const decided = (requests.data ?? []).filter((q) => q.decision);
  const canDecide = me.data?.role === "admin" || me.data?.role === "approver";

  return (
    <Shell crumb="Govern / MCP servers">
      <div className="grid min-h-full grid-cols-[1fr_256px] min-[1440px]:grid-cols-[1fr_340px]">
        <section className="min-w-0 space-y-6 p-5">
          <div className="flex items-end justify-between gap-3">
            <div>
              <h1 className="text-h1 font-semibold tracking-tight">MCP servers</h1>
              <p className="text-muted">Servers run in their own pods, or elsewhere and are reached by URL; either way, only through the gateway. Their tools are closed to everyone until an access rule names them.</p>
            </div>
            <Button className="w-auto whitespace-nowrap" onClick={() => setAsking(true)}>Request a server</Button>
          </div>
          {servers.isError && <p className="text-bad">{servers.error.message}</p>}
          <div className="overflow-x-auto"><table className="w-full border-collapse [&_td]:px-1.5 [&_th]:px-1.5 min-[1440px]:[&_td]:px-3 min-[1440px]:[&_th]:px-3">
            <thead>
              <tr className="border-b border-line">
                <th className={th}>Server</th><th className={th}>Runs</th><th className={th}>Tools</th>
                <th className={th}>Rules</th><th className={th}>Approved by</th><th className={th}>Status</th>
              </tr>
            </thead>
            <tbody>
              {servers.data?.map((s) => (
                <ServerRow key={s.name} s={s} open={open === s.name} onToggle={() => setOpen(open === s.name ? null : s.name)} />
              ))}
              {servers.data?.length === 0 && (
                <tr><td colSpan={6} className="px-3 py-6 text-muted">No MCP servers yet. Request one; an approver decides.</td></tr>
              )}
            </tbody>
          </table></div>
          {decided.length > 0 && (
            <div>
              <h2 className="mb-2 font-mono text-label uppercase tracking-[.12em] text-muted">Decisions</h2>
              <table className="w-full border-collapse text-body">
                <tbody>
                  {decided.map((q) => (
                    <tr key={q.name} className="border-b border-line-soft align-top">
                      <td className="whitespace-nowrap px-3 py-2 font-mono">{q.serverName}</td>
                      <td className={cn("px-3 py-2", q.decision === "Denied" && "text-bad")}>{q.decision}</td>
                      <td className="px-3 py-2 text-muted">asked by {q.requestedBy}, {q.decision.toLowerCase()} by {q.decidedBy}{q.comment && `: “${q.comment}”`}
                        {q.pullRequest && <> · <a className="text-accent-text underline" href={q.pullRequest} target="_blank" rel="noopener noreferrer">pull request</a></>}</td>
                      <td className="px-3 py-2 font-mono text-small text-muted" title={q.image || q.url}>{runs(q)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
        <aside className="space-y-5 border-l border-line bg-sunken p-5">
          {asking && <RequestForm onDone={() => setAsking(false)} />}
          <div>
            <h2 className="mb-2 text-h2 font-semibold">Waiting for an approver <span className="font-mono text-muted">{waiting.length}</span></h2>
            {waiting.length === 0 && <p className="text-muted">Nothing is waiting.</p>}
            <div className="space-y-3">
              {waiting.map((q) => <PendingCard key={q.name} q={q} me={me.data?.email ?? ""} canDecide={canDecide} />)}
            </div>
          </div>
        </aside>
      </div>
    </Shell>
  );
}

function ServerRow({ s, open, onToggle }: { s: ServerView; open: boolean; onToggle: () => void }) {
  const me = useMe();
  const qc = useQueryClient();
  const approve = useMutation({
    mutationFn: () => sendJSON("POST", `/api/servers/${encodeURIComponent(s.name)}/approve-tools`, {}),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["servers"] }),
  });
  const readTools = useMutation({
    mutationFn: () => sendJSON("PUT", `/api/servers/${encodeURIComponent(s.name)}/tool-reader`, {}),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["servers"] }),
  });
  const clearReader = useMutation({
    mutationFn: () => sendJSON("DELETE", `/api/servers/${encodeURIComponent(s.name)}/tool-reader`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["servers"] }),
  });
  return (
    <>
      <tr onClick={onToggle} aria-expanded={open} className={cn("cursor-pointer border-b border-line-soft align-top hover:bg-sunken", open && "bg-selected")}>
        <td className="whitespace-nowrap px-3 py-2.5"><span className="font-mono">{s.name}</span><span className="block text-small text-muted">{s.displayName}</span></td>
        <td className="min-w-[8.5rem] px-3 py-2.5 font-mono text-small [overflow-wrap:anywhere]" title={s.image || s.url}>{runs(s)}
          {s.connection && <span className="block font-sans text-small text-muted">each user's own {s.connection} account</span>}</td>
        <td className="px-3 py-2.5 tabular-nums">{s.tools.length || <span className="text-muted">–</span>}</td>
        <td className="px-3 py-2.5">{s.rules.length ? s.rules.map((r, i) => <Fragment key={r}><span className="whitespace-nowrap">{r}{i < s.rules.length - 1 && ","}</span> </Fragment>) : <span className="text-muted">None: closed to everyone</span>}</td>
        <td className="whitespace-nowrap px-3 py-2.5">{s.approvedBy || <span className="text-muted">{s.gitManaged ? "pull request" : "added directly"}</span>}</td>
        <td className="whitespace-nowrap px-3 py-2.5"><Status ready={s.ready} text={s.ready === "True" ? "Running" : undefined} />
          {s.changedTools.length > 0 && <span className="block text-small text-bad">{s.changedTools.length} tool(s) changed</span>}</td>
      </tr>
      {s.changedTools.length > 0 && (
        <tr className="border-b border-line-soft bg-warn-soft">
          <td colSpan={6} className="px-3 py-2.5" data-testid={`changed-${s.name}`}>
            <b>Changed since approval:</b> <span className="font-mono">{s.changedTools.join(", ")}</span>. The gateway hides these tools and refuses calls to them.
            Read the new descriptions below before approving.
            {me.data?.role === "admin" && (
              <Button className="ml-3 w-auto" disabled={approve.isPending} onClick={(e) => { e.stopPropagation(); approve.mutate(); }}>Approve new definitions</Button>
            )}
            {approve.isError && <span className="block text-bad">{approve.error.message}</span>}
          </td>
        </tr>
      )}
      {open && (
        <tr className="border-b border-line-soft bg-sunken">
          <td colSpan={6} className="px-3 py-3">
            <p className="mb-2 text-small text-muted">
              Clients see these tools as <span className="font-mono">{s.name}_&lt;tool&gt;</span>. {s.url ? "URL" : "Image"} <span className="font-mono break-all">{s.url || s.image}</span>
              {s.connection && <> · each call uses the caller's own <span className="font-mono">{s.connection}</span> grant</>}
              {s.requestedBy && <> · asked for by {s.requestedBy}</>}
            </p>
            {s.connection && (
              <p className="mb-2 flex flex-wrap items-center gap-2 text-small" data-testid={`reader-${s.name}`}>
                {s.reader && s.toolsRead === "True" ? <span>Tools read with {s.reader}'s account{s.toolsReadAt && `, ${new Date(s.toolsReadAt).toLocaleString()}`}</span>
                  : s.reader && s.toolsRead === "False" ? <span className="text-warn">{s.reader}'s account could not read the tools: {s.toolsReadMessage}</span>
                  : s.reader ? <span className="text-muted">Waiting to read tools with {s.reader}'s account</span>
                  : <span className="text-muted">No account reads this server's tools yet</span>}
                {me.data?.role === "admin" && <>
                  <Button className="w-auto" disabled={readTools.isPending} onClick={() => readTools.mutate()}>Use my account to read tools</Button>
                  {s.reader && <Button variant="outline" className="w-auto" disabled={clearReader.isPending} onClick={() => clearReader.mutate()}>Clear</Button>}
                </>}
                {(readTools.isError || clearReader.isError) && <span className="text-bad" role="alert">{(readTools.error ?? clearReader.error)?.message}</span>}
              </p>
            )}
            {s.tools.length === 0 && <p className="text-muted">{s.ready === "True" ? "Reading tools…" : s.message || "Tools appear once the server is running."}</p>}
            <ul className="grid grid-cols-2 gap-x-6 gap-y-1">
              {s.tools.map((t) => (
                <li key={t.name}><span className="font-mono text-small">{t.name}</span>
                  {t.description && <span className="block truncate text-small text-muted" title={t.description}>{t.description}</span>}</li>
              ))}
            </ul>
          </td>
        </tr>
      )}
    </>
  );
}

function PendingCard({ q, me, canDecide }: { q: RequestView; me: string; canDecide: boolean }) {
  const qc = useQueryClient();
  const app = useConnections().data?.find((c) => c.name === q.connection)?.displayName ?? q.connection;
  const [comment, setComment] = useState("");
  const decide = useMutation({
    mutationFn: (d: "approve" | "deny") => sendJSON("POST", `/api/server-requests/${encodeURIComponent(q.name)}/${d}`, { comment }),
    onSuccess: () => qc.invalidateQueries(),
  });
  const own = q.requestedBy === me;
  return (
    <div className="rounded-md border border-line bg-surface p-3" data-testid={`request-${q.serverName}`}>
      <p className="font-mono font-medium">{q.serverName}</p>
      <p className="text-small text-muted">{q.displayName && `${q.displayName} · `}asked by {q.requestedBy} · {q.transport}</p>
      <p className="mt-1 break-all font-mono text-small">{q.url || q.image}</p>
      {q.connection && <p className="text-small text-muted">Each user signs in to {app} with their own account.</p>}
      {q.reason && <p className="mt-2">“{q.reason}”</p>}
      {canDecide && !own && (
        <div className="mt-3 space-y-2">
          <input id={`comment-${q.name}`} aria-label="Comment for the requester" className={field} value={comment}
            onChange={(e) => setComment(e.target.value)} placeholder="Comment (optional)" />
          <div className="flex flex-wrap gap-2">
            <Button className="w-auto" disabled={decide.isPending} onClick={() => decide.mutate("approve")}>Approve {q.serverName}</Button>
            <Button variant="outline" className="w-auto" disabled={decide.isPending} onClick={() => decide.mutate("deny")}>Deny</Button>
          </div>
          <p className="text-small text-muted">{q.url ? "Approving lets the gateway reach this URL." : "Approving starts exactly this image digest."} Access still needs a rule.</p>
        </div>
      )}
      {own && <p className="mt-2 text-small text-muted">You asked for this, so another approver decides.</p>}
      {decide.isError && <p className="mt-2 text-bad">{decide.error.message}</p>}
    </div>
  );
}

function RequestForm({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient();
  const conns = useConnections();
  const [remote, setRemote] = useState(false);
  const [f, setF] = useState<RequestInput>({ serverName: "", displayName: "", image: "", url: "", connection: "", transport: "stdio", reason: "" });
  const set = (k: keyof RequestInput) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => setF({ ...f, [k]: e.target.value });
  const where = (r: boolean) => { setRemote(r); setF({ ...f, image: "", url: "", connection: "", transport: r ? "streamable-http" : "stdio" }); };
  const send = useMutation({
    mutationFn: () => sendJSON("POST", "/api/server-requests", f),
    onSuccess: () => { qc.invalidateQueries(); onDone(); },
  });
  return (
    <form onSubmit={(e) => { e.preventDefault(); send.mutate(); }} className="space-y-3 rounded-md border border-line bg-surface p-4">
      <h2 className="text-h2 font-semibold">Request a server</h2>
      <label className="block"><span className="mb-1 block text-muted">Name</span>
        <input id="req-name" className={field} value={f.serverName} onChange={set("serverName")} placeholder={remote ? "jira" : "postgres-readonly"} required /></label>
      <label className="block"><span className="mb-1 block text-muted">Display name</span>
        <input id="req-display" className={cn(field, "font-sans")} value={f.displayName} onChange={set("displayName")} placeholder={remote ? "Jira" : "Postgres (read-only)"} /></label>
      <fieldset className="space-y-1">
        <legend className="mb-1 text-muted">Where it runs</legend>
        <label className="flex items-center gap-2"><input type="radio" name="req-where" checked={!remote} onChange={() => where(false)} /> In the cluster, from a container image</label>
        <label className="flex items-center gap-2"><input type="radio" name="req-where" checked={remote} onChange={() => where(true)} /> Already running elsewhere, reached by URL</label>
      </fieldset>
      {!remote && (
        <label className="block"><span className="mb-1 block text-muted">Image, pinned by digest</span>
          <input id="req-image" className={field} value={f.image} onChange={set("image")} placeholder="ghcr.io/org/server@sha256:…" required /></label>
      )}
      {remote && (
        <>
          <label className="block"><span className="mb-1 block text-muted">Server URL</span>
            <input id="req-url" className={field} value={f.url} onChange={set("url")} placeholder="https://mcp.example.com/mcp" required /></label>
          <label className="block"><span className="mb-1 block text-muted">Sign-in for each user</span>
            <select id="req-connection" className={cn(field, "font-sans")} value={f.connection} onChange={set("connection")}>
              <option value="">None: the server needs no user sign-in</option>
              {conns.data?.map((c) => <option key={c.name} value={c.name}>{c.displayName} (each user connects their own account)</option>)}
            </select></label>
        </>
      )}
      <label className="block"><span className="mb-1 block text-muted">Transport</span>
        <select id="req-transport" className={field} value={f.transport} onChange={set("transport")}>
          {!remote && <option value="stdio">stdio</option>}<option value="streamable-http">streamable-http</option><option value="sse">sse</option>
        </select></label>
      <label className="block"><span className="mb-1 block text-muted">Why your team needs it</span>
        <textarea id="req-reason" className={cn(field, "font-sans")} rows={3} value={f.reason} onChange={set("reason")} /></label>
      {send.isError && <p className="rounded-md border border-bad p-2 text-bad">{send.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={send.isPending}>Send request</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onDone}>Cancel</Button>
      </div>
    </form>
  );
}
