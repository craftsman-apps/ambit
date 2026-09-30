import type { ReactNode } from "react";
import { Link, Navigate } from "@tanstack/react-router";
import { useMe, useModels, useRequests, useRules, useServers } from "@/api";
import { Button } from "@/components/ui/button";
import { ThemeSwitch } from "@/components/theme-switch";

function NavLink({ to, children, count }: { to: string; children: ReactNode; count?: number }) {
  return (
    <Link to={to} className="flex items-center justify-between border-l-2 border-transparent px-4 py-1.5 text-muted hover:text-ink"
      activeProps={{ className: "border-accent! bg-selected text-ink font-medium" }} activeOptions={{ exact: true, includeSearch: false }}>
      <span>{children}</span>
      {count !== undefined && <span className="font-mono text-[11px] text-muted">{count}</span>}
    </Link>
  );
}

const Section = ({ children }: { children: ReactNode }) => (
  <div className="px-4 pb-2 pt-5 font-mono text-[10.5px] uppercase tracking-[.12em] text-muted">{children}</div>
);

// Style A shell: left navigation, top bar, content. Pages render inside it.
export function Shell({ crumb, children }: { crumb: string; children: ReactNode }) {
  const me = useMe();
  const rules = useRules();
  const models = useModels();
  const servers = useServers();
  const requests = useRequests();
  if (me.isPending) return null;
  if (!me.data) return <Navigate to="/login" />;
  const u = me.data;
  return (
    <div className="grid min-h-screen grid-cols-[212px_1fr] text-[13.5px]">
      <nav className="border-r border-line bg-sunken py-4" aria-label="Console">
        <div className="flex items-center gap-2.5 px-4 pb-3">
          <img src="/favicon.svg" alt="" className="size-6" />
          <span className="font-semibold tracking-tight">Ambit</span>
        </div>
        <Section>Govern</Section>
        <NavLink to="/">Overview</NavLink>
        <NavLink to="/access-rules" count={rules.data?.length}>Access rules</NavLink>
        <NavLink to="/models" count={models.data?.length}>Models</NavLink>
        <NavLink to="/servers" count={servers.data?.length}>
          MCP servers
          {!!requests.data?.filter((q) => !q.decision).length && (
            <span className="ml-1.5 rounded bg-warn-soft px-1 font-mono text-[11px]" title="Requests waiting for an approver">
              {requests.data.filter((q) => !q.decision).length} waiting
            </span>
          )}
        </NavLink>
        {u.role === "admin" && <NavLink to="/budgets">Budgets</NavLink>}
        {u.role === "admin" && <><Section>Evidence</Section><NavLink to="/audit">Audit</NavLink><NavLink to="/usage">Usage</NavLink></>}
        <Section>You</Section>
        <NavLink to="/connections">My connections</NavLink>
        {u.role === "admin" && <><Section>Settings</Section><NavLink to="/settings/sign-in">Sign-in</NavLink><NavLink to="/settings/apps">Apps</NavLink><NavLink to="/settings/offboarding">Offboarding</NavLink></>}
      </nav>
      <div className="grid grid-rows-[auto_1fr]">
        <header className="flex items-center justify-between border-b border-line bg-surface px-4 py-2">
          <span className="text-muted">{crumb}</span>
          <div className="flex items-center gap-3 text-[13px]">
            <ThemeSwitch />
            <span className="text-muted">{u.email}</span>
            <span className="rounded-md border border-line px-1.5 py-0.5 font-mono text-[11px]">{u.role}</span>
            <form method="post" action="/auth/logout">
              <Button variant="outline" type="submit" className="w-auto py-1.5">Sign out</Button>
            </form>
          </div>
        </header>
        <main className="min-w-0 bg-surface">{children}</main>
      </div>
    </div>
  );
}

export function Status({ ready, text }: { ready: string; text?: string }) {
  const tone = ready === "True" ? "bg-ok" : ready === "False" ? "bg-bad" : "bg-warn";
  const label = text ?? (ready === "True" ? "Ready" : ready === "False" ? "Degraded" : "Pending");
  return (
    <span className={`inline-flex items-center gap-1.5 whitespace-nowrap ${ready === "False" ? "text-bad" : ""}`}>
      <i className={`block size-[7px] rounded-full ${tone}`} aria-hidden />
      {label}
    </span>
  );
}
