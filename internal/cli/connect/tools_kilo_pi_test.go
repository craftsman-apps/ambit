package connect

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

const kiloSeed = `{
  "$schema": "https://app.kilo.ai/config.json",
  // my providers
  "provider": {
    "lmstudio": { "options": { "baseURL": "http://127.0.0.1:1234/v1" } }
  },
  "mcp": {
    "github": { "type": "local", "command": ["npx", "-y", "@modelcontextprotocol/server-github"] }
  },
}
`

func jsonIn(path, file string, exists bool, prev *FileRecord) Input {
	return Input{Path: path, File: []byte(file), Exists: exists, URL: "https://llm.example.com", Model: "gpt-5", AmbitPath: ambitBin,
		GOOS: "darwin", Getenv: func(string) string { return "" }, Prev: prev, MCPURL: "https://mcp.example.com/mcp",
		KeyFile: "/home/a/.config/ambit/kilo-key"}
}

func decodeJSONC(t *testing.T, b []byte) map[string]any {
	t.Helper()
	std, err := hujson.Standardize(append([]byte(nil), b...))
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	var m map[string]any
	if err := json.Unmarshal(std, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// jsonRoundTrip: a second plan with the record changes nothing, and disconnect gives back want byte for byte.
func jsonRoundTrip(t *testing.T, plan func(Input) (*Plan, error), in Input, p *Plan, want string) {
	t.Helper()
	in.File, in.Exists, in.Prev, in.Force = p.After, true, &p.Record, false
	again, err := plan(in)
	if err != nil || !again.Already {
		t.Fatalf("rerun: %v already %v conflicts %v", err, again != nil && again.Already, again.Conflicts)
	}
	d, err := UnplanJSON(p.Record, p.After, true)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		if !d.Delete {
			t.Fatalf("a file connect made must be deleted, got %q", d.After)
		}
		return
	}
	if string(d.After) != want {
		t.Fatalf("disconnect did not restore the file\n got  %q\n want %q", d.After, want)
	}
}

func TestKiloFreshFile(t *testing.T) {
	in := jsonIn("/home/a/.config/kilo/kilo.jsonc", "", false, nil)
	p, err := PlanKilo(in)
	if err != nil || len(p.Conflicts) > 0 {
		t.Fatal(err, p.Conflicts)
	}
	m := decodeJSONC(t, p.After)
	prov := m["provider"].(map[string]any)["ambit"].(map[string]any)
	opt := prov["options"].(map[string]any)
	if opt["apiKey"] != "{file:/home/a/.config/ambit/kilo-key}" || opt["baseURL"] != "https://llm.example.com/v1" || prov["npm"] != "@ai-sdk/openai-compatible" || m["model"] != "ambit/gpt-5" {
		t.Fatalf("%s", p.After)
	}
	mcp := m["mcp"].(map[string]any)["ambit"].(map[string]any)
	if mcp["type"] != "remote" || mcp["url"] != "https://mcp.example.com/mcp" || mcp["headers"].(map[string]any)["Authorization"] != "Bearer {file:/home/a/.config/ambit/kilo-key}" {
		t.Fatalf("mcp: %#v", mcp)
	}
	jsonRoundTrip(t, PlanKilo, in, p, "")
}

// Existing JSONC with comments, a trailing comma and other providers and servers: Ambit's entries go next to them.
func TestKiloExistingFile(t *testing.T) {
	in := jsonIn("/home/a/.config/kilo/kilo.jsonc", kiloSeed, true, nil)
	p, err := PlanKilo(in)
	if err != nil || len(p.Conflicts) > 0 {
		t.Fatal(err, p.Conflicts)
	}
	m := decodeJSONC(t, p.After)
	if m["provider"].(map[string]any)["lmstudio"] == nil || m["mcp"].(map[string]any)["github"] == nil || !strings.Contains(string(p.After), "// my providers") {
		t.Fatalf("%s", p.After)
	}
	jsonRoundTrip(t, PlanKilo, in, p, kiloSeed)
}

// Another model, and a provider "ambit" the person wrote: conflicts; --force replaces them. The model comes back at
// disconnect; the person's provider (which may hold a key) stays in the backup only.
func TestKiloConflicts(t *testing.T) {
	seed := `{"model": "anthropic/claude", "provider": {"ambit": {"options": {"apiKey": "sk-old"}}}}`
	in := jsonIn("/home/a/.config/kilo/kilo.json", seed, true, nil)
	in.MCPURL = ""
	p, err := PlanKilo(in)
	if err != nil || p.After != nil || len(p.Conflicts) != 2 {
		t.Fatalf("%v %v", err, p.Conflicts)
	}
	in.Force = true
	if p, err = PlanKilo(in); err != nil || strings.Contains(string(p.After), "sk-old") || len(p.Record.RemovedSecrets) != 1 {
		t.Fatalf("%v\n%s", err, p.After)
	}
	d, _ := UnplanJSON(p.Record, p.After, true)
	if m := decodeJSONC(t, d.After); m["model"] != "anthropic/claude" || len(d.Notes) != 1 {
		t.Fatalf("%s %v", d.After, d.Notes)
	}
}

func TestKiloWindowsAndPaths(t *testing.T) {
	in := jsonIn(`C:\Users\a\.config\kilo\kilo.jsonc`, "", false, nil)
	in.GOOS, in.KeyFile = "windows", `C:\Users\a\.config\ambit\kilo-key`
	p, err := PlanKilo(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.After), `"apiKey": "{file:C:/Users/a/.config/ambit/kilo-key}"`) {
		t.Fatalf("%s", p.After)
	}
	none := func(string) bool { return false }
	if got := KiloPath(func(string) string { return "" }, "/home/a", none); got != filepath.Join("/home/a", ".config", "kilo", "kilo.jsonc") {
		t.Fatalf("default %q", got)
	}
	xdg := func(k string) string { return map[string]string{"XDG_CONFIG_HOME": "/x"}[k] }
	only := func(name string) func(string) bool { return func(p string) bool { return p == name } }
	if got := KiloPath(xdg, "/home/a", only(filepath.Join("/x", "kilo", "kilo.json"))); got != filepath.Join("/x", "kilo", "kilo.json") {
		t.Fatalf("existing kilo.json %q", got)
	}
	if got := KiloKeyPath(xdg, "/home/a"); got != filepath.Join("/x", "ambit", "kilo-key") {
		t.Fatalf("key path %q", got)
	}
}

