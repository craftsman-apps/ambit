import { Link } from "@tanstack/react-router";
import { useMe, useModels, useRules } from "@/api";
import { Card } from "@/components/ui/card";
import { Shell } from "@/components/shell";

export function HomePage() {
  const me = useMe();
  const rules = useRules();
  const models = useModels();
  const u = me.data;
  const attention = (rules.data ?? []).filter((r) => r.ready !== "True").length;
  const closed = (models.data ?? []).filter((m) => !m.allSignedIn && m.allowedGroups.length === 0).length;
  return (
    <Shell crumb="Govern / Overview">
      <section className="max-w-4xl space-y-4 p-5">
        <h1 className="text-xl font-semibold tracking-tight" data-testid="signed-in-as">
          Signed in as {u?.bootstrap ? "admin (bootstrap)" : u?.name || u?.email}
        </h1>
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
        </div>
      </section>
    </Shell>
  );
}
