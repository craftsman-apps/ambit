package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/presets"
)

func TestGate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cat := []presets.Preset{{ID: "gh", Version: 1, Verified: true}, {ID: "slack", Version: 1, Verified: true}}
	good := Results{Date: now.Add(-24 * time.Hour), Commit: "abc", CatalogHash: catalogHash(cat),
		Presets: []Result{{Preset: "gh", Version: 1, Pass: true}, {Preset: "slack", Version: 1, Pass: true}}}
	noChange := func(string) ([]string, error) { return nil, nil }
	req := []string{"gh", "slack"}
	if p := gateProblems(good, now, cat, noChange, req); len(p) != 0 {
		t.Fatal(p)
	}
	cases := map[string]func(*Results) func(string) ([]string, error){
		"older than 7 days": func(r *Results) func(string) ([]string, error) { r.Date = now.Add(-8 * 24 * time.Hour); return noChange },
		"in the future":     func(r *Results) func(string) ([]string, error) { r.Date = now.Add(time.Hour); return noChange },
		"failed":            func(r *Results) func(string) ([]string, error) { r.Presets[1].Pass = false; return noChange },
		"catalog changed":   func(r *Results) func(string) ([]string, error) { r.CatalogHash = "x"; return noChange },
		"changed since":     func(r *Results) func(string) ([]string, error) { return func(string) ([]string, error) { return []string{"internal/console/broker.go"}, nil } },
		"git error":         func(r *Results) func(string) ([]string, error) { return func(string) ([]string, error) { return nil, errors.New("shallow") } },
	}
	for want, mutate := range cases {
		r := good
		r.Presets = append([]Result(nil), good.Presets...)
		changed := mutate(&r)
		if p := strings.Join(gateProblems(r, now, cat, changed, req), "\n"); !strings.Contains(p, want) {
			t.Fatalf("%s: %s", want, p)
		}
	}
	unverified := []presets.Preset{{ID: "gh", Version: 1, Verified: true}, {ID: "slack", Version: 1}}
	if p := strings.Join(gateProblems(good, now, unverified, noChange, req), "\n"); !strings.Contains(p, "slack is not verified") {
		t.Fatal(p)
	}
	// A 24th verified preset without a result also blocks the release.
	extra := append(append([]presets.Preset(nil), cat...), presets.Preset{ID: "zoom", Version: 1, Verified: true})
	r := good
	r.CatalogHash = catalogHash(extra)
	if p := strings.Join(gateProblems(r, now, extra, noChange, req), "\n"); !strings.Contains(p, "zoom has no passing result") {
		t.Fatal(p)
	}
}

func TestChangedSinceFailsInAShallowClone(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	run("add", "a")
	run("commit", "-qm", "one")
	tested := run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a"), []byte("2"), 0o644)
	run("commit", "-qam", "two")
	shallow := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+dir, shallow).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s", out)
	}
	if _, err := changedSinceIn(shallow, tested); err == nil {
		t.Fatal("a shallow clone must fail the check, not pass it")
	}
	if files, err := changedSinceIn(dir, tested); err != nil || len(files) != 0 { // "a" is not a watched path
		t.Fatalf("full history: %v %v", files, err)
	}
}
