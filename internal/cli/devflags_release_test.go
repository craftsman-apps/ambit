//go:build !dev

package cli

import (
	"strings"
	"testing"
)

// Ruling S9: the release build has no development flags, neither in the help nor in flag parsing.
func TestReleaseBuildRejectsDevFlags(t *testing.T) {
	_, help := runCLI(t, &fakeHelm{}, "local", "up", "--help")
	if !strings.Contains(help, "--openrouter-key-file") {
		t.Fatalf("help output is not the help of local up: %q", help)
	}
	if strings.Contains(help, "dev-") {
		t.Fatalf("help mentions a development flag: %q", help)
	}
	for _, flag := range []string{"--dev-values", "--dev-load-archive"} {
		code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", flag, "x.yaml")
		if code != 2 || !strings.Contains(s, "unknown flag: "+flag) {
			t.Errorf("%s: code %d, output %q; want 2 and unknown flag", flag, code, s)
		}
	}
}