func TestJSONToolsRefuseInvalidFiles(t *testing.T) {
	for name, plan := range map[string]func(Input) (*Plan, error){"kilo": PlanKilo, "pi models": PlanPiModels, "pi settings": PlanPiSettings, "pi mcp": PlanPiMCP} {
		for _, seed := range []string{`{"provider": `, `[1, 2]`, `{"provider": "x", "providers": 3, "mcpServers": 4}`} {
			if name == "pi settings" && strings.HasPrefix(seed, `{"provider": "x"`) {
				continue // settings.json has no nested entry of Ambit's
			}
			in := jsonIn("/f.json", seed, true, nil)
			in.Force = true
			if p, err := plan(in); err == nil && p.After != nil {
				t.Errorf("%s %q: no error, after:\n%s", name, seed, p.After)
			}
		}
	}
}

func TestPiModelsSettingsMCP(t *testing.T) {
	seed := "{\n  \"providers\": {\n    \"ollama\": { \"baseUrl\": \"http://localhost:11434/v1\", \"api\": \"openai-completions\", \"apiKey\": \"ollama\", \"models\": [{ \"id\": \"qwen\" }] }\n  }\n}\n"
	in := jsonIn("/home/a/.pi/agent/models.json", seed, true, nil)
	p, err := PlanPiModels(in)
	if err != nil || len(p.Conflicts) > 0 {
		t.Fatal(err, p.Conflicts)
	}
	prov := decodeJSONC(t, p.After)["providers"].(map[string]any)["ambit"].(map[string]any)
	if prov["apiKey"] != "!'"+ambitBin+"' key print --url 'https://llm.example.com'" || prov["api"] != "openai-completions" || prov["baseUrl"] != "https://llm.example.com/v1" {
		t.Fatalf("%s", p.After)
	}
	jsonRoundTrip(t, PlanPiModels, in, p, seed)

	// settings: another default is a conflict; --force replaces it and disconnect gives it back.
	sseed := `{"defaultProvider": "anthropic", "defaultModel": "claude-sonnet-4", "theme": "dark"}`
	sin := jsonIn("/home/a/.pi/agent/settings.json", sseed, true, nil)
	if p, _ := PlanPiSettings(sin); len(p.Conflicts) != 2 {
		t.Fatalf("conflicts %v", p.Conflicts)
	}
	sin.Force = true
	sp, err := PlanPiSettings(sin)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeJSONC(t, sp.After); m["defaultProvider"] != "ambit" || m["defaultModel"] != "gpt-5" || m["theme"] != "dark" {
		t.Fatalf("%s", sp.After)
	}
	jsonRoundTrip(t, PlanPiSettings, sin, sp, sseed)

	min := jsonIn("/home/a/.pi/agent/mcp.json", "", false, nil)
	mp, err := PlanPiMCP(min)
	if err != nil {
		t.Fatal(err)
	}
	if srv := decodeJSONC(t, mp.After)["mcpServers"].(map[string]any)["ambit"].(map[string]any); srv["url"] != "https://mcp.example.com/mcp" || srv["auth"].(map[string]any)["provider"] != "ambit" {
		t.Fatalf("%s", mp.After)
	}
	if strings.Contains(string(mp.After), "ambit_") {
		t.Fatal("mcp.json holds a key")
	}
	jsonRoundTrip(t, PlanPiMCP, min, mp, "")
	min.MCPURL = "http://mcp.example.com/mcp"
	if _, err := PlanPiMCP(min); err == nil {
		t.Fatal("pi sends no key over plain http to another host")
	}
	min.MCPURL = "http://localhost:8080/mcp"
	if _, err := PlanPiMCP(min); err != nil {
		t.Fatal(err)
	}
}

