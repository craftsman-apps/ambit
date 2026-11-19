package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const mcpAddr = "https://mcp.example.com/mcp"

func (r *connectRig) mustRun(args ...string) string {
	r.t.Helper()
	if code := r.run(args...); code != 0 {
		r.t.Fatalf("%v: code %d\n%s%s", args, code, r.stdout.String(), r.stderr.String())
	}
	return r.stdout.String()
}

func (r *connectRig) privateFile(p, want string) {
	r.t.Helper()
	if got := r.readFile(p); got != want {
		r.t.Fatalf("%s holds %q, want %q", p, got, want)
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		r.t.Fatalf("%s mode %v", p, fi.Mode().Perm())
	}
}

// noKey fails when the output or a config file shows the key.
func (r *connectRig) noKey(out string, files ...string) {
	r.t.Helper()
	if strings.Contains(out, testAmbitKey) || strings.Contains(out, otherAmbitKey) {
		r.t.Fatalf("the output shows the key:\n%s", out)
	}
	for _, f := range files {
		if c := r.readFile(f); strings.Contains(c, "ambit_") {
			r.t.Fatalf("%s holds a key:\n%s", f, c)
		}
	}
}

const seedHermes = "# my Hermes\nmodel:\n  default: anthropic/claude-sonnet-4\n  provider: auto\n\nproviders:\n  together:\n    api: https://api.together.xyz/v1\n    key_env: TOGETHER_API_KEY\n"

