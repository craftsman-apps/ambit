package console

import (
	"net"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDependenciesReport(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := ln.Addr().String()
	ln.Close() // nothing listens: Valkey is down

	deploy := func(name string, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)}, Status: appsv1.DeploymentStatus{ReadyReplicas: ready}}
	}
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deploy("ambit", 2), deploy("ambit-dex", 1), deploy("ambit-ratelimit", 0)).Build() // no ambit-guard
	d := &Dependencies{Client: c, Namespace: testNS, Budgets: &Budgets{Valkey: Valkey{Addr: down}},
		RatelimitMode: "FailClosed", Protection: true}

	got := map[string]Dependency{}
	for _, x := range d.Report(t.Context()) {
		got[x.Name] = x
	}
	want := map[string]struct {
		healthy bool
		mode    string
	}{
		"Gateway": {true, "required"}, "Dex": {true, "partial"}, "Rate limit service": {false, "fail-closed"},
		"Valkey": {false, "fail-closed"}, "ambit-guard": {false, "fail-closed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d dependencies, want %d (no evidence: no ClickHouse rows): %+v", len(got), len(want), got)
	}
	for name, w := range want {
		if g := got[name]; g.Healthy != w.healthy || g.Mode != w.mode {
			t.Errorf("%s: healthy %v mode %s (%s), want healthy %v mode %s", name, g.Healthy, g.Mode, g.Detail, w.healthy, w.mode)
		}
	}
	if got["Gateway"].Detail != "2 of 2 pods ready" || got["Rate limit service"].Detail != "0 of 2 pods ready" {
		t.Errorf("details: %q, %q", got["Gateway"].Detail, got["Rate limit service"].Detail)
	}
	if g := got["ambit-guard"].Detail; g == "" {
		t.Error("a missing Deployment should say why it is unhealthy")
	}
}

// Only admins see dependencies (component names, addresses, pod counts).
func TestDependenciesEndpointIsAdminOnly(t *testing.T) {
	_, c, auth := serversFixture(t)
	d := &Dependencies{Client: c, Namespace: testNS, Budgets: &Budgets{Valkey: Valkey{Addr: "127.0.0.1:1"}}}
	h := NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, Dependencies: d})
	for _, tc := range []struct {
		who  *http.Cookie
		want int
	}{
		{nil, http.StatusUnauthorized},
		{user(auth, "alice@acme.test", RoleViewer), http.StatusForbidden},
		{user(auth, "sam@acme.test", RoleApprover), http.StatusForbidden},
	} {
		if r := do(t, h, "GET", "/api/dependencies", tc.who, nil); r.Code != tc.want || strings.Contains(r.Body.String(), "Valkey") {
			t.Errorf("%v: %d %s", tc.who, r.Code, r.Body)
		}
	}
	r := do(t, h, "GET", "/api/dependencies", user(auth, "priya@acme.test", RoleAdmin), nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"name":"Valkey"`) {
		t.Fatalf("admin: %d %s", r.Code, r.Body)
	}
}

// ambit-router at 0 replicas is off on purpose (no router of type LLM): healthy. Some replicas missing: requests of
// LLM routers get no label.
func TestModelRoutingDependency(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	router := func(want, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ambit-router", Namespace: testNS},
			Spec: appsv1.DeploymentSpec{Replicas: ptr.To(want)}, Status: appsv1.DeploymentStatus{ReadyReplicas: ready}}
	}
	row := func(d *Dependencies) (Dependency, bool) {
		for _, x := range d.Report(t.Context()) {
			if x.Name == "Model routing classifier" {
				return x, true
			}
		}
		return Dependency{}, false
	}
	for name, tc := range map[string]struct {
		dep     *appsv1.Deployment
		healthy bool
		detail  string
	}{
		"off":     {router(0, 0), true, "Off: no router of type LLM"},
		"ready":   {router(2, 2), true, "2 of 2 pods ready"},
		"partial": {router(2, 1), false, "1 of 2 pods ready; requests use no label"},
	} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.dep).Build()
		g, ok := row(&Dependencies{Client: c, Namespace: testNS, ModelRouting: true})
		if !ok || g.Healthy != tc.healthy || g.Detail != tc.detail || g.Mode != "fail-open" {
			t.Errorf("%s: %+v", name, g)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, ok := row(&Dependencies{Client: c, Namespace: testNS}); ok {
		t.Error("no row when ambit-router is not installed")
	}
}
