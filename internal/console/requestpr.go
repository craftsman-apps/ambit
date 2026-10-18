package console

import (
	"context"
	"log/slog"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// In GitOps mode an approval opens a pull request and leaves the request's decision empty (a decision cannot be
// changed once set). followPR reads that pull request and shows what became of it.
const (
	statePROpen    = "PullRequestOpen"
	statePRUnknown = "PullRequestUnknown"
)

// followPR returns the state of an undecided request's pull request: statePROpen, statePRUnknown (the lookup
// failed: treated as open, to fail safe), or "" when there is nothing to wait for. Merged sets the decision to
// Approved (once) in q; closed without a merge returns a note and the request waits for an approver again.
func (c *Catalog) followPR(ctx context.Context, q *v1.ServerRequest) (state, note string) {
	if c.GitOps == nil || q.Status.Decision != "" || q.Status.PullRequestBranch == "" {
		return "", ""
	}
	st, found, err := c.GitOps.Repo.PullRequestByBranch(ctx, q.Status.PullRequestBranch)
	switch {
	case err != nil:
		slog.Warn("pull request lookup failed", "request", q.Name, "err", err)
		return statePRUnknown, ""
	case found && st.Merged:
		at := metav1.Now()
		if !st.MergedAt.IsZero() {
			at = metav1.NewTime(st.MergedAt)
		}
		q.Status.Decision, q.Status.DecidedBy, q.Status.DecidedAt = "Approved", q.Status.PullRequestBy, &at
		if err := c.Client.Status().Update(ctx, q); err != nil { // a concurrent listing settled it first: the view is the same
			slog.Warn("could not record the merged pull request", "request", q.Name, "err", err)
		}
		return "", ""
	case found && !st.Closed:
		return statePROpen, ""
	case found:
		return "", "pull request closed without merging"
	default:
		return "", "pull request not found"
	}
}
