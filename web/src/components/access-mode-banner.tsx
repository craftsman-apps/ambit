import { Link } from "@tanstack/react-router";
import { useConfig } from "@/api";
import { modeBanner, pendingBanner } from "@/lib/audit-mode";

// While audit-only mode is on, every page says so to everyone signed in: nothing is refused by an access rule.
// A mode switch or a rules change that agentgateway has not applied yet is shown to admins (the server sends
// the pending entry only to them).
export function AccessModeBanner({ admin }: { admin: boolean }) {
  const cfg = useConfig().data;
  if (!cfg) return null;
  return (
    <>
      {cfg.accessMode === "audit" && (
        <p className="border-b border-line bg-warn-soft px-4 py-1.5" role="status" data-testid="access-mode-banner">
          <strong>{modeBanner(cfg.modeSince, new Date())}</strong>{" "}
          Ambit lets every signed-in request through, and every tool is listed to everyone. Access rules are recorded, not enforced.
          {admin && <> <Link to="/audit" className="underline">See what would be refused</Link> · To enforce, set <code className="font-mono">access.mode: enforce</code> and upgrade.</>}
        </p>
      )}
      {cfg.pending && (
        <p className="border-b border-line bg-warn-soft px-4 py-1.5" role="alert" data-testid="mode-pending-banner">
          {pendingBanner(cfg.pending, cfg.accessMode)}
        </p>
      )}
    </>
  );
}
