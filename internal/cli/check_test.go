package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/checks"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/local"
)

func TestSelectGroups(t *testing.T) {
	for _, c := range []struct {
		name      string
		f         checkFlags
		installed bool
		url       string
		want      []string
	}{
		{"nothing installed", checkFlags{}, false, "", []string{"pre"}},
		{"--pre --local", checkFlags{pre: true, local: true}, true, "", []string{"pre", "local"}},
		{"local install", checkFlags{}, true, "http://console.localtest.me:8080", []string{"health"}},
		{"production install", checkFlags{}, true, "https://console.example.com", []string{"health", "production"}},
		{"--production on local", checkFlags{production: true}, true, "http://console.localtest.me:8080", []string{"health", "production"}},
		{"--no-production", checkFlags{noProduction: true}, true, "https://console.example.com", []string{"health"}},
	} {
		g, err := selectGroups(c.f, c.installed, c.url)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, k := range []string{"pre", "local", "health", "production"} {
			if g[k] {
				got = append(got, k)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if _, err := selectGroups(checkFlags{production: true, noProduction: true}, true, ""); err == nil {
		t.Fatal("--production with --no-production must be a usage error")
	}
}

// unreachable is a kubeconfig whose API server does not answer, so every check reports unknown.
func unreachable(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte(`apiVersion: v1
kind: Config
current-context: gone
clusters: [{name: gone, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: gone, context: {cluster: gone, user: u}}]
users: [{name: u, user: {token: x}}]
`), 0o600)
	return p
}

func TestCheckJSONAndExitCodes(t *testing.T) {
	kc := unreachable(t)
	code, s := runCLI(t, &fakeHelm{}, "check", "--kubeconfig", kc, "--output", "json")
	var rs []checks.Result
	if err := json.Unmarshal([]byte(s), &rs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	if len(rs) != 6 || rs[0].ID != "R1" || rs[0].Status != checks.Unknown {
		t.Fatalf("pre group = %+v", rs)
	}
	if code != 0 {
		t.Fatalf("unknown results must exit 0 without --fail-on-unknown; got %d", code)
	}
	if code, _ := runCLI(t, &fakeHelm{}, "check", "--kubeconfig", kc, "--fail-on-unknown"); code != 1 {
		t.Fatalf("--fail-on-unknown: exit %d, want 1", code)
	}
	h := &fakeHelm{releases: map[string]*helmx.Release{"ambit": {Name: "ambit", ChartVersion: "0.2.0", Status: "deployed"}},
		values: map[string]map[string]any{"ambit/all": {"console": map[string]any{"url": "https://console.example.com"}}}}
	_, s = runCLI(t, h, "check", "--kubeconfig", kc)
	if !strings.Contains(s, "H1") || !strings.Contains(s, "P10") || !strings.Contains(s, "Checking context gone (https://127.0.0.1:1), namespace ambit-system") {
		t.Fatalf("installed, production URL: output\n%s", s)
	}
	if code, _ := runCLI(t, h, "check", "--kubeconfig", kc, "-f", filepath.Join(t.TempDir(), "missing.yaml")); code != 2 {
		t.Fatalf("missing -f file: exit %d, want 2", code)
	}
}

func TestCheckWithoutAnyKubeconfig(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o600)
	t.Setenv("KUBECONFIG", empty)
	code, s := runCLI(t, &fakeHelm{}, "check")
	if code != 0 || !strings.Contains(s, "? R1  no cluster to look at") || !strings.Contains(s, "0 passed, 0 warnings, 0 failed, 6 unknown") {
		t.Fatalf("code %d, output\n%s", code, s)
	}
}

// noKind is a kind with no clusters, for the local checks without Docker.
type noKind struct{}

func (noKind) Exists(string) (bool, error)         { return false, nil }
func (noKind) Create(string, []byte, string) error { return errors.New("not in this test") }
func (noKind) Delete(string, string) error         { return errors.New("not in this test") }
func (noKind) ControlPlane(string) (string, error) { return "", errors.New("not in this test") }
func (noKind) LoadArchive(string, string) error    { return errors.New("not in this test") }

// Ruling S13: `check --pre --local` on a laptop with no kubeconfig runs the local checks and reports the cluster
// checks as unknown, exit 0.
func TestCheckPreLocalWithoutKubeconfig(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o600)
	for name, kc := range map[string]string{"empty file": empty, "no file": filepath.Join(t.TempDir(), "missing")} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", kc)
			var b bytes.Buffer
			a := &app{in: strings.NewReader(""), out: &b, errw: &b,
				newHelm: func(helmx.Options) (helmx.Client, error) { return nil, errors.New("helm must not be used") },
				testLocalEnv: &local.Env{Store: local.Store{Dir: t.TempDir()}, Kind: noKind{},
					Docker: func() local.DockerInfo {
						return local.ParseDockerInfo([]byte(`{"ServerVersion":"29.1.0","OperatingSystem":"Docker Desktop","MemTotal":8217030656}`))
					},
					PortFree: func(int) bool { return true }}}
			root := a.root()
			root.SetArgs([]string{"check", "--pre", "--local"})
			root.SetOut(&b)
			root.SetErr(&b)
			code, s := a.exit(root.Execute()), b.String()
			if code != 0 || !strings.Contains(s, "5 passed, 0 warnings, 0 failed, 6 unknown") {
				t.Fatalf("code %d, output\n%s", code, s)
			}
			for _, id := range []string{"R1", "R2", "R3", "R4", "R5", "R6"} {
				if !strings.Contains(s, "? "+id+"  no cluster to look at") {
					t.Errorf("%s is not unknown:\n%s", id, s)
				}
			}
			for _, id := range []string{"L1", "L2", "L3", "L4", "L5"} {
				if !strings.Contains(s, "✓ "+id+" ") {
					t.Errorf("%s did not pass:\n%s", id, s)
				}
			}
		})
	}
}

