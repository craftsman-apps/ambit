package console

import (
	"net"
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
