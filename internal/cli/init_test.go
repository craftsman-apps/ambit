package cli

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite internal/cli/testdata/init/*.yaml")

// scripted answers each question whose text contains a key; every other question gets Enter (its default). It
// records the questions.
type scripted struct {
	answers map[string]string
	asked   []string
}

func (s *scripted) Ask(q, def string) (string, error) {
	s.asked = append(s.asked, q)
	for k, v := range s.answers {
		if strings.Contains(q, k) {
			return v, nil
		}
	}
	return def, nil
}

// initPaths are the answer sets of the golden files (spec D2 3.1): one per sign-in, token and storage path.
var initPaths = map[string]map[string]string{
	"oidc-own-tokens": {"Company sign-in": "oidc", "OIDC issuer URL": "https://login.acme.example", "Gateway tokens": "idp",
		"Token issuer URL": "https://login.acme.example"},
	"github":           {"Company sign-in": "github", "GitHub OAuth app client ID": "Iv1.abc", "GitHub organization": "acme", "Gateway tokens": "dex"},
	"saml":             {"Company sign-in": "saml", "SAML single sign-on URL": "https://idp.acme.example/sso", "Gateway tokens": "dex"},
	"ldap":             {"Company sign-in": "ldap", "Gateway tokens": "dex"},
	"dex-tokens":       {"Gateway tokens": "dex", "MCP gateway on?": "n"},
	"external-storage": {"Gateway tokens": "dex", "Your own ClickHouse": "y", "Your own Valkey": "y", "When the budget store": "FailClosed"},
	"gitops":           {"Gateway tokens": "dex", "GitOps mode": "y", "Configuration repository": "acme/platform-config", "GitHub App ID": "123456"},
	"no-tls":           {"Gateway tokens": "dex", "TLS Secret": "none", "Gateway replicas": "3"},
}

func initRun(t *testing.T, dir string, p prompter, tty bool, args ...string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	a := &app{in: strings.NewReader(""), out: &b, errw: &b, testPrompter: p, testTTY: tty,
		newHelm: func(helmx.Options) (helmx.Client, error) { return &fakeHelm{}, nil }}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(&b)
	root.SetErr(&b)
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	return a.exit(root.Execute()), b.String()
}

// withoutHeader drops the comment lines (they hold the date).
func withoutHeader(b []byte) string {
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "#") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

func TestInitGoldenFiles(t *testing.T) {
	old := version
	version = "0.5.0"
	t.Cleanup(func() { version = old })
	for name, answers := range initPaths {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := &scripted{answers: answers}
			code, outText := initRun(t, dir, s, false, "init")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, outText)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "values.yaml"))
			if !strings.HasPrefix(string(got), "# Values for ambit install, written by ambit init 0.5.0 on ") || !strings.Contains(string(got), "does not read this file back") {
				t.Errorf("header: %q", strings.SplitN(string(got), "\n", 3)[:2])
			}
			golden := filepath.Join("testdata", "init", name+".yaml")
			if *updateGolden {
				os.MkdirAll(filepath.Dir(golden), 0o755)
				os.WriteFile(golden, []byte(withoutHeader(got)), 0o644)
			}
			want, err := os.ReadFile(golden)
			if err != nil || withoutHeader(got) != string(want) {
				t.Fatalf("%s differs from %s (go test -run TestInitGoldenFiles -update-golden rewrites it):\n%s", "values.yaml", golden, got)
			}
			// 3.1 no secret: a question about a Secret asks only for its name.
			for _, q := range s.asked {
				lq := strings.ToLower(q)
				if (strings.Contains(lq, "secret") || strings.Contains(lq, "password") || strings.Contains(lq, "private key")) && !strings.HasPrefix(q, "Name of the") {
					t.Errorf("a question may ask for a secret value: %q", q)
				}
			}
			if !strings.Contains(outText, "Next: ambit install -f values.yaml") {
				t.Errorf("output:\n%s", outText)
			}
		})
	}
}

// The output names every Secret with a kubectl command that holds placeholders only, and the --secret-file form.
func TestInitPrintsSecretCommands(t *testing.T) {
	dir := t.TempDir()
	code, s := initRun(t, dir, &scripted{answers: initPaths["oidc-own-tokens"]}, false, "init", "-n", "ambit-prod")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
	for _, want := range []string{
		"Wrote values.yaml (",
		"Create these Secrets before ambit install (values are never written to files by ambit init):",
		"kubectl -n ambit-prod create secret generic ambit-sso --from-literal=clientSecret=<client secret>",
		"kubectl -n ambit-prod create secret tls ambit-tls --cert=<file> --key=<file>",
		"ambit install -f values.yaml --secret-file ambit-sso/clientSecret=<file> --secret-file ambit-tls/tls.crt=<file> --secret-file ambit-tls/tls.key=<file>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

// 3.1: an existing file is refused without --force; no terminal is exit 2; --gitops takes argocd only.
func TestInitRefusals(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("mine: true\n"), 0o644)
	if code, s := initRun(t, dir, &scripted{}, false, "init"); code != 1 || !strings.Contains(s, "values.yaml exists") {
		t.Fatalf("existing: exit %d\n%s", code, s)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "values.yaml")); string(b) != "mine: true\n" {
		t.Fatal("the existing file was changed")
	}
	if code, s := initRun(t, dir, &scripted{answers: map[string]string{"Gateway tokens": "dex"}}, false, "init", "--force"); code != 0 {
		t.Fatalf("--force: exit %d\n%s", code, s)
	}
	if code, s := initRun(t, dir, nil, false, "init", "--out", "other.yaml"); code != 2 || !strings.Contains(s, "needs a terminal") {
		t.Fatalf("no terminal: exit %d\n%s", code, s)
	}
	if code, s := initRun(t, dir, &scripted{}, false, "init", "--gitops", "flux"); code != 2 || !strings.Contains(s, "only argocd") {
		t.Fatalf("--gitops flux: exit %d\n%s", code, s)
	}
}

// The mapping table: every values path init writes exists in the chart's values.yaml of this version.
func TestInitKeysExistInTheChart(t *testing.T) {
	b, err := os.ReadFile("../../charts/ambit/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart map[string]any
	if err := yaml.Unmarshal(b, &chart); err != nil {
		t.Fatal(err)
	}
	for _, k := range valueKeys {
		if _, ok := get(chart, k); !ok {
			t.Errorf("charts/ambit/values.yaml has no %s, which ambit init writes", k)
		}
	}
	// And every key in the golden files is in the table.
	for name := range initPaths {
		gb, err := os.ReadFile(filepath.Join("testdata", "init", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		yaml.Unmarshal(gb, &v)
		for _, k := range leafPaths(v, "") {
			if !contains(valueKeys, k) {
				t.Errorf("%s writes %s, which is not in valueKeys", name, k)
			}
		}
	}
}

func get(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func leafPaths(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if mm, ok := v.(map[string]any); ok && len(mm) > 0 {
			out = append(out, leafPaths(mm, p)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// 3.1 renders: each golden file renders with the chart of this checkout. The chart's dependencies must be in
// charts/ambit/charts (helm dependency build charts/ambit); without them the test is skipped unless
// AMBIT_RENDER_CHARTS=1 (CI's cli-install job sets it).
func TestInitGoldenFilesRender(t *testing.T) {
	if _, err := os.Stat("../../charts/ambit/charts"); err != nil {
		if os.Getenv("AMBIT_RENDER_CHARTS") == "1" {
			t.Fatal("charts/ambit/charts is missing: helm dependency build charts/ambit")
		}
		t.Skip("charts/ambit/charts is missing (helm dependency build charts/ambit)")
	}
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	os.WriteFile(kc, []byte(prodKubeconfig), 0o600)
	h, err := helmx.New(helmx.Options{Kubeconfig: kc, Namespace: "ambit-system"})
	if err != nil {
		t.Fatal(err)
	}
	// The check can fail: the chart refuses the LLM gateway without a token issuer.
	if _, err := h.Render(context.Background(), "ambit", "../../charts/ambit", "", map[string]any{"llm": map[string]any{"enabled": true}}, "v1.31.0"); err == nil {
		t.Fatal("a values file without a token issuer rendered; the render check proves nothing")
	}
	for name := range initPaths {
		b, _ := os.ReadFile(filepath.Join("testdata", "init", name+".yaml"))
		var v map[string]any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Render(context.Background(), "ambit", "../../charts/ambit", "", v, "v1.31.0"); err != nil {
			t.Errorf("%s does not render: %v", name, err)
		}
	}
}

// 3.1.1: the Argo CD files parse as Applications with the waves and sources of the spec.
func TestInitGitopsArgoCDFiles(t *testing.T) {
	old := version
	version = "0.5.0"
	t.Cleanup(func() { version = old })
	for _, parent := range []string{"n", "y"} {
		dir := t.TempDir()
		answers := map[string]string{"Gateway tokens": "dex", "GitOps mode": "y", "Configuration repository": "acme/platform-config",
			"GitHub App ID": "123456", "Directory of Ambit": "platform/ambit", "app of apps": parent}
		code, s := initRun(t, dir, &scripted{answers: answers}, false, "init", "--gitops", "argocd")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, s)
		}
		read := func(p string) map[string]any {
			b, err := os.ReadFile(filepath.Join(dir, p))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
		if _, err := os.Stat(filepath.Join(dir, "platform/ambit/values.yaml")); err != nil {
			t.Fatal("no values file at <path>/values.yaml")
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "platform/ambit/crds/dependency-crds.yaml")); !bytes.Contains(b, []byte("gateways.gateway.networking.k8s.io")) {
			t.Fatal("no CRD bundle copy")
		}
		repo := "https://github.com/acme/platform-config.git"
		for file, want := range map[string]struct{ wave, check string }{
			"argocd/ambit-dependency-crds.yaml": {"-2", "path=platform/ambit/crds ServerSideApply=true"},
			"argocd/ambit-crds.yaml":            {"-1", "chart=ambit-crds 0.5.0"},
			"argocd/ambit.yaml":                 {"0", "chart=ambit 0.5.0 $values/platform/ambit/values.yaml ref=values"},
			"argocd/ambit-objects.yaml":         {"1", "path=platform/ambit"},
		} {
			m := read(file)
			md := m["metadata"].(map[string]any)
			if m["kind"] != "Application" || md["namespace"] != "argocd" || md["annotations"].(map[string]any)["argocd.argoproj.io/sync-wave"] != want.wave {
				t.Errorf("%s: %v", file, m)
			}
			spec := m["spec"].(map[string]any)
			var src []any
			if one, ok := spec["source"]; ok {
				src = []any{one}
			} else {
				src = spec["sources"].([]any)
			}
			flat := ""
			for _, s := range src {
				sm := s.(map[string]any)
				flat += " " + yamlString(sm)
			}
			flat += " " + yamlString(spec["syncPolicy"].(map[string]any))
			for _, w := range strings.Fields(want.check) {
				k, v, ok := strings.Cut(w, "=")
				if ok && k != "ServerSideApply" && k != "CreateNamespace" {
					w = k + ": " + v
				}
				if !strings.Contains(flat, w) {
					t.Errorf("%s: no %q in %s", file, w, flat)
				}
			}
			if file != "argocd/ambit-crds.yaml" && strings.Contains(file, "ambit") && !strings.Contains(flat, repo) && file != "argocd/ambit-crds.yaml" {
				t.Errorf("%s does not name the repository: %s", file, flat)
			}
		}
		_, err := os.Stat(filepath.Join(dir, "argocd-parent.yaml"))
		if (parent == "y") != (err == nil) {
			t.Errorf("parent %s: argocd-parent.yaml exists = %v", parent, err == nil)
		}
		if parent == "n" && !strings.Contains(s, "Apply them in this order, and wait until each is Healthy") {
			t.Errorf("no order note:\n%s", s)
		}
		if !strings.Contains(s, "resource.customizations.health.ambitproject.dev_AccessRule") {
			t.Errorf("no health check note:\n%s", s)
		}
		// A second run needs --force.
		if code, _ := initRun(t, dir, &scripted{answers: answers}, false, "init", "--gitops", "argocd"); code != 1 {
			t.Errorf("second run without --force: exit %d", code)
		}
	}
}

func yamlString(m map[string]any) string {
	b, _ := yaml.Marshal(m)
	return string(b)
}

// Codex review: only a valid Secret name reaches the printed kubectl command; an answer with '=' is asked again.
func TestInitSecretNameIsAName(t *testing.T) {
	dir := t.TempDir()
	answers := []string{"x=REAL-VALUE", "my-sso"}
	p := &seqSecret{answers: answers}
	code, s := initRun(t, dir, p, false, "init")
	if code != 0 || strings.Contains(s, "REAL-VALUE") || !strings.Contains(s, "create secret generic my-sso --from-literal=clientSecret=<client secret>") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// seqSecret answers oidc sign-in and gives its answers, in order, to the Secret name question.
type seqSecret struct{ answers []string }

func (p *seqSecret) Ask(q, def string) (string, error) {
	switch {
	case strings.Contains(q, "Company sign-in"):
		return "oidc", nil
	case strings.Contains(q, "Gateway tokens"):
		return "dex", nil
	case strings.Contains(q, "sign-in secret"):
		a := p.answers[0]
		p.answers = p.answers[1:]
		return a, nil
	}
	return def, nil
}
