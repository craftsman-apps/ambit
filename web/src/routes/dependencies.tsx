import { useQuery } from "@tanstack/react-query";
import { Shell, Status } from "@/components/shell";

type Dependency = { name: string; purpose: string; mode: string; whenDown: string; healthy: boolean; detail: string };

const modeText: Record<string, string> = {
  required: "Required", "fail-open": "Fails open", "fail-closed": "Fails closed", partial: "Partly",
};

// Dependencies (S4): each component Ambit relies on, what happens when it is down, and whether it answers now.
export function DependenciesPage() {
  const q = useQuery({
    queryKey: ["dependencies"], refetchInterval: 15_000,
    queryFn: async (): Promise<Dependency[]> => {
      const r = await fetch("/api/dependencies", { credentials: "same-origin" });
      if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `/api/dependencies returned ${r.status}`);
      return r.json();
    },
  });
  const th = "px-3 py-2 text-left font-mono text-label font-medium uppercase tracking-[.12em] text-muted";
  const down = q.data?.filter((d) => !d.healthy) ?? [];
  return (
    <Shell crumb="Settings / Dependencies">
      <section className="max-w-5xl space-y-5 p-5">
        <div>
          <h1 className="text-h1 font-semibold tracking-tight">Dependencies</h1>
          <p className="text-muted">What Ambit relies on, what happens when each part is down, and whether it answers now. Checked every 15 seconds.</p>
        </div>
        {q.isLoading && <p className="text-muted">Checking…</p>}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {q.data && (down.length === 0
          ? <p><Status ready="True" text="Everything answers" /></p>
          : <p role="alert"><Status ready="False" text={`Not answering: ${down.map((d) => d.name).join(", ")}`} /></p>)}
        {q.data && (
          <table className="w-full border-collapse text-body">
            <thead><tr className="border-b border-line"><th className={th}>Component</th><th className={th}>Health</th><th className={th}>When it is down</th></tr></thead>
            <tbody>
              {q.data.map((d) => (
                <tr key={d.name} className="border-b border-line-soft align-top" data-testid={`dep-${d.name}`}>
                  <td className="px-3 py-2.5"><span className="font-medium">{d.name}</span><span className="block text-small text-muted">{d.purpose}</span></td>
                  <td className="px-3 py-2.5"><Status ready={d.healthy ? "True" : "False"} text={d.healthy ? "Answers" : "Down"} /><span className="block text-small text-muted">{d.detail}</span></td>
                  <td className="px-3 py-2.5"><span className="font-medium">{modeText[d.mode] ?? d.mode}.</span> {d.whenDown}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </Shell>
  );
}
