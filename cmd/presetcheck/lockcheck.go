package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/craftsman-apps/ambit/internal/presets"
)

// lockProblems: a preset whose content changed must have a higher version than on the base branch.
// Added presets and removed presets are not problems.
func lockProblems(base, head []presets.Preset) []string {
	old := map[string]presets.Preset{}
	for _, p := range base {
		old[p.ID] = p
	}
	var out []string
	for _, p := range head {
		b, ok := old[p.ID]
		switch {
		case !ok:
		case p.Version < b.Version:
			out = append(out, fmt.Sprintf("%s: version went down (%d → %d)", p.ID, b.Version, p.Version))
		case p.Version == b.Version && presets.Hash(p) != presets.Hash(b):
			out = append(out, fmt.Sprintf("%s: content changed; raise version above %d", p.ID, b.Version))
		}
	}
	return out
}

// baseCatalog reads the base branch's catalog; a base without the file (the first catalog) is empty.
func baseCatalog(show func() ([]byte, error)) ([]presets.Preset, error) {
	raw, err := show()
	var ee *exec.ExitError
	if errors.As(err, &ee) && (strings.Contains(string(ee.Stderr), "does not exist in") || strings.Contains(string(ee.Stderr), "exists on disk, but not in")) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the base catalog: %w (CI must fetch full history)", err)
	}
	return presets.Parse(raw)
}

func lockcheck(baseRef string) error {
	base, err := baseCatalog(func() ([]byte, error) {
		return exec.Command("git", "show", baseRef+":internal/presets/presets.yaml").Output()
	})
	if err != nil {
		return err
	}
	head, err := presets.All()
	if err != nil {
		return err
	}
	if p := lockProblems(base, head); len(p) > 0 {
		return fmt.Errorf("preset versions:\n  %s", joinLines(p))
	}
	return nil
}
