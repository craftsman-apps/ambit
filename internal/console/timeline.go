package console

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Step is one stage of a console change in GitOps mode: opened → merged → synced → applied.
type Step struct {
	Name   string `json:"name"`
	State  string `json:"state"` // done, waiting, failed, or skipped
	Detail string `json:"detail,omitempty"`
}

// Change is the timeline of one pull request the console opened.
type Change struct {
	PullRequestState
	Verb  string `json:"verb"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
}

// ambitKinds are the objects the console proposes, by the lower-case kind in its branch names.
var ambitKinds = map[string]func() client.Object{
	"accessrule": func() client.Object { return &v1.AccessRule{} },
	"budget":     func() client.Object { return &v1.Budget{} },
	"connection": func() client.Object { return &v1.Connection{} },
	"server":     func() client.Object { return &v1.Server{} },
}

// parseBranch reads ambit/<verb>-<kind>-<name>-<unix seconds>, the branch Propose creates.
func parseBranch(branch string) (verb, kind, name string, ok bool) {
	rest, ok := strings.CutPrefix(branch, branchPrefix)
	if !ok {
		return "", "", "", false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 4 {
		return "", "", "", false
	}
	if _, err := strconv.ParseInt(parts[len(parts)-1], 10, 64); err != nil {
		return "", "", "", false
	}
	verb, kind, name = parts[0], parts[1], strings.Join(parts[2:len(parts)-1], "-")
	_, known := ambitKinds[kind]
	return verb, kind, name, known
}

// timeline builds the steps of pull request number. The object's status counts only once Argo CD has
// synced the merge, so the page never reports the old version as the change.
func (c *Catalog) timeline(ctx context.Context, number int) (Change, error) {
	st, err := c.GitOps.Repo.PullRequestState(ctx, number)
	if err != nil {
		return Change{}, err
	}
	verb, kind, name, ok := parseBranch(st.Branch)
	if !ok {
		return Change{}, fmt.Errorf("pull request #%d was not opened by the Ambit console", number)
	}
	ch := Change{PullRequestState: st, Verb: verb, Kind: kind, Name: name,
		Steps: []Step{{Name: "Pull request opened", State: "done", Detail: "#" + strconv.Itoa(number)}}}

	merged := Step{Name: "Merged", State: "waiting", Detail: "waiting for a reviewer"}
	switch {
	case st.Merged:
		merged = Step{Name: "Merged", State: "done", Detail: shortSHA(st.MergeCommit)}
	case st.Closed:
		merged = Step{Name: "Merged", State: "failed", Detail: "closed without merging; nothing changed"}
	}
	ch.Steps = append(ch.Steps, merged)
	if !st.Merged {
		return ch, nil
	}

	sync := c.synced(ctx, st.MergeCommit)
	ch.Steps = append(ch.Steps, sync)
	switch sync.State { // until Argo CD has synced the merge, the cluster's status describes the old version
	case "waiting":
		ch.Steps = append(ch.Steps, Step{Name: "Applied", State: "waiting", Detail: "waiting for Argo CD"})
		return ch, nil
	case "skipped":
		ch.Steps = append(ch.Steps, Step{Name: "Applied", State: "skipped", Detail: "unknown without Argo CD: the cluster may still have the old version"})
		return ch, nil
	}
	ch.Steps = append(ch.Steps, c.applied(ctx, verb, kind, name))
	return ch, nil
}

func shortSHA(sha string) string { return sha[:min(7, len(sha))] }

// synced reads the Argo CD Application: Synced at a revision that includes the merge commit.
func (c *Catalog) synced(ctx context.Context, commit string) Step {
	s := Step{Name: "Synced by Argo CD"}
	ns, name, ok := strings.Cut(c.ArgoApplication, "/")
	if !ok {
		s.State, s.Detail = "skipped", "Argo CD is not configured (gitops.argocd.application)"
		return s
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"})
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, app); err != nil {
		slog.Error("read Argo CD Application", "application", c.ArgoApplication, "err", err)
		s.State, s.Detail = "waiting", "cannot read Argo CD Application "+c.ArgoApplication
		return s
	}
	status, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	revision, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
	revisions, _, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions") // multi-source
	if revision != "" {
		revisions = []string{revision}
	}
	if status != "Synced" || len(revisions) == 0 {
		s.State, s.Detail = "waiting", "Argo CD application is "+strings.ToLower(orDefault(status, "unknown"))
		return s
	}
	for _, rev := range revisions { // a revision of another source's repository does not compare: skip it
		if c.contains(ctx, commit, rev) {
			s.State, s.Detail = "done", shortSHA(rev)
			return s
		}
	}
	s.State, s.Detail = "waiting", "synced at "+shortSHA(revisions[0])+", which does not include the change yet"
	return s
}

// contains asks GitHub once per commit and revision pair: the answer never changes once true.
func (c *Catalog) contains(ctx context.Context, commit, revision string) bool {
	key := commit + "..." + revision
	if _, ok := c.containsCache.Load(key); ok {
		return true
	}
	in, err := c.GitOps.Repo.Contains(ctx, commit, revision)
	if err != nil {
		slog.Warn("compare revisions", "commit", commit, "revision", revision, "err", err)
		return false
	}
	if in {
		c.containsCache.Store(key, true)
	}
	return in
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// applied checks the Ambit object: gone after a delete; otherwise its controller has reported Ready on
// the current generation.
func (c *Catalog) applied(ctx context.Context, verb, kind, name string) Step {
	s := Step{Name: "Applied"}
	obj := ambitKinds[kind]()
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, obj)
	if verb == "delete" {
		switch {
		case apierrors.IsNotFound(err):
			s.State, s.Detail = "done", "removed"
		case err != nil:
			s.State, s.Detail = "waiting", "cannot read "+kind+" "+name
		default:
			s.State, s.Detail = "waiting", "still in the cluster"
		}
		return s
	}
	if apierrors.IsNotFound(err) {
		s.State, s.Detail = "waiting", "not in the cluster yet"
		return s
	}
	if err != nil {
		s.State, s.Detail = "waiting", "cannot read "+kind+" "+name
		return s
	}
	if kind == "connection" { // no controller reports on Connections: Argo CD applying it is the change
		s.State, s.Detail = "done", "in the cluster"
		return s
	}
	ready := meta.FindStatusCondition(conditionsOf(obj), "Ready")
	switch {
	case ready == nil || ready.ObservedGeneration != obj.GetGeneration():
		s.State, s.Detail = "waiting", "the controller has not checked this version yet"
	case ready.Status == metav1.ConditionTrue:
		s.State, s.Detail = "done", orDefault(ready.Message, "ready")
	case ready.Status == metav1.ConditionFalse:
		s.State, s.Detail = "failed", ready.Message
	default:
		s.State, s.Detail = "waiting", orDefault(ready.Message, "not ready yet")
	}
	return s
}

func conditionsOf(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *v1.AccessRule:
		return o.Status.Conditions
	case *v1.Budget:
		return o.Status.Conditions
	case *v1.Server:
		return o.Status.Conditions
	}
	return nil
}

func (c *Catalog) change(w http.ResponseWriter, r *http.Request, _ Session) {
	if c.GitOps == nil {
		writeErr(w, http.StatusNotFound, "the console is not in GitOps mode")
		return
	}
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || n < 1 {
		writeErr(w, http.StatusBadRequest, "number must be a pull request number")
		return
	}
	ch, err := c.timeline(r.Context(), n)
	if err != nil {
		slog.Error("change timeline", "number", n, "err", err)
		writeErr(w, http.StatusBadGateway, "could not read pull request #"+strconv.Itoa(n)+" in "+c.GitOps.Repository+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ch)
}
