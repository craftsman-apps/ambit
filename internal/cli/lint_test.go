package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lintRisky = `apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: {name: everyone-tools}
spec:
  allSignedIn: true
  servers:
  - name: github
  - name: time
    tools: [get_current_time]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: not-ambit}
data: {allSignedIn: "true"}
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: {name: eng-models}
spec:
  groups: [eng]
  models: [gemma]
  servers:
  - name: time
    tools: [get_current_time]
`

const lintList = `apiVersion: v1
kind: List
items:
- apiVersion: ambitproject.dev/v1alpha1
  kind: AccessRule
  metadata: {name: a}
  spec: {groups: [eng], servers: [{name: files}]}
- apiVersion: ambitproject.dev/v1alpha1
  kind: AccessRule
  metadata: {name: b}
  spec: {users: [user01@example.com], models: [gemma]}
`

func writeLint(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLintFindsRiskyAccessRules(t *testing.T) {
	dir := writeLint(t, map[string]string{"rules.yaml": lintRisky, "sub/list.yml": lintList, "notes.txt": "allSignedIn: true"})
	code, s := runCLI(t, nil, "lint", dir)
	if code != 0 {
		t.Fatalf("code %d\n%s", code, s)
	}
	for _, want := range []string{
		"rules.yaml: AccessRule everyone-tools: matches all signed-in users; name groups or users [all-signed-in]",
		"rules.yaml: AccessRule everyone-tools: allows every tool of server github, including tools added later; list the tools [all-tools]",
		"list.yml: AccessRule a: allows every tool of server files, including tools added later; list the tools [all-tools]",
		"2 files, 4 AccessRules, 3 warnings",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "eng-models") || strings.Contains(s, "AccessRule b") || strings.Contains(s, "not-ambit") || strings.Contains(s, "server time") {
		t.Errorf("a safe rule or another object was reported:\n%s", s)
	}
}

func TestLintStrictAndErrors(t *testing.T) {
	dir := writeLint(t, map[string]string{"rules.yaml": lintRisky})
	if code, s := runCLI(t, nil, "lint", "--strict", dir); code != 1 {
		t.Fatalf("--strict with warnings: code %d\n%s", code, s)
	}
	clean := writeLint(t, map[string]string{"ok.yaml": lintList[:strings.Index(lintList, "- apiVersion")] + lintList[strings.LastIndex(lintList, "- apiVersion"):]})
	if code, s := runCLI(t, nil, "lint", "--strict", clean); code != 0 || !strings.Contains(s, "1 files, 1 AccessRules, 0 warnings") {
		t.Fatalf("--strict without warnings: code %d\n%s", code, s)
	}
	bad := writeLint(t, map[string]string{"bad.yaml": "kind: [unclosed\n"})
	if code, s := runCLI(t, nil, "lint", bad); code != 2 {
		t.Fatalf("bad YAML: code %d\n%s", code, s)
	}
	badList := writeLint(t, map[string]string{"list.yaml": "apiVersion: v1\nkind: List\nitems:\n- [apiVersion, kind]\n"})
	if code, s := runCLI(t, nil, "lint", badList); code != 2 || !strings.Contains(s, "item 0 of a List") {
		t.Fatalf("a List item that is not an object: code %d\n%s", code, s)
	}
	if code, _ := runCLI(t, nil, "lint"); code != 2 {
		t.Fatal("no path accepted")
	}
}

func TestLintGitHubAnnotations(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	dir := writeLint(t, map[string]string{"a,b.yaml": lintRisky})
	_, s := runCLI(t, nil, "lint", dir)
	if !strings.Contains(s, "::warning file="+filepath.Join(dir, "a%2Cb.yaml")+"::AccessRule everyone-tools: matches all signed-in users") {
		t.Fatalf("no escaped annotation:\n%s", s)
	}
}
