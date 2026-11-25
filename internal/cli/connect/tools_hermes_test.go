package connect

import (
	"path/filepath"
	"strings"
	"testing"
)

const hermesSeed = `# Hermes settings
model:
  default: anthropic/claude-sonnet-4
  provider: auto
  context_length: 200000

providers:
  together:
    api: https://api.together.xyz/v1
    key_env: TOGETHER_API_KEY

mcp_servers:
  github:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]

terminal:
  backend: local
`

func hermesIn(file string, exists bool, prev *FileRecord) Input {
	return Input{Path: "/home/a/.hermes/config.yaml", File: []byte(file), Exists: exists, URL: "https://llm.example.com", Model: "gpt-5",
		AmbitPath: ambitBin, GOOS: "darwin", Getenv: func(string) string { return "" }, Prev: prev, MCPURL: "https://mcp.example.com/mcp"}
}

func mustHermes(t *testing.T, in Input) *Plan {
	t.Helper()
	p, err := PlanHermes(in, "/home/a/.ambit/hermes-mcp-headers.txt")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// hermesRoundTrip: disconnect of the plan's record gives back want, byte for byte.
func hermesRoundTrip(t *testing.T, p *Plan, want string, created bool) {
	t.Helper()
	d, err := UnplanBlocks(&p.Record, p.After, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		if !d.Delete {
			t.Fatalf("a file connect made must be deleted, got %q", d.After)
		}
		return
	}
	if d.Delete || string(d.After) != want {
		t.Fatalf("disconnect did not restore the file\n got  %q\n want %q", d.After, want)
	}
}

func TestHermesFreshFile(t *testing.T) {
	p := mustHermes(t, hermesIn("", false, nil))
	got, err := decodeYAML("x", p.After)
	if err != nil {
		t.Fatal(err)
	}
	prov := got["providers"].(map[string]any)["ambit"].(map[string]any)
	if prov["key_cmd"] != "'"+ambitBin+"' key print --url 'https://llm.example.com'" || prov["base_url"] != "https://llm.example.com/v1" {
		t.Fatalf("provider: %#v", prov)
	}
	if m := got["model"].(map[string]any); m["provider"] != "custom:ambit" || m["default"] != "gpt-5" {
		t.Fatalf("model: %#v", m)
	}
	mcp := got["mcp_servers"].(map[string]any)["ambit"].(map[string]any)
	if mcp["command"] != "npx" || !strings.Contains(strings.Join(anyStrings(mcp["args"]), " "), "mcp-remote@0.14.3 https://mcp.example.com/mcp --header-file /home/a/.ambit/hermes-mcp-headers.txt") {
		t.Fatalf("mcp: %#v", mcp)
	}
	if strings.Contains(string(p.After), "ambit_") {
		t.Fatal("the config holds a key")
	}
	hermesRoundTrip(t, p, "", true)
}

func anyStrings(v any) []string {
	var s []string
	for _, x := range v.([]any) {
		s = append(s, x.(string))
	}
	return s
}

// An existing config: the model is a conflict; with --force it is replaced, the provider and the server go next to
// the person's, a second run changes nothing, and disconnect gives the file back byte for byte.
func TestHermesExistingFileForceRerunDisconnect(t *testing.T) {
	p := mustHermes(t, hermesIn(hermesSeed, true, nil))
	if p.After != nil || len(p.Conflicts) != 1 || p.Conflicts[0].What != "model is set to another value" {
		t.Fatalf("conflicts %v, after %q", p.Conflicts, p.After)
	}
	in := hermesIn(hermesSeed, true, nil)
	in.Force = true
	p = mustHermes(t, in)
	got, _ := decodeYAML("x", p.After)
	if _, ok := got["providers"].(map[string]any)["together"]; !ok {
		t.Fatalf("the other provider is gone:\n%s", p.After)
	}
	if _, ok := got["mcp_servers"].(map[string]any)["github"]; !ok || got["terminal"] == nil {
		t.Fatalf("other entries are gone:\n%s", p.After)
	}
	if !strings.Contains(string(p.After), "# ambit-replaced: model:\n") || !strings.Contains(string(p.After), "# ambit-replaced:   context_length: 200000\n") {
		t.Fatalf("the old model is not kept as a comment:\n%s", p.After)
	}
	// re-run, without --force: the person decided the model already
	again := mustHermes(t, hermesIn(string(p.After), true, &p.Record))
	if !again.Already || len(again.Conflicts) != 0 || string(again.After) != string(p.After) {
		t.Fatalf("rerun: already %v conflicts %v\n%s", again.Already, again.Conflicts, again.After)
	}
	hermesRoundTrip(t, p, hermesSeed, false)
}

// `providers: {}` (as a fresh install may write) becomes a mapping; without --mcp-url no MCP entry is written.
// CRLF line ends and the end without a line break survive the round trip.
func TestHermesEmptyMappingAndCRLF(t *testing.T) {
	seed := "model:\r\n  provider: custom:ambit\r\n  default: gpt-5\r\nproviders: {}\r\nmcp_servers: {}"
	in := hermesIn(seed, true, nil)
	in.MCPURL = ""
	p := mustHermes(t, in)
	if len(p.Conflicts) != 0 || strings.Contains(string(p.After), "mcp-remote") || !strings.Contains(string(p.After), "providers:\r\n  ambit:\r\n") {
		t.Fatalf("conflicts %v\n%q", p.Conflicts, p.After)
	}
	hermesRoundTrip(t, p, seed, false)
}

// providers.ambit written by the person is a conflict; --force comments it out, and disconnect puts it back.
func TestHermesOwnAmbitProvider(t *testing.T) {
	seed := "providers:\n    ambit:\n        api: https://old.example.com/v1\n        api_key: sk-old\n    other:\n        api: https://x.example.com\n"
	in := hermesIn(seed, true, nil)
	in.MCPURL = ""
	p := mustHermes(t, in)
	if len(p.Conflicts) != 1 || p.Conflicts[0].What != "providers.ambit already exists" {
		t.Fatalf("conflicts %v", p.Conflicts)
	}
	in.Force = true
	p = mustHermes(t, in)
	got, _ := decodeYAML("x", p.After)
	provs := got["providers"].(map[string]any)
	if provs["ambit"].(map[string]any)["base_url"] != "https://llm.example.com/v1" || provs["other"] == nil {
		t.Fatalf("providers: %#v\n%s", provs, p.After)
	}
	hermesRoundTrip(t, p, seed, false)
}

func TestHermesWindows(t *testing.T) {
	env := map[string]string{"LOCALAPPDATA": `C:\Users\a\AppData\Local`}
	if got := HermesConfigPath("windows", func(k string) string { return env[k] }, `C:\Users\a`); got != `C:\Users\a\AppData\Local/hermes/config.yaml` &&
		got != `C:\Users\a\AppData\Local\hermes\config.yaml` {
		t.Fatalf("path %q", got)
	}
	if got := HermesConfigPath("linux", func(k string) string { return map[string]string{"HERMES_HOME": "/srv/h"}[k] }, "/home/a"); got != filepath.Join("/srv/h", "config.yaml") {
		t.Fatalf("HERMES_HOME: %q", got)
	}
	in := hermesIn("", false, nil)
	in.GOOS, in.AmbitPath = "windows", `C:\Program Files\ambit\ambit.exe`
	p := mustHermes(t, in)
	got, _ := decodeYAML("x", p.After)
	if kc := got["providers"].(map[string]any)["ambit"].(map[string]any)["key_cmd"]; kc != `"C:\Program Files\ambit\ambit.exe" key print --url "https://llm.example.com"` {
		t.Fatalf("key_cmd %q", kc)
	}
	// cmd.exe reads no single quotes: a path with ' or $ ... is double-quoted, or refused when no quoting is right.
	in.AmbitPath = `C:\Users\O'Brien\ambit.exe`
	got, _ = decodeYAML("x", mustHermes(t, in).After)
	if kc := got["providers"].(map[string]any)["ambit"].(map[string]any)["key_cmd"]; kc != `"C:\Users\O'Brien\ambit.exe" key print --url "https://llm.example.com"` {
		t.Fatalf("key_cmd %q", kc)
	}
	for _, bad := range []string{`C:\Users\a$b\ambit.exe`, `C:\Users\100%\ambit.exe`} {
		in.AmbitPath = bad
		if _, err := PlanHermes(in, "h"); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

// Files ambit cannot read or edit stop with an error and no plan.
func TestHermesRefuses(t *testing.T) {
	for name, seed := range map[string]string{
		"invalid YAML":       "model: [unclosed\n",
		"providers is list":  "providers:\n  - name: x\n    base_url: https://x\n",
		"providers is value": "providers: something\n",
		"top is a list":      "- a\n- b\n",
		"edited block":       "# >>> ambit: x\nmodel: y\n",
		"model twice":        "model: {provider: auto}\nmodel:\n  default: other\n",
		"quoted providers":   "\"providers\":\n  other:\n    api: https://example.com\n",
		"ambit twice":        "providers:\n  ambit:\n    api: a\n  'ambit':\n    api: b\n",
	} {
		in := hermesIn(seed, true, nil)
		in.Force = true
		if p, err := PlanHermes(in, "/h"); err == nil {
			t.Errorf("%s: no error, after:\n%s", name, p.After)
		}
	}
}

// A decision with --force is for one entry: after providers.ambit was replaced, the person's own mcp_servers.ambit
// is still a conflict.
func TestHermesForceIsPerEntry(t *testing.T) {
	seed := "providers:\n  ambit:\n    api: https://old.example.com/v1\n"
	in := hermesIn(seed, true, nil)
	in.MCPURL, in.Force = "", true
	p := mustHermes(t, in)
	again := mustHermes(t, hermesIn(string(p.After), true, &p.Record))
	again.Record = p.Record
	if again.After == nil || len(again.Conflicts) != 0 {
		// with --mcp-url but no mcp_servers yet: no conflict either
		t.Fatalf("rerun: %v", again.Conflicts)
	}
	withMine := string(p.After) + "mcp_servers:\n  ambit:\n    command: mine\n"
	in2 := hermesIn(withMine, true, &p.Record)
	got := mustHermes(t, in2)
	if len(got.Conflicts) != 1 || got.Conflicts[0].What != "mcp_servers.ambit already exists" {
		t.Fatalf("conflicts %v", got.Conflicts)
	}
}