func TestPiWindowsAndDir(t *testing.T) {
	in := jsonIn(`C:\Users\a\.pi\agent\models.json`, "", false, nil)
	in.GOOS, in.AmbitPath = "windows", `C:\Program Files\ambit\ambit.exe`
	if got := piKeyCommand(in); got != `!"C:\Program Files\ambit\ambit.exe" key print --url "https://llm.example.com"` {
		t.Fatalf("%q", got)
	}
	in.GOOS, in.AmbitPath = "linux", "/opt/$tools/ambit"
	if got := piKeyCommand(in); got != `!'/opt/$$tools/ambit' key print --url 'https://llm.example.com'` {
		t.Fatalf("$ must be written as $$: %q", got)
	}
	in.GOOS, in.AmbitPath = "windows", `C:\Users\a$b\ambit.exe`
	if _, err := PlanPiModels(in); err == nil || !strings.Contains(err.Error(), "cannot run a command") {
		t.Fatalf("a $ in a Windows path must be refused: %v", err)
	}
	in.AmbitPath = `C:\Users\O'Brien\ambit.exe`
	if got := piKeyCommand(in); got != `!"C:\Users\O'Brien\ambit.exe" key print --url "https://llm.example.com"` {
		t.Fatalf("cmd.exe reads no single quotes: %q", got)
	}
	env := func(v string) func(string) string { return func(string) string { return v } }
	for v, want := range map[string]string{"": filepath.Join("/h", ".pi", "agent"), "~/pi": filepath.Join("/h", "pi"), "/srv/pi": "/srv/pi"} {
		if got := PiDir(env(v), "/h"); got != want {
			t.Errorf("PI_CODING_AGENT_DIR=%q: %q, want %q", v, got, want)
		}
	}
}
