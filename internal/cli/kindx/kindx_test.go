package kindx

import (
	"testing"
	"time"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/kind"
)

func TestConfig(t *testing.T) {
	raw, err := Config(kind.ClusterYAML, "ambit-cli-test", 18080, 18443)
	if err != nil {
		t.Fatal(err)
	}
	var c v1alpha4.Cluster
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "ambit-cli-test" || len(c.Nodes) != 1 || len(c.KubeadmConfigPatches) != 1 {
		t.Fatalf("config = %+v", c)
	}
	n := c.Nodes[0]
	if n.Labels[Label] != LabelValue {
		t.Fatalf("labels = %v", n.Labels)
	}
	got := map[int32]int32{}
	for _, m := range n.ExtraPortMappings {
		got[m.ContainerPort] = m.HostPort
		if m.ListenAddress != "127.0.0.1" {
			t.Errorf("port %d listens on %q, want 127.0.0.1", m.ContainerPort, m.ListenAddress)
		}
	}
	if got[30080] != 18080 || got[30443] != 18443 {
		t.Fatalf("port mappings = %v", got)
	}
}

func TestParseInspect(t *testing.T) {
	c, err := parseInspect("4f1c2e7d9a0b\t/ambit-cli-test-control-plane\t2026-10-14T09:00:01.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	want := Container{ID: "4f1c2e7d9a0b", Name: "ambit-cli-test-control-plane", Created: time.Date(2026, 10, 14, 9, 0, 1, 123456789, time.UTC)}
	if c != want {
		t.Fatalf("parseInspect = %+v, want %+v", c, want)
	}
	for _, bad := range []string{"", "4f1c\t/x", "4f1c\t/x\tyesterday"} {
		if _, err := parseInspect(bad); err == nil {
			t.Errorf("parseInspect(%q) = nil error", bad)
		}
	}
}
