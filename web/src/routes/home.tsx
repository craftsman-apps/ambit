import { Link } from "@tanstack/react-router";
import { useConnections, useMe, useModels, useMyAccess, useRequests, useRules, useServers } from "@/api";
import { Card } from "@/components/ui/card";
import { Shell } from "@/components/shell";

export function HomePage() {
  const me = useMe();
  const rules = useRules();
  const models = useModels();
  const servers = useServers();
  const requests = useRequests();
  const access = useMyAccess();
  const conns = useConnections();
  const broken = (conns.data ?? []).filter((c) => c.state === "broken");
  const u = me.data;
  const waiting = (requests.data ?? []).filter((q) => !q.decision).length;
  const attention = (rules.data ?? []).filter((r) => r.ready !== "True").length;
  const closed = (models.data ?? []).filter((m) => !m.allSignedIn && m.allowedGroups.length === 0).length;
  return (
    <Shell crumb="Govern / Overview">
      <section className="max-w-4xl space-y-4 p-5">
        <h1 className="text-xl font-semibold tracking-tight" data-testid="signed-in-as">
          Signed in as {u?.bootstrap ? "admin (bootstrap)" : u?.name || u?.email}
        </h1>
        {broken.length > 0 && (
          <Card className="border-bad" data-testid="broken-connections">
            <b>Reconnect needed:</b> {broken.map((c) => c.displayName).join(", ")}. Your agent cannot use {broken.length === 1 ? "it" : "them"} until you reconnect.{" "}
            <Link to="/connections" className="text-accent-text underline">My connections</Link>
          </Card>
        )}
        {u?.bootstrap && (
          <Card className="border-warn bg-warn-soft">You are using the bootstrap admin. Connect your company identity provider, then turn this account off.</Card>
        )}
        <div className="grid grid-cols-3 gap-3">
          <Link to="/models" className="rounded-md border border-line p-4 hover:bg-sunken">
            <div className="font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Models</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{models.data?.length ?? "–"}</div>
            <div className="text-[12.5px] text-muted">{closed} closed to everyone</div>
          </Link>
          <Link to="/access-rules" className="rounded-md border border-line p-4 hover:bg-sunken">
            <div className="font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">Access rules</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{rules.data?.length ?? "–"}</div>
            <div className={attention ? "text-[12.5px] text-bad" : "text-[12.5px] text-muted"}>{attention} need attention</div>
          </Link>
          <Link to="/servers" className="rounded-md border border-line p-4 hover:bg-sunken">
            <div className="font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">MCP servers</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{servers.data?.length ?? "–"}</div>
            <div className={waiting ? "text-[12.5px] text-warn" : "text-[12.5px] text-muted"}>{waiting} waiting for an approver</div>
          </Link>
        </div>
        {access.data && (
          <Card data-testid="my-access">
            <h2 className="mb-1 font-semibold">What you can use</h2>
            <p className="mb-3 text-[12.5px] text-muted">
              From the access rules that name your groups{access.data.groups.length ? <> ({access.data.groups.join(", ")})</> : " (you have none)"} or everyone signed in.
              If a tool you need is missing, ask an admin for a rule; the gateway reports tools you may not use as unknown.
            </p>
            <dl className="grid grid-cols-[96px_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-muted">Models</dt>
              <dd>{access.data.models.length ? access.data.models.join(", ") : <span className="text-muted">None</span>}</dd>
              <dt className="text-muted">MCP tools</dt>
              <dd>
                {access.data.servers.length === 0 && <span className="text-muted">None</span>}
                {access.data.servers.map((s) => (
                  <span key={s.server} className="block">
                    <span className="font-mono">{s.server}</span>: {s.allTools ? "all tools" : s.tools.map((t) => `${s.server}_${t}`).join(", ")}
                    <span className="text-[12px] text-muted"> · rule {s.rules.join(", ")}</span>
                  </span>
                ))}
              </dd>
            </dl>
          </Card>
        )}
      </section>
    </Shell>
  );
}
