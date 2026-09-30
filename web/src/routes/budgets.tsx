import { useQuery } from "@tanstack/react-query";
import { Shell, Status } from "@/components/shell";

type UserUse = { email: string; used: number };
type BudgetView = { name: string; group: string; groupLimit: number | null; userLimit: number | null; groupUsed: number; users: UserUse[];
  ready: string; message: string; resetsAt: string; countsMissing?: boolean };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}

function Bar({ used, limit }: { used: number; limit: number }) {
  const pct = Math.min(100, Math.round((used / limit) * 100));
  const tone = pct >= 100 ? "bg-bad" : pct >= 80 ? "bg-warn" : "bg-accent";
  return (
    <span className="inline-flex items-center gap-2">
      <span className="block h-1.5 w-32 overflow-hidden rounded-full bg-line-soft" role="meter" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label={`${pct}% used`}>
        <span className={`block h-full ${tone}`} style={{ width: `${pct}%` }} />
      </span>
      <span className="font-mono text-[12px] tabular-nums">{used} / {limit}</span>
    </span>
  );
}

// Budgets (journey A3): how much of each group's daily LLM request budget is used, shared across replicas.
export function BudgetsPage() {
  const q = useQuery({ queryKey: ["budgets"], queryFn: () => get<BudgetView[]>("/api/budgets"), refetchInterval: 30_000 });
  return (
    <Shell crumb="Govern / Budgets">
      <section className="max-w-4xl space-y-4 p-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Budgets</h1>
          <p className="text-muted">LLM requests per day for each group, and for each person in it. Over the limit the gateway answers 429 until the day resets (UTC).</p>
        </div>
        {q.isError && <p className="text-bad">{q.error.message}</p>}
        {q.data?.length === 0 && <p className="text-muted">No budgets yet. Add a Budget object for a group.</p>}
        {q.data?.map((b) => (
          <div key={b.name} className="rounded-md border border-line p-4" data-testid={`budget-${b.group}`}>
            <div className="mb-2 flex items-center justify-between">
              <h2 className="font-semibold">{b.group} <span className="font-mono text-[12px] font-normal text-muted">{b.name}</span></h2>
              <Status ready={b.ready} text={b.ready === "True" ? "Enforced" : undefined} />
            </div>
            {b.ready === "False" && <p className="mb-2 text-[12.5px] text-bad">{b.message}</p>}
            {b.countsMissing && <p className="mb-2 text-[12.5px] text-warn">Today's counts are unavailable (the counter store is not reachable).</p>}
            <dl className="grid grid-cols-[150px_1fr] gap-x-3 gap-y-1.5">
              <dt className="text-muted">Whole group</dt><dd>{b.groupLimit ? <Bar used={b.groupUsed} limit={b.groupLimit} /> : <span className="text-muted">no group limit</span>}</dd>
              <dt className="text-muted">Each person</dt><dd>{b.userLimit ? `${b.userLimit} per day` : <span className="text-muted">no per-person limit</span>}</dd>
            </dl>
            {b.userLimit && b.users.length > 0 && (
              <ul className="mt-2 space-y-1">
                {b.users.map((u) => <li key={u.email} className="flex items-center justify-between"><span>{u.email}</span><Bar used={u.used} limit={b.userLimit!} /></li>)}
              </ul>
            )}
            <p className="mt-2 text-[12px] text-muted">Resets {new Date(b.resetsAt).toLocaleString()}.</p>
          </div>
        ))}
      </section>
    </Shell>
  );
}
