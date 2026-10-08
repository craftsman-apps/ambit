import { useMutation, useQueryClient } from "@tanstack/react-query";
import { sendJSON, useConnections, type ConnectionView } from "@/api";
import { Button } from "@/components/ui/button";
import { Shell, Status } from "@/components/shell";

const state = (c: ConnectionView) =>
  c.state === "connected" ? <Status ready="True" text="Connected" />
    : c.state === "broken" ? <Status ready="False" text="Reconnect needed" />
    : <Status ready="Unknown" text="Not connected" />;

// My connections (journeys U1, U4): the SaaS apps your agent uses with your own account.
export function ConnectionsPage() {
  const q = useConnections();
  const qc = useQueryClient();
  const params = new URLSearchParams(window.location.search);
  const disconnect = useMutation({
    mutationFn: (name: string) => sendJSON("DELETE", `/api/connections/${encodeURIComponent(name)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["connections"] }),
  });
  return (
    <Shell crumb="You / My connections">
      <section className="max-w-4xl space-y-4 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">My connections</h1>
          <p className="text-muted">Apps your agent uses with your own account. Each call uses your grant, never someone else's.</p>
        </div>
        {params.get("connected") && <p className="rounded-md border border-ok p-2" role="status">Connected {params.get("connected")}.</p>}
        {params.get("error") && <p className="rounded-md border border-bad p-2 text-bad" role="status">{params.get("error")}</p>}
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        <table className="w-full border-collapse">
          <thead>
            <tr className="border-b border-line text-left font-mono text-[10.5px] uppercase tracking-[.1em] text-muted">
              <th className="px-3 py-2.5 font-medium">App</th><th className="px-3 py-2.5 font-medium">Used by</th>
              <th className="px-3 py-2.5 font-medium">Status</th><th className="px-3 py-2.5 font-medium" />
            </tr>
          </thead>
          <tbody>
            {q.data?.map((c) => (
              <tr key={c.name} className="border-b border-line-soft align-top" data-testid={`connection-${c.name}`}>
                <td className="px-3 py-2.5"><span className="font-medium">{c.displayName}</span><span className="block font-mono text-[12px] text-muted">{c.name}</span></td>
                <td className="px-3 py-2.5 font-mono text-[12.5px]">{c.servers.join(", ") || <span className="font-sans text-muted">No server yet</span>}</td>
                <td className="px-3 py-2.5">{state(c)}{c.error && <span className="block text-[12px] text-muted">{c.error}</span>}</td>
                <td className="px-3 py-2.5 text-right">
                  {c.state !== "connected"
                    ? <a className="inline-block rounded-md bg-accent px-3 py-1.5 font-medium text-accent-ink" href={c.connectURL}>{c.state === "broken" ? "Reconnect" : "Connect"}</a>
                    : <span className="inline-flex flex-wrap justify-end gap-2">
                        <a className="inline-block rounded-md border border-line px-3 py-1.5 font-medium" href={c.connectURL}>Connect again</a>
                        <Button variant="outline" className="w-auto" disabled={disconnect.isPending} onClick={() => disconnect.mutate(c.name)}>Disconnect</Button>
                      </span>}
                  {c.state === "connected" && !c.revocable && <span className="block text-[12px] text-muted">The provider has no revocation endpoint; disconnecting only forgets the grant.</span>}
                </td>
              </tr>
            ))}
            {q.data?.length === 0 && <tr><td colSpan={4} className="px-3 py-6 text-muted">No apps to connect yet. An admin adds them under Settings, Apps.</td></tr>}
          </tbody>
        </table>
        {disconnect.isError && <p className="text-bad">{disconnect.error.message}</p>}
      </section>
    </Shell>
  );
}
