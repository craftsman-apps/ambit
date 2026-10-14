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
