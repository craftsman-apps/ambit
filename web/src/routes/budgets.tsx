import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { isProposed, sendJSON, useConfig, useMe, type PullRequest } from "@/api";
import { Button } from "@/components/ui/button";
import { PullRequestNotice } from "@/components/pull-request";
import { Shell, Status } from "@/components/shell";

type UserUse = { email: string; used: number };
type BudgetView = { name: string; group: string; groupLimit: number | null; userLimit: number | null; groupUsed: number; users: UserUse[];
  ready: string; message: string; resetsAt: string; countsMissing?: boolean; gitManaged: boolean };
type BudgetInput = { name?: string; group: string; groupRequestsPerDay: number | null; userRequestsPerDay: number | null };

async function get<T>(url: string): Promise<T> {
  const r = await fetch(url, { credentials: "same-origin" });
  if (!r.ok) throw new Error((await r.json().catch(() => null))?.error ?? `${url} returned ${r.status}`);
  return r.json();
}
const field = "w-full rounded-md border border-line bg-surface px-2.5 py-1.5 text-[13px] outline-none focus:border-accent";
// A Kubernetes name from a group name: "ML Research/EU" -> "ml-research-eu".
const nameFor = (g: string) => g.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63) || "budget";

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

// The Budgets form: a new budget (no `editing`) or a change to one.
function BudgetForm({ editing, onDone }: { editing?: BudgetView; onDone: () => void }) {
  const qc = useQueryClient();
  const repository = useConfig().data?.repository;
  const [group, setGroup] = useState(editing?.group ?? "");
  const [groupLimit, setGroupLimit] = useState(editing?.groupLimit?.toString() ?? "");
  const [userLimit, setUserLimit] = useState(editing?.userLimit?.toString() ?? "");
  const [pr, setPR] = useState<PullRequest | null>(null);
  const num = (v: string) => (v.trim() === "" ? null : Number(v));
  const save = useMutation({
    mutationFn: () => {
      const body: BudgetInput = { group: group.trim(), groupRequestsPerDay: num(groupLimit), userRequestsPerDay: num(userLimit) };
      return editing ? sendJSON("PUT", `/api/budgets/${encodeURIComponent(editing.name)}`, body)
        : sendJSON("POST", "/api/budgets", { ...body, name: nameFor(group) });
    },
    onSuccess: (res) => { qc.invalidateQueries({ queryKey: ["budgets"] }); if (isProposed(res)) setPR(res.pullRequest); else onDone(); },
  });
  if (pr) return <PullRequestNotice pr={pr} repository={repository} onDone={onDone} />;
  return (
    <form className="space-y-3 rounded-md border border-line p-4" onSubmit={(e) => { e.preventDefault(); save.mutate(); }} aria-label={editing ? `Change budget for ${editing.group}` : "New budget"}>
      <h2 className="font-semibold">{editing ? `Change the budget for ${editing.group}` : "New budget"}</h2>
      <label className="block"><span className="mb-1 block text-muted">Group (as your identity provider names it)</span>
        <input id="budget-group" className={field} value={group} onChange={(e) => setGroup(e.target.value)} placeholder="eng" required disabled={!!editing} /></label>
      <div className="grid grid-cols-2 gap-3">
        <label className="block"><span className="mb-1 block text-muted">Whole group, requests per day</span>
          <input id="budget-group-limit" type="number" min={1} className={field} value={groupLimit} onChange={(e) => setGroupLimit(e.target.value)} placeholder="no limit" /></label>
        <label className="block"><span className="mb-1 block text-muted">Each person, requests per day</span>
          <input id="budget-user-limit" type="number" min={1} className={field} value={userLimit} onChange={(e) => setUserLimit(e.target.value)} placeholder="no limit" /></label>
      </div>
      <p className="text-[12.5px] text-muted">Set one or both. Days are UTC. Over a limit the gateway answers 429; every replica shares the same count.</p>
      {save.isError && <p className="text-bad" role="alert">{save.error.message}</p>}
      <div className="flex gap-2">
        <Button type="submit" className="w-auto" disabled={save.isPending}>{editing ? "Save" : "Add budget"}</Button>
        <Button type="button" variant="outline" className="w-auto" onClick={onDone}>Cancel</Button>
      </div>
    </form>
  );
}

// Budgets (journey A3): each group's daily LLM request budget, and how much of it is used today.
export function BudgetsPage() {
  const q = useQuery({ queryKey: ["budgets"], queryFn: () => get<BudgetView[]>("/api/budgets"), refetchInterval: 30_000 });
  const qc = useQueryClient();
  const me = useMe().data;
  const gitops = useConfig().data?.mode === "gitops";
  const repository = useConfig().data?.repository;
  const [form, setForm] = useState<"new" | BudgetView | null>(null);
  const [pr, setPR] = useState<PullRequest | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: (name: string) => sendJSON("DELETE", `/api/budgets/${encodeURIComponent(name)}`),
    onSuccess: (res) => { setConfirm(null); qc.invalidateQueries({ queryKey: ["budgets"] }); if (isProposed(res)) setPR(res.pullRequest); },
  });
  const admin = me?.role === "admin";
  return (
    <Shell crumb="Govern / Budgets">
      <section className="max-w-4xl space-y-4 p-5">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold tracking-tight">Budgets</h1>
            <p className="text-muted">LLM requests per day for each group, and for each person in it. Over the limit the gateway answers 429 until the day resets (UTC).</p>
          </div>
          {admin && !form && <Button className="w-auto shrink-0" onClick={() => setForm("new")}>New budget</Button>}
        </div>
        {form && <BudgetForm editing={form === "new" ? undefined : form} onDone={() => setForm(null)} />}
        {pr && <PullRequestNotice pr={pr} repository={repository} onDone={() => setPR(null)} />}
        {remove.isError && <p className="text-bad" role="alert">{remove.error.message}</p>}
        {q.isError && <p className="text-bad" role="alert">{q.error.message}</p>}
        {q.data?.length === 0 && !form && <p className="text-muted">No budgets yet. Groups without a budget have no daily limit.{admin && " Add one with New budget."}</p>}
        {q.data?.map((b) => (
          <div key={b.name} className="rounded-md border border-line p-4" data-testid={`budget-${b.group}`}>
            <div className="mb-2 flex items-center justify-between gap-3">
              <h2 className="font-semibold">{b.group} <span className="font-mono text-[12px] font-normal text-muted">{b.name}</span></h2>
              <span className="flex items-center gap-3">
                <Status ready={b.ready} text={b.ready === "True" ? "Enforced" : undefined} />
                {admin && (b.gitManaged && !gitops
                  ? <span className="text-[12px] text-muted">Managed in Git</span>
                  : <>
                      <Button variant="outline" className="w-auto" onClick={() => setForm(b)}>Edit</Button>
                      <Button variant="outline" className="w-auto" onClick={() => setConfirm(b.name)}>Delete</Button>
                    </>)}
              </span>
            </div>
            {confirm === b.name && (
              <div className="mb-3 flex flex-wrap items-center gap-2 rounded-md border border-bad p-2" role="alert">
                <span>Remove the budget for {b.group}? The group will have no daily limit.</span>
                <Button className="w-auto" disabled={remove.isPending} onClick={() => remove.mutate(b.name)}>Remove budget</Button>
                <Button variant="outline" className="w-auto" onClick={() => setConfirm(null)}>Keep it</Button>
              </div>
            )}
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
            <p className="mt-2 text-[12px] text-muted">Resets {new Date(b.resetsAt).toISOString().replace("T", " ").slice(0, 16)} UTC.</p>
          </div>
        ))}
      </section>
    </Shell>
  );
}
