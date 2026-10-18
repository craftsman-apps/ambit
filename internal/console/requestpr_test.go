package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stateRepo answers PullRequestByBranch from a table; branches it does not know are "not found".
type stateRepo struct {
	recordingRepo
	states map[string]PullRequestState
	err    error
}

func (r *stateRepo) PullRequestByBranch(_ context.Context, b string) (PullRequestState, bool, error) {
	st, ok := r.states[b]
	return st, ok, r.err
}

// A GitOps approval is not final: the request shows the pull request until it is merged (#113).
func TestGitOpsRequestFollowsItsPullRequest(t *testing.T) {
	_, c, a := serversFixture(t)
	repo := &stateRepo{states: map[string]PullRequestState{}}
	h := NewServer(a, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS,
		GitOps: &GitOps{Repo: repo, Repository: "acme/config", Branch: "main", Dir: "ambit"}})
	alice, sam := user(a, "alice@acme.test", RoleViewer), user(a, "sam@acme.test", RoleApprover)
	ask := func(name string) RequestView {
		r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: name, Image: echoDigest})
		return decodeAs[RequestView](t, r.Body.String())
	}
	view := func(name string) RequestView {
		for _, v := range decodeAs[[]RequestView](t, do(t, h, "GET", "/api/server-requests", sam, nil).Body.String()) {
			if v.Name == name {
				return v
			}
		}
		t.Fatalf("request %s not listed", name)
		return RequestView{}
	}
	approve := func(r RequestView) *httptest.ResponseRecorder {
		return do(t, h, "POST", "/api/server-requests/"+r.Name+"/approve", sam, DecisionInput{Comment: "ok"})
	}
	branchOf := func(r RequestView) string {
		var q v1.ServerRequest
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: r.Name}, &q); err != nil {
			t.Fatal(err)
		}
		return q.Status.PullRequestBranch
	}
	req := ask("everything")
	if r := approve(req); r.Code != http.StatusAccepted {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	branch := branchOf(req)
	if branch == "" {
		t.Fatal("the branch was not recorded on the request")
	}

	// Open: "Pull request open" with the link; no decision; a second approve and a deny are refused.
	repo.states[branch] = PullRequestState{PullRequest: PullRequest{Branch: branch}}
	if v := view(req.Name); v.State != "PullRequestOpen" || v.Decision != "" || v.PullRequest != "https://github.com/acme/config/pull/1" {
		t.Fatalf("open: %+v", v)
	}
	if r := approve(req); r.Code != http.StatusConflict {
		t.Fatalf("second approve while open: %d %s", r.Code, r.Body)
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", sam, DecisionInput{}); r.Code != http.StatusConflict {
		t.Fatalf("deny while open: %d %s", r.Code, r.Body)
	}
	if len(repo.files) != 1 {
		t.Fatalf("a second pull request was opened: %d files", len(repo.files))
	}

	// The lookup fails: shown as unknown, and approve is still refused (fail safe).
	repo.err = errors.New("github is down")
	if v := view(req.Name); v.State != "PullRequestUnknown" || v.Decision != "" {
		t.Fatalf("unknown: %+v", v)
	}
	if r := approve(req); r.Code != http.StatusConflict {
		t.Fatalf("approve while unknown: %d", r.Code)
	}
	repo.err = nil

	// Closed without a merge: back to pending with a note; an approver can approve again, which opens a new pull request.
	repo.states[branch] = PullRequestState{PullRequest: PullRequest{Branch: branch}, Closed: true}
	if v := view(req.Name); v.State != "" || v.Decision != "" || v.Note != "pull request closed without merging" {
		t.Fatalf("closed: %+v", v)
	}
	if r := approve(req); r.Code != http.StatusAccepted {
		t.Fatalf("approve after close: %d %s", r.Code, r.Body)
	}
	if b2 := branchOf(req); b2 == "" || len(repo.files) != 2 {
		t.Fatalf("new branch %q, files %d", b2, len(repo.files))
	}
	if v := view(req.Name); v.PullRequest != "https://github.com/acme/config/pull/2" {
		t.Fatalf("the link is still the closed pull request: %+v", v)
	}

	// Merged: Approved by the approver who opened it, recorded once; a later approve says so.
	repo.states[branchOf(req)] = PullRequestState{PullRequest: PullRequest{Branch: branchOf(req)}, Merged: true, MergedAt: time.Now()}
	if v := view(req.Name); v.Decision != "Approved" || v.DecidedBy != "sam@acme.test" || v.State != "" || v.Comment != "ok" {
		t.Fatalf("merged: %+v", v)
	}
	var q v1.ServerRequest
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: req.Name}, &q); err != nil || q.Status.Decision != "Approved" {
		t.Fatalf("stored status %+v err %v", q.Status, err)
	}
	if r := approve(req); r.Code != http.StatusConflict {
		t.Fatalf("approve after merge: %d", r.Code)
	}

	// A request whose pull request was closed can be denied.
	other := ask("second")
	if r := approve(other); r.Code != http.StatusAccepted {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	repo.states[branchOf(other)] = PullRequestState{Closed: true}
	if r := do(t, h, "POST", "/api/server-requests/"+other.Name+"/deny", sam, DecisionInput{}); r.Code != http.StatusOK {
		t.Fatalf("deny after close: %d %s", r.Code, r.Body)
	}
}
