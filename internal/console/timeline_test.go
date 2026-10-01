package console

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// timelineRepo is a Git host with one pull request and a linear history: main1 → merge1 → main2.
type timelineRepo struct {
	recordingRepo
	state PullRequestState
}

func (r *timelineRepo) PullRequestState(context.Context, int) (PullRequestState, error) {
	return r.state, nil
}
func (r *timelineRepo) Contains(_ context.Context, commit, revision string) (bool, error) {
	order := map[string]int{"main1": 1, "merge1": 2, "main2": 3}
	return order[revision] >= order[commit], nil
}

func argoApp(status, revision string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "ambit-config", "namespace": "argocd"},
		"status":   map[string]any{"sync": map[string]any{"status": status, "revision": revision}},
	}}
	return u
}

func TestChangeTimeline(t *testing.T) {
	branch := "ambit/update-accessrule-eng-time-1759300000"
	open := PullRequestState{PullRequest: PullRequest{Number: 12, Branch: branch}}
	merged := PullRequestState{PullRequest: PullRequest{Number: 12, Branch: branch}, Merged: true, MergeCommit: "merge1", MergedAt: time.Now()}
	rule := func(gen, observed int64, status metav1.ConditionStatus) *v1.AccessRule {
		return &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng-time", Namespace: testNS, Generation: gen},
			Status: v1.AccessRuleStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: status, ObservedGeneration: observed, Message: "server time is not running"}}}}
	}
	cases := []struct {
		name string
		pr   PullRequestState
		app  *unstructured.Unstructured // nil: Argo CD not configured
		rule *v1.AccessRule
		verb string
		want []string // state of each step
		last string   // detail of the last step
	}{
		{"open", open, argoApp("Synced", "main1"), rule(1, 1, "True"), "", []string{"done", "waiting"}, "waiting for a reviewer"},
		{"closed", PullRequestState{PullRequest: open.PullRequest, Closed: true}, nil, nil, "", []string{"done", "failed"}, "closed without merging; nothing changed"},
		{"merged, Argo CD at an older revision", merged, argoApp("Synced", "main1"), rule(1, 1, "True"), "", []string{"done", "done", "waiting", "waiting"}, "waiting for Argo CD"},
		{"merged, Argo CD out of sync", merged, argoApp("OutOfSync", "merge1"), rule(2, 1, "True"), "", []string{"done", "done", "waiting", "waiting"}, "waiting for Argo CD"},
		{"synced, controller not yet", merged, argoApp("Synced", "merge1"), rule(2, 1, "True"), "", []string{"done", "done", "done", "waiting"}, "the controller has not checked this version yet"},
		{"synced at a later commit, ready", merged, argoApp("Synced", "main2"), rule(2, 2, "True"), "", []string{"done", "done", "done", "done"}, "server time is not running"},
		{"synced, not ready", merged, argoApp("Synced", "merge1"), rule(2, 2, "False"), "", []string{"done", "done", "done", "failed"}, "server time is not running"},
		{"no Argo CD configured", merged, nil, rule(2, 2, "True"), "", []string{"done", "done", "skipped", "done"}, "server time is not running"},
		{"delete applied", merged, argoApp("Synced", "merge1"), nil, "delete", []string{"done", "done", "done", "done"}, "removed"},
		{"delete not yet applied", merged, argoApp("Synced", "merge1"), rule(1, 1, "True"), "delete", []string{"done", "done", "done", "waiting"}, "still in the cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = v1.AddToScheme(scheme)
			b := fake.NewClientBuilder().WithScheme(scheme)
			argo := ""
			if tc.app != nil {
				b, argo = b.WithObjects(tc.app), "argocd/ambit-config"
			}
			if tc.rule != nil {
				b = b.WithObjects(tc.rule)
			}
			pr := tc.pr
			if tc.verb == "delete" {
				pr.Branch = "ambit/delete-accessrule-eng-time-1759300000"
			}
			repo := &timelineRepo{state: pr}
			auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
			h := NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: b.Build(), Namespace: testNS, ArgoApplication: argo,
				GitOps: &GitOps{Repo: repo, Repository: "acme/config", Branch: "main", Dir: "ambit"}})
			r := do(t, h, "GET", "/api/changes/12", user(auth, "priya@acme.test", RoleAdmin), nil)
			if r.Code != http.StatusOK {
				t.Fatalf("%d %s", r.Code, r.Body)
			}
			ch := decodeAs[Change](t, r.Body.String())
			var got []string
			for _, s := range ch.Steps {
				got = append(got, s.State)
			}
			if len(got) != len(tc.want) || ch.Kind != "accessrule" || ch.Name != "eng-time" {
				t.Fatalf("steps %v (kind %s, name %s), want %v", got, ch.Kind, ch.Name, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("steps %v, want %v: %+v", got, tc.want, ch.Steps)
				}
			}
			if d := ch.Steps[len(ch.Steps)-1].Detail; d != tc.last {
				t.Errorf("last step detail %q, want %q", d, tc.last)
			}
		})
	}
}

func TestParseBranch(t *testing.T) {
	for branch, want := range map[string][3]string{
		"ambit/update-accessrule-eng-time-1759300000": {"update", "accessrule", "eng-time"},
		"ambit/add-server-time-b-1759300000":          {"add", "server", "time-b"},
		"ambit/delete-budget-eng-1759300000":          {"delete", "budget", "eng"},
	} {
		v, k, n, ok := parseBranch(branch)
		if !ok || [3]string{v, k, n} != want {
			t.Errorf("%s: %s %s %s %v", branch, v, k, n, ok)
		}
	}
	for _, bad := range []string{"renovate/chart", "ambit/update-accessrule-1759300000", "ambit/update-secret-x-1759300000", "ambit/update-accessrule-x-y"} {
		if _, _, _, ok := parseBranch(bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}
