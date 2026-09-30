package console

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recordingRepo is a Git host that remembers what the console proposed.
type recordingRepo struct{ files []FileChange }

func (r *recordingRepo) OpenPullRequest(_ context.Context, branch, title, _ string, f FileChange) (PullRequest, error) {
	r.files = append(r.files, f)
	return PullRequest{Number: len(r.files), URL: "https://github.com/acme/config/pull/" + itoa(len(r.files)), Title: title, Branch: branch}, nil
}
func (r *recordingRepo) OpenPullRequests(context.Context) ([]PullRequest, error) { return nil, nil }

func gitopsFixture(t *testing.T) (http.Handler, client.Client, *Auth, *recordingRepo) {
	t.Helper()
	_, c, a := serversFixture(t)
	repo := &recordingRepo{}
	h := NewServer(a, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS,
		GitOps: &GitOps{Repo: repo, Repository: "acme/config", Branch: "main", Dir: "ambit"}})
	return h, c, a, repo
}

func TestGitOpsRuleChangesBecomePullRequests(t *testing.T) {
	h, c, a, repo := gitopsFixture(t)
	admin := user(a, "priya@acme.test", RoleAdmin)
	if cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", admin, nil).Body.String()); cfg.Mode != "gitops" || cfg.Repository != "acme/config" {
		t.Fatalf("config %+v", cfg)
	}
	r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "ml-time", Groups: []string{"ml-research"}, Servers: []v1.ServerAccess{{Name: "time"}}})
	if r.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	if p := decodeAs[Proposed](t, r.Body.String()); p.PullRequest.Number != 1 || p.PullRequest.Title != "Ambit: create access rule ml-time" {
		t.Fatalf("proposed %+v", p)
	}
	var ar v1.AccessRule
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "ml-time"}, &ar); err == nil {
		t.Fatal("GitOps mode wrote to the cluster")
	}
	f := repo.files[0]
	if f.Path != "ambit/accessrules/ml-time.yaml" || !strings.Contains(string(f.Content), "- ml-research") ||
		!strings.Contains(string(f.Content), "ambitproject.dev/created-by: priya@acme.test") || strings.Contains(string(f.Content), "namespace:") {
		t.Fatalf("file %s:\n%s", f.Path, f.Content)
	}

	// eng-time is synced from Git (Argo CD's annotation): in GitOps mode it is edited by pull request, not refused.
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng-time"}, &ar)
	ar.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "ambit-config:ambitproject.dev/AccessRule:ambit-system/eng-time"}
	c.Update(t.Context(), &ar)
	r = do(t, h, "PUT", "/api/access-rules/eng-time", admin, RuleInput{Groups: []string{"eng", "finance"}})
	if r.Code != http.StatusAccepted || strings.Contains(string(repo.files[1].Content), "argocd") {
		t.Fatalf("update: %d %s\n%s", r.Code, r.Body, repo.files[1].Content)
	}
	if !strings.Contains(string(repo.files[1].Content), "get_current_time") {
		t.Fatalf("update dropped the rule's servers:\n%s", repo.files[1].Content)
	}
	if r := do(t, h, "DELETE", "/api/access-rules/eng-time", admin, nil); r.Code != http.StatusAccepted || repo.files[2].Content != nil {
		t.Fatalf("delete: %d %+v", r.Code, repo.files[2])
	}
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng-time"}, &ar)
	if len(ar.Spec.Groups) != 1 {
		t.Fatalf("cluster changed: %+v", ar.Spec)
	}
}

func TestGitOpsApprovalOpensAPullRequestInsteadOfCreatingTheServer(t *testing.T) {
	h, c, a, repo := gitopsFixture(t)
	r := do(t, h, "POST", "/api/server-requests", user(a, "alice@acme.test", RoleViewer), RequestInput{ServerName: "everything", Image: echoDigest})
	req := decodeAs[RequestView](t, r.Body.String())
	r = do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", user(a, "sam@acme.test", RoleApprover), DecisionInput{Comment: "ok"})
	if r.Code != http.StatusAccepted {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	var srv v1.Server
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everything"}, &srv); err == nil {
		t.Fatal("server created before the pull request was merged")
	}
	f := repo.files[0]
	if f.Path != "ambit/servers/everything.yaml" || !strings.Contains(string(f.Content), echoDigest) ||
		!strings.Contains(string(f.Content), "ambitproject.dev/approved-by: sam@acme.test") {
		t.Fatalf("file %s:\n%s", f.Path, f.Content)
	}
	var q v1.ServerRequest
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: req.Name}, &q)
	if q.Status.Decision != "Approved" || q.Status.PullRequest != "https://github.com/acme/config/pull/1" {
		t.Fatalf("request status %+v", q.Status)
	}
}

func TestDirectModeReportsItself(t *testing.T) {
	h, _, a := serversFixture(t)
	if cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "x@acme.test", RoleViewer), nil).Body.String()); cfg.Mode != "direct" {
		t.Fatalf("%+v", cfg)
	}
}
