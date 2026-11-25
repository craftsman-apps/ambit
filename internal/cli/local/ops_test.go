package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

type recordingHelm struct{ opts []helmx.ApplyOptions }

func (h *recordingHelm) Apply(_ context.Context, _, _, _ string, _ map[string]any, o helmx.ApplyOptions) (helmx.Release, error) {
	h.opts = append(h.opts, o)
	return helmx.Release{}, errors.New("stop here")
}
func (h *recordingHelm) Get(string) (*helmx.Release, error)          { return nil, nil }
func (h *recordingHelm) Values(string, bool) (map[string]any, error) { return nil, nil }
func (h *recordingHelm) List(bool) ([]helmx.Release, error)          { return nil, nil }
func (h *recordingHelm) Uninstall(string) (helmx.Release, error)     { return helmx.Release{}, nil }
func (h *recordingHelm) Render(context.Context, string, string, string, map[string]any, string) (string, error) {
	return "", nil
}

// #107: local up still recovers a pending release without asking: it installs only into a cluster it owns.
func TestLocalUpRecoversPendingReleases(t *testing.T) {
	h := &recordingHelm{}
	o := &clusterOps{h: h}
	if err := o.InstallCharts(context.Background(), Charts{Dir: "charts"}, nil, time.Second, func(string) {}); err == nil {
		t.Fatal("want the recorded error")
	}
	if len(h.opts) != 1 || !h.opts[0].RecoverPending {
		t.Fatalf("Apply options = %+v, want RecoverPending", h.opts)
	}
}

func TestChartsDepsBuilt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ambit[dev]") // brackets are not a pattern
	c := Charts{Dir: dir}
	deps := filepath.Join(dir, "ambit", "charts")
	os.MkdirAll(deps, 0o755)
	if c.DepsBuilt() {
		t.Fatal("an empty dependency folder is not built")
	}
	os.WriteFile(filepath.Join(deps, "agentgateway-v2.45.1.tgz"), []byte("x"), 0o644)
	if !c.DepsBuilt() {
		t.Fatal("a packed dependency was not found")
	}
}
