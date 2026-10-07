package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		"older than 7 days": func(r *Results) func(string) ([]string, error) {
			r.Date = now.Add(-8 * 24 * time.Hour)
			return noChange
		},
		"in the future":   func(r *Results) func(string) ([]string, error) { r.Date = now.Add(time.Hour); return noChange },
		"failed":          func(r *Results) func(string) ([]string, error) { r.Presets[1].Pass = false; return noChange },
		"catalog changed": func(r *Results) func(string) ([]string, error) { r.CatalogHash = "x"; return noChange },
		"changed since": func(r *Results) func(string) ([]string, error) {
			return func(string) ([]string, error) { return []string{"internal/console/broker.go"}, nil }
		},
		"git error": func(r *Results) func(string) ([]string, error) {
			return func(string) ([]string, error) { return nil, errors.New("shallow") }
		},
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

func TestGateIgnoresAPassForAnotherVersion(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cat := []presets.Preset{{ID: "gh", Version: 2, Verified: true}}
	r := Results{Date: now, Commit: "abc", CatalogHash: catalogHash(cat), Presets: []Result{{Preset: "gh", Version: 1, Pass: true}}}
	p := strings.Join(gateProblems(r, now, cat, func(string) ([]string, error) { return nil, nil }, []string{"gh"}), "\n")
	if !strings.Contains(p, "gh has no passing result") {
		t.Fatal(p)
	}
}

func TestChangedSinceInRealHistory(t *testing.T) {
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
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalogYAML := func(url, verifiedOn string) string {
		s := "- id: gh\n  version: 1\n  server: {url: " + url + ", transport: http}\n"
		if verifiedOn != "" {
			s += "  verified: true\n  verifiedOn: \"" + verifiedOn + "\"\n"
		}
		return s
	}
	commit := func(msg string) string { run("add", "-A"); run("commit", "-qm", msg); return run("rev-parse", "HEAD") }
	run("init", "-q", "-b", "main")
	write("internal/presets/presets.yaml", catalogYAML("https://a.example/mcp", ""))
	tested := commit("one")

	write("internal/presets/presets.yaml", catalogYAML("https://a.example/mcp", "2026-11-01"))
	commit("verify only")
	if files, err := changedSinceIn(dir, tested); err != nil || len(files) != 0 {
		t.Fatalf("a verifiedOn-only edit must be excused: %v %v", files, err)
	}

	write("internal/console/x.go", "package console\n")
	commit("console change")
	if files, err := changedSinceIn(dir, tested); err != nil || len(files) != 1 || files[0] != "internal/console/x.go" {
		t.Fatalf("a console change must be reported: %v %v", files, err)
	}

	write("api/v1alpha1/types.go", "package v1alpha1\n") // TokenRequest reads the Connection type
	commit("api change")
	if files, err := changedSinceIn(dir, tested); err != nil || !strings.Contains(strings.Join(files, " "), "api/v1alpha1/types.go") {
		t.Fatalf("an API type change must be reported: %v %v", files, err)
	}

	write("internal/presets/presets.yaml", catalogYAML("https://b.example/mcp", "2026-11-01"))
	commit("url change")
	files, err := changedSinceIn(dir, tested)
	if err != nil || !strings.Contains(strings.Join(files, " "), "internal/presets/presets.yaml") {
		t.Fatalf("a URL change must be reported: %v %v", files, err)
	}

	for _, bad := range []string{"HEAD", "main", "HEAD~1", "--all", ""} {
		if _, err := changedSinceIn(dir, bad); err == nil || !strings.Contains(err.Error(), "full commit id") {
			t.Fatalf("%q must be refused: %v", bad, err)
		}
	}

	run("checkout", "-q", "-b", "side", tested)
	write("other", "x")
	side := commit("side")
	run("checkout", "-q", "main")
	if _, err := changedSinceIn(dir, side); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("a non-ancestor commit must fail: %v", err)
	}
}

// The initial release ships 21 presets; PayPal (dynamic client registration) and Salesforce wait for phase 2.
func TestRequiredAreTheInitial21(t *testing.T) {
	want := []string{"github", "linear", "asana", "monday", "airtable", "box", "dropbox", "canva", "gmail",
		"google-calendar", "google-drive", "google-docs", "google-sheets", "slack", "zoom", "webex", "hubspot",
		"docusign", "servicenow", "databricks", "pagerduty"}
	if !slices.Equal(required, want) {
		t.Fatalf("required = %v", required)
	}
}

// Results from a partial run (make preset-live APP=<id>) never pass the gate.
func TestGateRefusesPartialResults(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cat := []presets.Preset{{ID: "gh", Version: 1, Verified: true}}
	r := Results{Date: now.Add(-time.Hour), Commit: "abc", CatalogHash: catalogHash(cat), Partial: true,
		Presets: []Result{{Preset: "gh", Version: 1, Pass: true}}}
	p := gateProblems(r, now, cat, func(string) ([]string, error) { return nil, nil }, []string{"gh"})
	if !strings.Contains(strings.Join(p, "\n"), "partial") {
		t.Fatalf("partial results accepted: %v", p)
	}
}
