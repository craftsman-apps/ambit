package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/presets"
)

func TestLockProblems(t *testing.T) {
	a := presets.Preset{ID: "gh", Version: 1, Scopes: []string{"repo"}}
	changed := a
	changed.Scopes = []string{"repo", "read:org"}
	bumped := changed
	bumped.Version = 2
	verifiedOnly := a
	verifiedOnly.Verified = true
	if p := lockProblems([]presets.Preset{a}, []presets.Preset{changed}); len(p) != 1 || !strings.Contains(p[0], "gh") {
		t.Fatalf("unbumped change not caught: %v", p)
	}
	for _, head := range []presets.Preset{bumped, verifiedOnly} {
		if p := lockProblems([]presets.Preset{a}, []presets.Preset{head}); len(p) != 0 {
			t.Fatalf("false alarm: %v", p)
		}
	}
	lowered := a
	lowered.Version = 0
	if p := lockProblems([]presets.Preset{a}, []presets.Preset{lowered}); len(p) != 1 {
		t.Fatal("lowered version not caught")
	}
}

func TestBaseWithoutCatalogIsEmpty(t *testing.T) {
	base, err := baseCatalog(func() ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("fatal: path 'internal/presets/presets.yaml' does not exist in 'origin/main'")}
	})
	if err != nil || len(base) != 0 {
		t.Fatalf("base %v err %v", base, err)
	}
	if _, err := baseCatalog(func() ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("fatal: invalid object name 'origin/main'")} // shallow clone: no base ref
	}); err == nil {
		t.Fatal("a missing base branch must fail, not pass")
	}
}
