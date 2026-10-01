import { useQuery } from "@tanstack/react-query";
import { getJSON, type Change, type ChangeStep, type PullRequest } from "@/api";

const mark: Record<ChangeStep["state"], string> = { done: "✓", waiting: "…", failed: "✕", skipped: "–" };
const tone: Record<ChangeStep["state"], string> = { done: "text-ok", waiting: "text-muted", failed: "text-bad", skipped: "text-muted" };

// Shown after a change in GitOps mode: the change waits for review in Git, then Argo CD applies it.
// The timeline is read every 10 seconds until the change is applied or the pull request is closed.
export function PullRequestNotice({ pr, repository, onDone }: { pr: PullRequest; repository?: string; onDone?: () => void }) {
  const q = useQuery({
    queryKey: ["change", pr.number],
    queryFn: () => getJSON<Change>(`/api/changes/${pr.number}`),
    // Stops when the pull request is closed unmerged or the last step is settled; a failed Applied step can
    // still recover (a server that starts late), so it is read again, less often.
    refetchInterval: (query) => {
      const d = query.state.data;
      const last = d?.steps.at(-1);
      if (d?.closed || (d?.steps.length === 4 && (last?.state === "done" || last?.state === "skipped"))) return false;
      return last?.state === "failed" ? 30_000 : 10_000;
    },
  });
  const steps = q.data?.steps;
  const applied = steps?.length === 4 && steps[3].state === "done";
  return (
    <div className="space-y-2 rounded-md border border-line bg-surface p-3" role="status">
      <p className="font-medium">Pull request #{pr.number} opened{repository && <> in <span className="font-mono">{repository}</span></>}</p>
      {!applied && <p className="text-[12.5px] text-muted">The change applies when the pull request is merged and Argo CD syncs it.</p>}
      {steps && (
        <ol className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[12.5px]" aria-label="Progress of this change" data-testid="change-timeline">
          {steps.map((s, i) => (
            <li key={s.name} className="flex items-center gap-2" data-state={s.state}>
              {i > 0 && <span className="text-muted" aria-hidden>→</span>}
              <span className={tone[s.state]}><span aria-hidden>{mark[s.state]} </span>{s.name}</span>
              {s.detail && <span className="font-mono text-[11.5px] text-muted">{s.detail}</span>}
            </li>
          ))}
        </ol>
      )}
      {q.isError && <p className="text-[12.5px] text-muted">Progress unavailable: {q.error.message}</p>}
      <p className="flex gap-3">
        <a className="text-accent-text underline" href={pr.url} target="_blank" rel="noopener noreferrer">Open the pull request</a>
        {onDone && <button type="button" className="text-muted underline" onClick={onDone}>Close</button>}
      </p>
    </div>
  );
}
