import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useConfig, useConnections, useMe, useModels, useMyAccess, useRequests, useRules, useServers } from "@/api";
import { Card } from "@/components/ui/card";
import { Shell } from "@/components/shell";
import { closedWording } from "@/lib/audit-mode";

export function HomePage() {
  const me = useMe();
  const rules = useRules();
  const models = useModels();
  const config = useConfig();
  const servers = useServers();
  const requests = useRequests();
  const access = useMyAccess();
  const conns = useConnections();
  const broken = (conns.data ?? []).filter((c) => c.state === "broken");
  const u = me.data;
  const waiting = (requests.data ?? []).filter((q) => !q.decision).length;
  const attention = (rules.data ?? []).filter((r) => r.ready !== "True").length;
  const closed = (models.data ?? []).filter((m) => !m.allSignedIn && m.allowedGroups.length === 0).length;
  const admin = u?.role === "admin";
  // Admins see budgets at 80% or more today; the budgets endpoint is admin-only.
  const budgets = useQuery({
    queryKey: ["budgets"], enabled: admin,
    queryFn: async () => { const r = await fetch("/api/budgets", { credentials: "same-origin" }); return r.ok ? r.json() as Promise<{ group: string; groupLimit: number | null; groupUsed: number; userLimit: number | null; users: { used: number }[] }[]> : []; },
  });
  const nearLimit = (budgets.data ?? []).filter((b) =>
    (b.groupLimit && b.groupUsed >= 0.8 * b.groupLimit) || (b.userLimit && b.users.some((x) => x.used >= 0.8 * b.userLimit!)));
  const changed = (servers.data ?? []).filter((s) => s.changedTools.length > 0);
  const todo = [
    changed.length > 0 && { to: "/servers", text: `${changed.map((s) => s.name).join(", ")}: tools changed since approval; the gateway hides them until an admin approves them again` },
    waiting > 0 && { to: "/servers", text: `${waiting} server request${waiting === 1 ? "" : "s"} waiting for an approver` },
    nearLimit.length > 0 && { to: "/budgets", text: `${nearLimit.map((b) => b.group).join(", ")}: at 80% or more of today's budget` },
    attention > 0 && { to: "/access-rules", text: `${attention} access rule${attention === 1 ? "" : "s"} not applied; open them to see why` },
  ].filter(Boolean) as { to: string; text: string }[];
  return (
    <Shell crumb="Govern / Overview">
      <section className="max-w-4xl space-y-4 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Overview</h1>
          <p className="text-muted" data-testid="signed-in-as">Signed in as {u?.bootstrap ? "admin (bootstrap)" : u?.name || u?.email}</p>
        </div>
        {admin && (
          <Card data-testid="attention" className={todo.length ? "border-warn" : undefined}>
            <h2 className="mb-1 font-semibold">Needs your attention</h2>
            {todo.length === 0 ? <p className="text-muted">Nothing. Every rule is applied, no tool changed, and no budget is near its limit.</p> : (
              <ul className="space-y-1">
                {todo.map((t) => <li key={t.text}><Link to={t.to} className="text-accent-text underline">{t.text}</Link></li>)}
              </ul>
            )}
          </Card>
        )}
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
            <div className="font-mono text-label uppercase tracking-[.12em] text-muted">Models</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{models.data?.length ?? "–"}</div>
            <div className="text-small text-muted">{closedWording(config.data?.accessMode === "audit").count(closed)}</div>
          </Link>
          <Link to="/access-rules" className="rounded-md border border-line p-4 hover:bg-sunken">
            <div className="font-mono text-label uppercase tracking-[.12em] text-muted">Access rules</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{rules.data?.length ?? "–"}</div>
            <div className={attention ? "text-small text-bad" : "text-small text-muted"}>{attention} need attention</div>
          </Link>
          <Link to="/servers" className="rounded-md border border-line p-4 hover:bg-sunken">
            <div className="font-mono text-label uppercase tracking-[.12em] text-muted">MCP servers</div>
            <div className="mt-1 text-2xl font-semibold tabular-nums">{servers.data?.length ?? "–"}</div>
            <div className={waiting ? "text-small text-warn" : "text-small text-muted"}>{waiting} waiting for an approver</div>
          </Link>
        </div>
        {access.data && (
          <Card data-testid="my-access">
            <h2 className="mb-1 font-semibold">What you can use</h2>
            <p className="mb-3 text-small text-muted">
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
                    <span className="text-small text-muted"> · rule {s.rules.join(", ")}</span>
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
