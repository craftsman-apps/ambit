package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

// fakeHelm is a helmx.Client with fixed releases.
type fakeHelm struct {
	releases map[string]*helmx.Release
	values   map[string]map[string]any // "name/user" or "name/all"
	err      error
	applied  []string
}

func (f *fakeHelm) Apply(_ context.Context, name, ref, version string, _ map[string]any) (helmx.Release, error) {
	f.applied = append(f.applied, name+" "+ref+" "+version)
	return helmx.Release{Name: name, Revision: 1, Status: "deployed"}, f.err
}
func (f *fakeHelm) Get(name string) (*helmx.Release, error) { return f.releases[name], f.err }
func (f *fakeHelm) Values(name string, all bool) (map[string]any, error) {
	k := name + "/user"
	if all {
		k = name + "/all"
	}
	return f.values[k], f.err
}

func runCLI(t *testing.T, h helmx.Client, args ...string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	a := &app{in: strings.NewReader(""), out: &b, errw: &b, newHelm: func(helmx.Options) (helmx.Client, error) { return h, nil }}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(&b)
	root.SetErr(&b)
	return a.exit(root.Execute()), b.String()
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	code, s := runCLI(t, &fakeHelm{}, "version", "--nope")
	if code != 2 || !strings.Contains(s, "✗ usage — unknown flag: --nope") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestJSONOnlyForCheckAndVersion(t *testing.T) {
	dir := t.TempDir()
	code, s := runCLI(t, &fakeHelm{}, "validate", dir, "--output", "json")
	if code != 2 || !strings.Contains(s, "only `ambit check` and `ambit version` print JSON") {
		t.Fatalf("code %d, output %q", code, s)
	}
	if code, _ := runCLI(t, &fakeHelm{}, "version", "--output", "yaml"); code != 2 {
		t.Fatalf("--output yaml: code %d, want 2", code)
	}
}

const goodModel = `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: good }
spec: { provider: OpenAI, model: gpt-5, credentialsRef: { name: ambit-llm-openai } }
`

const badModel = `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: bad }
spec: { provider: NotAProvider, model: x, credentialsRef: { name: ambit-llm-x } }
`

func TestValidateUnchanged(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(goodModel), 0o600)
	if code, s := runCLI(t, nil, "validate", dir); code != 0 || !strings.Contains(s, "1 Ambit objects in 1 files: 0 problems") {
		t.Fatalf("good: code %d, output %q", code, s)
	}
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(badModel), 0o600)
	code, s := runCLI(t, nil, "validate", dir)
	if code != 1 || !strings.Contains(s, "bad.yaml: ") || !strings.Contains(s, "2 Ambit objects in 2 files: 1 problems") {
		t.Fatalf("bad: code %d, output %q", code, s)
	}
	if code, _ := runCLI(t, nil, "validate"); code != 2 {
		t.Fatalf("no arguments: code %d, want 2", code)
	}
	if code, _ := runCLI(t, nil, "validate", filepath.Join(dir, "missing")); code != 2 {
		t.Fatalf("missing path: code %d, want 2", code)
	}
}

func TestVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "0.2.0"
	h := &fakeHelm{releases: map[string]*helmx.Release{
		"ambit-crds": {Name: "ambit-crds", ChartVersion: "0.2.0"}, "ambit": {Name: "ambit", ChartVersion: "0.2.0"}}}
	code, s := runCLI(t, h, "version")
	want := "ambit 0.2.0\ncharts 0.2.0 from oci://ghcr.io/craftsman-apps/charts\n" +
		"installed: ambit-crds 0.2.0 in namespace ambit-system\ninstalled: ambit 0.2.0 in namespace ambit-system\n"
	if code != 0 || s != want {
		t.Fatalf("code %d, output %q", code, s)
	}
	code, s = runCLI(t, &fakeHelm{err: errors.New("connection refused")}, "version", "--output", "json")
	var v versionInfo
	if code != 0 || json.Unmarshal([]byte(s), &v) != nil || v.Installed != nil || v.Cluster != "not reachable" || v.Charts != "0.2.0" {
		t.Fatalf("json without cluster: code %d, output %q", code, s)
	}
}

func TestVersionDevBuild(t *testing.T) {
	_, s := runCLI(t, &fakeHelm{}, "version")
	if !strings.Contains(s, "ambit dev\ncharts: none (development build; local up needs --chart-dir)\ninstalled: none in namespace ambit-system\n") {
		t.Fatalf("output %q", s)
	}
}
