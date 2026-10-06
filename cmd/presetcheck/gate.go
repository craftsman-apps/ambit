package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/presets"
)

var fullCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

var watched = []string{"api/v1alpha1", "internal/console", "internal/presets", "cmd/presetcheck", "internal/controller/tools.go", "go.mod", "go.sum"}

// required: the 22 initial presets. PayPal waits for dynamic client registration (phase 2): its MCP server accepts only self-registered clients.
var required = []string{"github", "linear", "asana", "monday", "airtable", "box", "dropbox", "canva", "gmail", "google-calendar",
	"google-drive", "google-docs", "google-sheets", "slack", "zoom", "webex", "hubspot", "salesforce", "docusign",
	"servicenow", "databricks", "pagerduty"}

func gateProblems(r Results, now time.Time, catalog []presets.Preset, changedSince func(string) ([]string, error), required []string) []string {
	var p []string
	switch {
	case r.Date.After(now):
		p = append(p, "results are dated in the future")
	case now.Sub(r.Date) > 7*24*time.Hour:
		p = append(p, "results are older than 7 days; run make preset-live")
	}
	if r.CatalogHash != catalogHash(catalog) {
		p = append(p, "catalog changed since the live run")
	}
	files, err := changedSince(r.Commit)
	if err != nil {
		p = append(p, "git error (the release job needs full history): "+err.Error())
	}
	for _, f := range files {
		p = append(p, "changed since the live run: "+f)
	}
	verified, passed, version := map[string]bool{}, map[string]bool{}, map[string]int{}
	for _, c := range catalog {
		verified[c.ID], version[c.ID] = c.Verified, c.Version
	}
	for _, x := range r.Presets {
		passed[x.Preset] = x.Pass && x.Version == version[x.Preset] // a pass for another version does not count
		if !x.Pass && verified[x.Preset] {
			p = append(p, x.Preset+" failed: "+x.Error)
		}
	}
	for _, id := range required {
		if !verified[id] {
			p = append(p, id+" is not verified")
		}
	}
	for _, c := range catalog { // every verified preset, not only the initial 22
		if c.Verified && !passed[c.ID] {
			p = append(p, c.ID+" has no passing result")
		}
	}
	return p
}

// changedSinceIn lists watched files changed between the tested commit and HEAD in dir. Any Git error
// (for example a shallow clone without the tested commit) is returned, never treated as "no change".
func changedSinceIn(dir, commit string) ([]string, error) {
	// A name such as HEAD, a branch or a tag would pass the checks below without testing anything.
	if !fullCommitRE.MatchString(commit) {
		return nil, fmt.Errorf("results.json must name a full commit id, got %q", commit)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return strings.TrimSpace(string(out)), err
	}
	shallow, err := git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, fmt.Errorf("the repository is unreadable: %w", err)
	}
	if shallow == "true" {
		return nil, errors.New("the repository is shallow (fetch-depth: 0 is required)")
	}
	if _, err := git("merge-base", "--is-ancestor", "--end-of-options", commit, "HEAD"); err != nil {
		return nil, fmt.Errorf("the tested commit %s is not an ancestor of HEAD: %w", commit, err)
	}
	out, err := git(append([]string{"diff", "--name-only", "--end-of-options", commit, "HEAD", "--"}, watched...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Fields(out) {
		if f == "internal/presets/presets.yaml" && onlyVerifiedChanged(dir, commit) {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

// onlyVerifiedChanged: the catalog differs only in verified/verifiedOn (Hash leaves them out).
func onlyVerifiedChanged(dir, commit string) bool {
	show := func(rev string) ([]presets.Preset, error) {
		cmd := exec.Command("git", "show", rev+":internal/presets/presets.yaml")
		cmd.Dir = dir
		raw, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		return presets.Parse(raw)
	}
	a, err1 := show(commit)
	b, err2 := show("HEAD")
	return err1 == nil && err2 == nil && catalogHash(a) == catalogHash(b)
}

// gate runs from the repository root (the release job's working directory).
func gate() error {
	raw, err := os.ReadFile("test/presets/results.json")
	if err != nil {
		return err
	}
	var r Results
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("test/presets/results.json: %w", err)
	}
	catalog, err := presets.All()
	if err != nil {
		return err
	}
	probs := gateProblems(r, time.Now().UTC(), catalog, func(c string) ([]string, error) { return changedSinceIn(".", c) }, required)
	if len(probs) > 0 {
		return fmt.Errorf("release gate failed:\n  %s", joinLines(probs))
	}
	return nil
}
