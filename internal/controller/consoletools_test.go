package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestConsoleToolsForPerUserServer(t *testing.T) {
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tools/jira-mcp":
			w.Write([]byte(`{"tools":[{"name":"get_issue","fingerprint":"f1"}],"reader":"admin@acme.test"}`))
		case "/v1/tools/unbound":
			w.WriteHeader(409)
			w.Write([]byte(`{"reason":"server is not bound to its connection"}`))
		case "/v1/tools/broken":
			w.WriteHeader(502)
			w.Write([]byte(`{"reason":"vendor read failed"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"reason":"no reader"}`))
		}
	}))
	defer console.Close()
	list := WithConsoleTools(func(context.Context, v1.Server) ([]v1.Tool, error) { t.Fatal("base lister used"); return nil, nil },
		ConsoleTools{URL: console.URL})
	tools, err := list(t.Context(), perUser("jira-mcp", "https://mcp.jira.test/mcp", "jira"))
	if err != nil || len(tools) != 1 || tools[0].Name != "get_issue" {
		t.Fatalf("tools %v err %v", tools, err)
	}
	for _, name := range []string{"other", "unbound"} { // 404 and 409 both mean: no reader
		if _, err := list(t.Context(), perUser(name, "https://x.test/mcp", "jira")); !errors.Is(err, ErrNoReader) {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	if _, err := list(t.Context(), perUser("broken", "https://x.test/mcp", "jira")); err == nil || errors.Is(err, ErrNoReader) {
		t.Fatalf("502 must be a normal error, got %v", err)
	}
}

func TestConsoleToolsLeavesOtherServersToBase(t *testing.T) {
	called := false
	list := WithConsoleTools(func(context.Context, v1.Server) ([]v1.Tool, error) { called = true; return nil, nil }, ConsoleTools{URL: "http://unused.invalid"})
	if _, err := list(t.Context(), perUser("plain", "https://x.test/mcp", "")); err != nil || !called {
		t.Fatalf("called %v err %v", called, err)
	}
}

func perUserEnv(ns string) *v1.Server {
	return &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "jira-mcp", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://mcp.jira.test/mcp", Connection: "jira", Transport: "streamable-http"}}
}

func TestPerUserToolChangeIsCaughtByPinning(t *testing.T) {
	needEnv(t)
	ns := "ambit-reader-pin"
	namespace(t, ns)
	ctx := t.Context()
	s := perUserEnv(ns)
	if err := envClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	fp := "f1"
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, ToolsRefresh: time.Nanosecond,
		ListTools: func(context.Context, v1.Server) ([]v1.Tool, error) {
			return []v1.Tool{{Name: "get_issue", Fingerprint: fp}}, nil
		}}
	_, _ = r.Reconcile(ctx, reconcile.Request{})
	fp = "f2-changed-description"
	_, _ = r.Reconcile(ctx, reconcile.Request{})
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(s), s)
	if c := meta.FindStatusCondition(s.Status.Conditions, "ToolsPinned"); c == nil || c.Reason != "ToolsChanged" {
		t.Fatalf("cond %+v", c)
	}
}

func TestReaderUnavailableKeepsLastList(t *testing.T) {
	needEnv(t)
	ns := "ambit-reader"
	namespace(t, ns)
	ctx := t.Context()
	s := perUserEnv(ns)
	if err := envClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, ToolsRefresh: time.Nanosecond,
		ListTools: func(context.Context, v1.Server) ([]v1.Tool, error) {
			calls++
			if calls == 1 {
				return []v1.Tool{{Name: "get_issue", Fingerprint: "f1"}}, nil
			}
			return nil, ErrNoReader
		}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(s), s)
	c := meta.FindStatusCondition(s.Status.Conditions, "ToolsRead")
	if len(s.Status.Tools) != 1 || c == nil || c.Reason != "ReaderUnavailable" {
		t.Fatalf("tools %v cond %+v", s.Status.Tools, c)
	}
}
