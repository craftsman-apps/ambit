import type { PullRequest } from "@/api";

// Shown after a change in GitOps mode: the change waits for review in Git.
export function PullRequestNotice({ pr, repository, onDone }: { pr: PullRequest; repository?: string; onDone?: () => void }) {
  return (
    <div className="space-y-2 rounded-md border border-line bg-surface p-3" role="status">
      <p className="font-medium">Pull request #{pr.number} opened{repository && <> in <span className="font-mono">{repository}</span></>}</p>
      <p className="text-[12.5px] text-muted">Nothing has changed yet. The change applies when the pull request is merged and Argo CD syncs it.</p>
      <p className="flex gap-3">
        <a className="text-accent-text underline" href={pr.url} target="_blank" rel="noopener noreferrer">Open the pull request</a>
        {onDone && <button type="button" className="text-muted underline" onClick={onDone}>Close</button>}
      </p>
    </div>
  );
}