// Decision from Task 2: with --output json, standard output carries only the JSON document.
func TestCheckJSONStdoutIsOnlyJSON(t *testing.T) {
	var o, e bytes.Buffer
	a := &app{in: strings.NewReader(""), out: &o, errw: &e,
		newHelm: func(helmx.Options) (helmx.Client, error) { return &fakeHelm{}, nil }}
	root := a.root()
	root.SetArgs([]string{"check", "--kubeconfig", unreachable(t), "--output", "json"})
	root.SetOut(&o)
	root.SetErr(&e)
	if code := a.exit(root.Execute()); code != 0 {
		t.Fatalf("exit %d, stderr\n%s", code, e.String())
	}
	var rs []checks.Result
	if err := json.Unmarshal(o.Bytes(), &rs); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, o.String())
	}
	if len(rs) != 6 {
		t.Fatalf("results = %d, want 6", len(rs))
	}
}

// An explicit --context or --kubeconfig that cannot be used is a usage error, never "no cluster" with exit 0.
func TestCheckExplicitTargetNotFound(t *testing.T) {
	kc := unreachable(t)
	code, s := runCLI(t, &fakeHelm{}, "check", "--kubeconfig", kc, "--context", "typo")
	if code != 2 || !strings.Contains(s, "context typo not found in "+kc) {
		t.Fatalf("--context typo: code %d, output\n%s", code, s)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o600)
	for _, args := range [][]string{
		{"check", "--kubeconfig", empty},
		{"check", "--kubeconfig", filepath.Join(t.TempDir(), "missing"), "--pre", "--local"},
		{"check", "--kubeconfig", empty, "--context", "typo"},
	} {
		if code, s := runCLI(t, &fakeHelm{}, args...); code != 2 || strings.Contains(s, "no cluster to look at") {
			t.Errorf("%v: code %d, output\n%s", args, code, s)
		}
	}
}

// A Helm read error is not "Ambit is not installed": the health checks run and report it.
func TestCheckHelmErrorIsNotNotInstalled(t *testing.T) {
	kc := unreachable(t)
	code, s := runCLI(t, &fakeHelm{err: errors.New("storage driver exploded")}, "check", "--kubeconfig", kc, "--no-production")
	if code != 1 || !strings.Contains(s, "✗ H1  ") || !strings.Contains(s, "storage driver exploded") || strings.Contains(s, "R1") {
		t.Fatalf("code %d, output\n%s", code, s)
	}
}

// I1 of the final review: with Docker down, `check --pre --local` does not run kind's docker ps. L1 fails with its
// next step, L5 is unknown, and stdout is still one JSON document.
func TestCheckPreLocalDockerDown(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	var o, e bytes.Buffer
	k := &countingKind{}
	a := &app{in: strings.NewReader(""), out: &o, errw: &e,
		newHelm: func(helmx.Options) (helmx.Client, error) { return nil, errors.New("helm must not be used") },
		testLocalEnv: &local.Env{Store: local.Store{Dir: t.TempDir()}, Kind: k,
			Docker:   func() local.DockerInfo { return local.DockerInfo{Err: "Cannot connect to the Docker daemon"} },
			PortFree: func(int) bool { return true }}}
	root := a.root()
	root.SetArgs([]string{"check", "--pre", "--local", "--output", "json"})
	root.SetOut(&o)
	root.SetErr(&e)
	if code := a.exit(root.Execute()); code != 1 {
		t.Fatalf("exit %d, stdout\n%s\nstderr\n%s", code, o.String(), e.String())
	}
	var rs []checks.Result
	if err := json.Unmarshal(o.Bytes(), &rs); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, o.String())
	}
	got := map[string]checks.Result{}
	for _, r := range rs {
		got[r.ID] = r
	}
	if got["L1"].Status != checks.Fail || !strings.Contains(got["L1"].Message, "Docker is not running") || !strings.Contains(got["L1"].Next, "start Docker") {
		t.Errorf("L1 = %+v", got["L1"])
	}
	if got["L5"].Status != checks.Unknown {
		t.Errorf("L5 = %+v", got["L5"])
	}
	if k.calls != 0 {
		t.Error("the ownership check ran against an unreachable Docker")
	}
}

// countingKind counts Exists calls; kind's docker ps is what it stands for.
type countingKind struct {
	noKind
	calls int
}

func (k *countingKind) Exists(string) (bool, error) {
	k.calls++
	return false, errors.New("docker ps hangs")
}
