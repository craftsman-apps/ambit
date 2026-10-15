package connect

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// A golden case is a directory testdata/<tool>/<case>/ with:
//
//	before      the file before connect (absent: the file does not exist)
//	steps.json  {"steps": [{"url", "model", "force", "env"}...]}, run one after the other on the result of the last
//	after       the file after the last step (absent when the last step stops on conflicts)
//	conflicts   the conflict lines of the last step, when it stops on conflicts
//	roundtrip   the file after disconnect, when it is not `before` (a removed secret is not restored)
type goldenStep struct {
	URL, Model string
	Force      bool
	Env        map[string]string
}

type goldenCase struct {
	dir    string
	before []byte
	exists bool
	steps  []goldenStep
}

func loadGolden(t *testing.T, tool string) []goldenCase {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", tool, "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no golden cases for %s: %v", tool, err)
	}
	var cases []goldenCase
	for _, d := range dirs {
		c := goldenCase{dir: d}
		if b, err := os.ReadFile(filepath.Join(d, "before")); err == nil {
			c.before, c.exists = b, true
		}
		raw, err := os.ReadFile(filepath.Join(d, "steps.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s struct{ Steps []goldenStep }
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		c.steps = s.Steps
		cases = append(cases, c)
	}
	return cases
}

// check compares got with the golden file name in dir, or rewrites it with -update. want == nil: the file must not exist.
func (c goldenCase) check(t *testing.T, name string, got *string) {
	t.Helper()
	p := filepath.Join(c.dir, name)
	if *update {
		if got == nil {
			os.Remove(p)
		} else if err := os.WriteFile(p, []byte(*got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	switch {
	case got == nil && err == nil:
		t.Fatalf("%s exists, but this case produced nothing for it:\n%s", p, want)
	case got != nil && err != nil:
		t.Fatalf("%s is missing; the case produced:\n%q", p, *got)
	case got != nil && *got != string(want):
		t.Fatalf("%s differs\n got  %q\n want %q", p, *got, want)
	}
}

func strp(s string) *string { return &s }

func conflictLines(cs []Conflict) *string {
	if len(cs) == 0 {
		return nil
	}
	var l []string
	for _, c := range cs {
		l = append(l, c.String())
	}
	return strp(strings.Join(l, "\n") + "\n")
}
