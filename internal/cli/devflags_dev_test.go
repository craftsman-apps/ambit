//go:build dev

package cli

import (
	"strings"
	"testing"
)

// The dev build accepts both flags: parsing passes and the command stops at its next check (no --chart-dir).
func TestDevBuildAcceptsDevFlags(t *testing.T) {
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--dev-values", "x.yaml", "--dev-load-archive", "a.tar")
	if code != 2 || strings.Contains(s, "unknown flag") || !strings.Contains(s, "--chart-dir") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestDevBuildAcceptsKeyringServiceFlag(t *testing.T) {
	r := newRig(t)
	if code := r.run("key", "print", "--url", "https://llm.example.com", "--dev-keyring-service", "ambit-test"); code != 1 || strings.Contains(r.stderr.String(), "unknown flag") {
		t.Fatalf("code %d, stderr %q", code, r.stderr.String())
	}
}
