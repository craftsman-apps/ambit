package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalUpDevBuildNeedsChartDir(t *testing.T) {
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key")
	if code != 2 || !strings.Contains(s, "next: ambit local up --chart-dir <path to charts/ in a checkout>") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestLocalUpChartDirChecks(t *testing.T) {
	dir := t.TempDir()
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--chart-dir", dir)
	if code != 2 || !strings.Contains(s, "has no chart ambit") {
		t.Fatalf("empty dir: code %d, output %q", code, s)
	}
	os.MkdirAll(filepath.Join(dir, "ambit"), 0o755)
	os.MkdirAll(filepath.Join(dir, "ambit-crds"), 0o755)
	code, s = runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--chart-dir", dir)
	if code != 2 || !strings.Contains(s, "next: helm dependency build "+filepath.Join(dir, "ambit")) {
		t.Fatalf("no dependencies: code %d, output %q", code, s)
	}
}

func TestLocalUpKeyFlagsConflict(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "0.2.0"
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--openrouter-key-file", "k.txt")
	if code != 2 || !strings.Contains(s, "cannot be used together") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestLocalRejectsJSON(t *testing.T) {
	if code, _ := runCLI(t, &fakeHelm{}, "local", "password", "--output", "json"); code != 2 {
		t.Fatalf("code %d, want 2", code)
	}
}