// Hermes: the provider runs ambit key print; the MCP server is mcp-remote with a user-only header file. A second
// run changes nothing, ambit key set renews the header file, and disconnect gives config.yaml back byte for byte.
func TestConnectHermesFlow(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	conf := r.write(".hermes/config.yaml", seedHermes)
	header := filepath.Join(r.home, ".ambit", "hermes-mcp-headers.txt")
	// the model is the person's: a conflict until --force
	if code := r.run("connect", "hermes", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--key-file", r.keyFile(), "--yes"); code == 0 ||
		!strings.Contains(r.stdout.String()+r.stderr.String(), "model is set to another value") || r.readFile(conf) != seedHermes || r.exists(header) {
		t.Fatalf("conflict: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.mustRun("connect", "hermes", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--key-file", r.keyFile(), "--yes", "--force")
	c := r.readFile(conf)
	for _, want := range []string{`key_cmd: "'` + fakeAmbit + `' key print --url '` + g.URL + `'"`, `provider: "custom:ambit"`, "  together:", `"--header-file", ` + jsonString(header)} {
		if !strings.Contains(c, want) {
			t.Fatalf("config.yaml lacks %q:\n%s", want, c)
		}
	}
	r.privateFile(header, "Authorization: Bearer "+testAmbitKey+"\n")
	r.noKey(out, conf)
	if !strings.Contains(out, "✓ test call:") || !strings.Contains(out, "hermes gateway restart") {
		t.Fatalf("output:\n%s", out)
	}
	if out := r.mustRun("connect", "hermes", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--yes"); !strings.Contains(out, "hermes is already connected") {
		t.Fatalf("rerun:\n%s", out)
	}
	r.env["AMBIT_API_KEY"] = otherAmbitKey
	if out := r.mustRun("key", "set", "--url", g.URL); !strings.Contains(out, "wrote the new key to "+header) {
		t.Fatalf("key set:\n%s", out)
	}
	r.privateFile(header, "Authorization: Bearer "+otherAmbitKey+"\n")
	r.mustRun("disconnect", "hermes", "--yes")
	if r.readFile(conf) != seedHermes || r.exists(header) {
		t.Fatalf("after disconnect:\n%s\nheader file left: %v", r.readFile(conf), r.exists(header))
	}
}

const seedKilo = "{\n  \"$schema\": \"https://app.kilo.ai/config.json\",\n  // mine\n  \"provider\": { \"lmstudio\": { \"options\": { \"baseURL\": \"http://127.0.0.1:1234/v1\" } } }\n}\n"

// Kilo Code: the key goes to a user-only key file that the provider and the MCP header refer to; --replace-key and
// ambit key set renew it; disconnect restores kilo.jsonc and deletes the key file.
func TestConnectKiloCodeFlow(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	conf := r.write(".config/kilo/kilo.jsonc", seedKilo)
	kp := filepath.Join(r.home, ".config", "ambit", "kilo-key")
	out := r.mustRun("connect", "kilocode", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--key-file", r.keyFile(), "--yes")
	c := r.readFile(conf)
	for _, want := range []string{`"apiKey":` + jsonString("{file:"+kp+"}"), `"Authorization":` + jsonString("Bearer {file:"+kp+"}"), `"model": "ambit/m"`, "// mine", `"lmstudio"`} {
		if !strings.Contains(strings.ReplaceAll(c, `": "`, `":"`), strings.ReplaceAll(want, `": "`, `":"`)) {
			t.Fatalf("kilo.jsonc lacks %q:\n%s", want, c)
		}
	}
	r.privateFile(kp, testAmbitKey)
	r.noKey(out, conf)
	if !strings.Contains(out, kp+": holds the key, readable only by you (mode 0600)") || !strings.Contains(out, "Reload Window") {
		t.Fatalf("output:\n%s", out)
	}
	if out := r.mustRun("connect", "kilocode", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--yes"); !strings.Contains(out, "kilocode is already connected") {
		t.Fatalf("rerun:\n%s", out)
	}
	r.env["AMBIT_API_KEY"] = otherAmbitKey
	r.mustRun("connect", "kilocode", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--replace-key", "--yes")
	r.privateFile(kp, otherAmbitKey)
	r.env["AMBIT_API_KEY"] = testAmbitKey
	if out := r.mustRun("key", "set", "--url", g.URL); !strings.Contains(out, "wrote the new key to "+kp) {
		t.Fatalf("key set:\n%s", out)
	}
	r.privateFile(kp, testAmbitKey)
	r.mustRun("disconnect", "kilocode", "--yes") // a key from ambit key set stays stored: only --all deletes it
	if r.readFile(conf) != seedKilo || r.exists(kp) || r.kc[g.URL] != testAmbitKey {
		t.Fatalf("after disconnect:\n%s\nkey file left %v, keys %v", r.readFile(conf), r.exists(kp), r.kc)
	}
}

// pi: models.json gets a provider whose apiKey runs ambit key print, settings.json the default, mcp.json a server
// that sends the provider's key. No file holds the key; disconnect restores the files and deletes the new one.
func TestConnectPiFlow(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	seedModels := "{\n  \"providers\": { \"ollama\": { \"baseUrl\": \"http://localhost:11434/v1\", \"api\": \"openai-completions\", \"apiKey\": \"ollama\", \"models\": [{ \"id\": \"qwen\" }] } }\n}\n"
	seedSettings := "{\n  \"theme\": \"dark\"\n}\n"
	models := r.write(".pi/agent/models.json", seedModels)
	settings := r.write(".pi/agent/settings.json", seedSettings)
	mcp := filepath.Join(r.home, ".pi", "agent", "mcp.json")
	out := r.mustRun("connect", "pi", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--key-file", r.keyFile(), "--yes")
	if c := r.readFile(models); !strings.Contains(c, `!'`+fakeAmbit+`' key print --url '`+g.URL+`'`) || !strings.Contains(c, `"ollama"`) {
		t.Fatalf("models.json:\n%s", c)
	}
	if c := r.readFile(settings); !strings.Contains(c, `"defaultProvider": "ambit"`) || !strings.Contains(c, `"defaultModel": "m"`) {
		t.Fatalf("settings.json:\n%s", c)
	}
	if c := r.readFile(mcp); !strings.Contains(c, `"provider": "ambit"`) || !strings.Contains(c, mcpAddr) {
		t.Fatalf("mcp.json:\n%s", c)
	}
	r.noKey(out, models, settings, mcp)
	if r.kc[g.URL] != testAmbitKey || !strings.Contains(out, "✓ test call:") {
		t.Fatalf("keychain %v\n%s", r.kc, out)
	}
	if out := r.mustRun("connect", "pi", "--url", g.URL, "--model", "m", "--mcp-url", mcpAddr, "--yes"); !strings.Contains(out, "pi is already connected") {
		t.Fatalf("rerun:\n%s", out)
	}
	r.mustRun("disconnect", "pi", "--yes")
	if r.readFile(models) != seedModels || r.readFile(settings) != seedSettings || r.exists(mcp) || len(r.kc) != 0 {
		t.Fatalf("after disconnect:\n%s\n%s\nmcp.json left %v", r.readFile(models), r.readFile(settings), r.exists(mcp))
	}
}

// A config file that does not parse stops connect with a clear message: nothing is written and no key is stored.
func TestConnectNewToolsInvalidFile(t *testing.T) {
	for tool, rel := range map[string]string{"hermes": ".hermes/config.yaml", "kilocode": ".config/kilo/kilo.jsonc", "pi": ".pi/agent/models.json"} {
		t.Run(tool, func(t *testing.T) {
			g, r := newGateway(t), newRig(t)
			bad := "model: [unclosed\n{\"provider\": \n"
			p := r.write(rel, bad)
			code := r.run("connect", tool, "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--force")
			msg := r.stdout.String() + r.stderr.String()
			if code == 0 || !strings.Contains(msg, p+" is not valid") || r.readFile(p) != bad || len(r.kc) != 0 {
				t.Fatalf("code %d, keys %v\n%s", code, r.kc, msg)
			}
			if r.exists(filepath.Join(r.home, ".config", "ambit", "kilo-key")) {
				t.Fatal("a key file was written")
			}
		})
	}
}

func TestConnectMCPURLOnlyForNewTools(t *testing.T) {
	r := newRig(t)
	if code := r.run("connect", "codex", "--url", "https://llm.example.com", "--model", "m", "--mcp-url", mcpAddr); code != 2 ||
		!strings.Contains(r.stderr.String()+r.stdout.String(), "--mcp-url") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
}
