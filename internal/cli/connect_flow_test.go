package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

// gateway is a fake Ambit LLM address: it records the requests and answers the three formats.
type gateway struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	header []http.Header
	status int // 0: 200
}

func newGateway(t *testing.T) *gateway {
	g := &gateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.header = append(g.header, r.Header.Clone())
		status := g.status
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"nope"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/messages":
			w.Write([]byte(`{"content":[{"type":"text","text":"Hello from messages"}]}`))
		case "/v1/responses":
			w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"Hello from responses"}]}]}`))
		default:
			w.Write([]byte(`{"choices":[{"message":{"content":"Hello from chat"}}]}`))
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *gateway) requests() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.paths) }

func (r *connectRig) write(rel, content string) string {
	r.t.Helper()
	p := filepath.Join(r.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *connectRig) keyFile() string { return r.write("key.txt", testAmbitKey+"\n") }

func (r *connectRig) readFile(p string) string {
	r.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *connectRig) exists(p string) bool { _, err := os.Stat(p); return err == nil }

const seedClaude = "{\n  // my settings\n  \"theme\": \"dark\",\n  \"permissions\": { \"allow\": [\"Bash(ls)\"] }\n}\n"

func TestConnectClaudeCodeThenDisconnect(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	settings := r.write(".claude/settings.json", seedClaude)
	if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m/free", "--key-file", r.keyFile(), "--console-url", "https://console.example.com", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	got := r.readFile(settings)
	for _, want := range []string{`"apiKeyHelper": "'` + fakeAmbit + `' key print --url '` + g.URL + `'"`, `"ANTHROPIC_BASE_URL": "` + g.URL + `"`, `"ANTHROPIC_MODEL": "m/free"`, "// my settings"} {
		if !strings.Contains(got, want) {
			t.Errorf("settings lack %q:\n%s", want, got)
		}
	}
	if r.kc[g.URL] != testAmbitKey {
		t.Fatalf("the key is not in the keychain: %v", r.kc)
	}
	if !r.exists(settings + ".ambit.bak") {
		t.Error("no backup")
	}
	out := r.stdout.String()
	for _, want := range []string{"+++ ", "key: stored in", "✓ test call: Hello from messages", "Restart Claude Code"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if g.requests() != 1 || g.paths[0] != "/v1/messages" || g.header[0].Get("X-Api-Key") != testAmbitKey {
		t.Fatalf("test call: %v %v", g.paths, g.header)
	}
	st, _ := connect.LoadState(r.statePath)
	if st.Tools["claude-code"].URL != g.URL || st.Keys[g.URL].Store != connect.StoreKeychain || st.Keys[g.URL].ConsoleURL != "https://console.example.com" {
		t.Fatalf("state: %+v", st)
	}

	if code := r.run("disconnect", "claude-code", "--yes"); code != 0 {
		t.Fatalf("disconnect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(settings) != seedClaude {
		t.Fatalf("disconnect did not restore the file:\n%s", r.readFile(settings))
	}
	if _, ok := r.kc[g.URL]; ok {
		t.Error("the stored key was not deleted")
	}
	out = r.stdout.String()
	for _, want := range []string{"key for " + g.URL + " deleted", "next: revoke the key in the console: https://console.example.com/keys"} {
		if !strings.Contains(out, want) {
			t.Errorf("disconnect output lacks %q:\n%s", want, out)
		}
	}
	st, _ = connect.LoadState(r.statePath)
	if len(st.Tools) != 0 || len(st.Keys) != 0 {
		t.Fatalf("state after disconnect: %+v", st)
	}
}

func TestConnectNeedsYesWithoutATerminal(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	settings := r.write(".claude/settings.json", seedClaude)
	code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile())
	if code != 2 || !strings.Contains(r.stdout.String()+r.stderr.String(), "run the command again with --yes") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(settings) != seedClaude || len(r.kc) != 0 || r.exists(r.statePath) || g.requests() != 0 {
		t.Fatal("something was changed before the person agreed")
	}
}

const seedCodex = "approval_policy = \"on-request\"\n\n[tui]\nnotifications = true\n"
const seedOpenCode = "{\n  // opencode\n  \"theme\": \"tokyonight\",\n}\n"
const seedZsh = "export PATH=\"$HOME/bin:$PATH\"\nalias ll='ls -l'\n"

// Spec 14 items 1 and 2 through the commands: connect every tool, disconnect --all, every file as it was.
func TestConnectAllToolsThenDisconnectAllRestoresEveryFile(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	claude := r.write(".claude/settings.json", seedClaude)
	codex := r.write(".codex/config.toml", seedCodex)
	oc := r.write(".config/opencode/opencode.json", seedOpenCode)
	zsh := r.write(".zshrc", seedZsh)
	kf := r.keyFile()
	for _, tool := range []string{"claude-code", "codex", "opencode"} {
		if code := r.run("connect", tool, "--url", g.URL, "--model", "m/free", "--key-file", kf, "--yes"); code != 0 {
			t.Fatalf("%s: code %d\n%s%s", tool, code, r.stdout.String(), r.stderr.String())
		}
	}
	if !strings.Contains(r.readFile(zsh), "opencode() { AMBIT_API_KEY=") || !strings.Contains(r.readFile(codex), "model_provider = \"ambit\"") {
		t.Fatalf("zsh:\n%s\ncodex:\n%s", r.readFile(zsh), r.readFile(codex))
	}
	if got := strings.Join(g.paths, " "); got != "/v1/messages /v1/responses /v1/chat/completions" {
		t.Fatalf("test calls: %s", got)
	}
	if len(r.kc) != 1 {
		t.Fatalf("one key per address, shared by the tools: %v", r.kc)
	}
	if code := r.run("disconnect", "--all", "--yes"); code != 0 {
		t.Fatalf("disconnect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	for p, want := range map[string]string{claude: seedClaude, codex: seedCodex, oc: seedOpenCode, zsh: seedZsh} {
		if got := r.readFile(p); got != want {
			t.Errorf("%s was not restored:\n%s", p, got)
		}
	}
	if len(r.kc) != 0 {
		t.Fatalf("the key is still stored: %v", r.kc)
	}
}

func TestConnectCreatesMissingFilesAndDisconnectRemovesThem(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	kf := r.keyFile()
	for _, tool := range []string{"claude-code", "codex", "opencode"} {
		if code := r.run("connect", tool, "--url", g.URL, "--model", "m", "--key-file", kf, "--yes"); code != 0 {
			t.Fatalf("%s: code %d\n%s%s", tool, code, r.stdout.String(), r.stderr.String())
		}
	}
	if !strings.Contains(r.stdout.String(), "does not exist; ambit creates it") {
		t.Errorf("no note about the missing directory:\n%s", r.stdout.String())
	}
	if code := r.run("disconnect", "--all", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	for _, rel := range []string{".claude", ".codex", ".config/opencode", ".zshrc"} {
		if r.exists(filepath.Join(r.home, filepath.FromSlash(rel))) && rel != ".config/opencode" {
			t.Errorf("%s was left behind", rel)
		}
	}
	if r.exists(filepath.Join(r.home, ".config", "opencode", "opencode.json")) {
		t.Error("opencode.json was left behind")
	}
}

func TestConnectDryRunChangesAndCallsNothing(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	settings := r.write(".claude/settings.json", seedClaude)
	code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--dry-run")
	if code != 0 || !strings.Contains(r.stdout.String(), "+++ ") || !strings.Contains(r.stdout.String(), "dry run: no problems found") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(settings) != seedClaude || r.exists(settings+".ambit.bak") || len(r.kc) != 0 || len(r.file) != 0 || r.exists(r.statePath) || g.requests() != 0 {
		t.Fatal("a dry run changed or called something")
	}
}

func TestConnectConflictsStopUnlessForced(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	before := "{\n  \"apiKeyHelper\": \"echo sk-ant-api03-SECRET\",\n  \"env\": { \"ANTHROPIC_AUTH_TOKEN\": \"sk-ant-SECRET\" }\n}\n"
	settings := r.write(".claude/settings.json", before)
	args := []string{"connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"}
	for _, extra := range [][]string{nil, {"--dry-run"}} {
		code := r.run(append(args, extra...)...)
		out := r.stdout.String() + r.stderr.String()
		if code != 1 || !strings.Contains(out, "✗ "+settings+": apiKeyHelper is set to another command") || !strings.Contains(out, "next: ambit connect claude-code --url "+g.URL+" --model m --force") || strings.Contains(out, "SECRET") {
			t.Fatalf("%v: code %d\n%s", extra, code, out)
		}
		if r.readFile(settings) != before || len(r.kc) != 0 || r.exists(r.statePath) {
			t.Fatal("a conflict changed something")
		}
	}
	if code := r.run(append(args, "--force")...); code != 0 {
		t.Fatalf("--force: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if got := r.readFile(settings); strings.Contains(got, "SECRET") || !strings.Contains(got, "key print") {
		t.Fatalf("after --force:\n%s", got)
	}
	if !strings.Contains(r.readFile(settings+".ambit.bak"), "sk-ant-SECRET") {
		t.Error("the removed token must stay in the backup")
	}
	r.run("disconnect", "claude-code", "--yes")
	if !strings.Contains(r.stdout.String(), "env.ANTHROPIC_AUTH_TOKEN was removed at connect; its old value is in "+settings+".ambit.bak") {
		t.Errorf("disconnect does not say where the token is:\n%s", r.stdout.String())
	}
}

func TestConnectKeySources(t *testing.T) {
	g := newGateway(t)
	t.Run("environment", func(t *testing.T) {
		r := newRig(t)
		r.env["AMBIT_API_KEY"] = testAmbitKey
		if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes"); code != 0 || r.kc[g.URL] != testAmbitKey {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("prompt", func(t *testing.T) {
		r := newRig(t)
		r.secret = testAmbitKey
		if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes"); code != 0 || r.kc[g.URL] != testAmbitKey {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("none, no terminal", func(t *testing.T) {
		r := newRig(t)
		code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes")
		if code != 2 || !strings.Contains(r.stderr.String()+r.stdout.String(), "AMBIT_API_KEY") {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("not an Ambit key", func(t *testing.T) {
		r := newRig(t)
		r.env["AMBIT_API_KEY"] = "sk-or-v1-0123456789abcdef"
		code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes")
		if code != 2 || !strings.Contains(r.stdout.String(), "this is not an Ambit key") || strings.Contains(r.stderr.String()+r.stdout.String(), "0123456789abcdef") {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("stored key is reused, --replace-key asks again", func(t *testing.T) {
		r := newRig(t)
		r.env["AMBIT_API_KEY"] = testAmbitKey
		r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes")
		r.env["AMBIT_API_KEY"] = ""
		if code := r.run("connect", "codex", "--url", g.URL, "--model", "m", "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "key: reused") {
			t.Fatalf("reuse: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		const other = "ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg"
		r.secret = other
		if code := r.run("connect", "codex", "--url", g.URL, "--model", "m2", "--replace-key", "--yes"); code != 0 || r.kc[g.URL] != other {
			t.Fatalf("replace: code %d, key %q\n%s%s", code, r.kc[g.URL], r.stdout.String(), r.stderr.String())
		}
	})
	t.Run("file fallback", func(t *testing.T) {
		r := newRig(t)
		r.env["AMBIT_API_KEY"] = testAmbitKey
		if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-store", "file", "--yes"); code != 0 || r.file[g.URL] != testAmbitKey || len(r.kc) != 0 {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		st, _ := connect.LoadState(r.statePath)
		if st.Keys[g.URL].Store != connect.StoreFile {
			t.Fatalf("state: %+v", st.Keys)
		}
		r.run("disconnect", "claude-code", "--yes")
		if len(r.file) != 0 {
			t.Error("the key file was not deleted")
		}
	})
}

func TestConnectTestCallFailureKeepsTheConfig(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   string
	}{
		{401, "the key was refused, or no access rule allows m"},
		{403, "the key was refused, or no access rule allows m"},
		{404, "the gateway does not know m"},
		{500, "provider"},
	} {
		g, r := newGateway(t), newRig(t)
		g.status = tt.status
		settings := r.write(".claude/settings.json", seedClaude)
		code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
		out := r.stdout.String() + r.stderr.String()
		if code != 1 || !strings.Contains(out, tt.want) || !strings.Contains(out, "to undo: ambit disconnect claude-code") {
			t.Errorf("HTTP %d: code %d\n%s", tt.status, code, out)
		}
		if !strings.Contains(r.readFile(settings), "apiKeyHelper") {
			t.Errorf("HTTP %d: the config must stay", tt.status)
		}
	}
	// A gateway that is not there.
	r := newRig(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	code := r.run("connect", "claude-code", "--url", url, "--model", "m", "--key-file", r.keyFile(), "--yes")
	if code != 1 || !strings.Contains(r.stdout.String(), "cannot reach "+url) {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	// --no-test skips it.
	g, r := newGateway(t), newRig(t)
	if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--no-test"); code != 0 || g.requests() != 0 {
		t.Fatalf("--no-test: code %d, %d requests", code, g.requests())
	}
}

func TestConnectInvalidFileChangesNothing(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	bad := "{\n  \"env\": {\n    \"A\": \n}\n"
	settings := r.write(".claude/settings.json", bad)
	code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	out := r.stdout.String() + r.stderr.String()
	if code != 1 || !strings.Contains(out, "is not valid JSON") || !strings.Contains(out, "next: fix the file, then run the command again") {
		t.Fatalf("code %d\n%s", code, out)
	}
	if r.readFile(settings) != bad || len(r.kc) != 0 || r.exists(r.statePath) || r.exists(settings+".ambit.bak") {
		t.Fatal("an invalid file changed something")
	}
}

func TestConnectOpenCodeV2IsRefused(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	v2 := "{\"providers\": {\"x\": {}}}\n"
	oc := r.write(".config/opencode/opencode.json", v2)
	for _, extra := range [][]string{nil, {"--dry-run"}} {
		args := append([]string{"connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"}, extra...)
		code := r.run(args...)
		if code != 1 || !strings.Contains(r.stderr.String()+r.stdout.String(), "uses the opencode v2 config format") || r.readFile(oc) != v2 || len(r.kc) != 0 {
			t.Fatalf("%v: code %d\n%s%s", extra, code, r.stdout.String(), r.stderr.String())
		}
	}
}

func TestConnectOpenCodeShellNonePrintsTheFunction(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	zsh := r.write(".zshrc", seedZsh)
	code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--shell", "none")
	if code != 0 || r.readFile(zsh) != seedZsh || !strings.Contains(r.stdout.String(), "opencode() { AMBIT_API_KEY=") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	// An unknown shell is the same, and a wrong --shell value is a usage error.
	r.env["SHELL"] = "/bin/tcsh"
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "opencode() { AMBIT_API_KEY=") {
		t.Fatalf("tcsh: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--shell", "ksh", "--dry-run"); code != 2 {
		t.Fatalf("--shell ksh: code %d", code)
	}
}

func TestConnectAlreadyConnected(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	settings := r.write(".claude/settings.json", seedClaude)
	args := []string{"connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"}
	r.run(args...)
	after := r.readFile(settings)
	os.Remove(settings + ".ambit.bak")
	if code := r.run(args...); code != 0 || !strings.Contains(r.stdout.String(), "✓ claude-code is already connected to "+g.URL) || !strings.Contains(r.stdout.String(), "test call") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(settings) != after || r.exists(settings+".ambit.bak") {
		t.Fatal("a second identical connect wrote the file")
	}
}

func TestConnectUpdateToANewURLThenDisconnectRestoresTheFirstState(t *testing.T) {
	g, g2, r := newGateway(t), newGateway(t), newRig(t)
	codex := r.write(".codex/config.toml", seedCodex)
	kf := r.keyFile()
	r.run("connect", "codex", "--url", g.URL, "--model", "m1", "--key-file", kf, "--yes")
	if code := r.run("connect", "codex", "--url", g2.URL, "--model", "m2", "--key-file", kf, "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if got := r.readFile(codex); strings.Contains(got, g.URL) || !strings.Contains(got, g2.URL) {
		t.Fatalf("not updated:\n%s", got)
	}
	r.run("disconnect", "codex", "--yes")
	if r.readFile(codex) != seedCodex {
		t.Fatalf("not restored:\n%s", r.readFile(codex))
	}
	// Both keys: the first one is no longer used by any tool, but it was never removed at the first update.
	if len(r.kc) != 1 {
		t.Logf("keys left: %v", r.kc)
	}
}

func TestDisconnectKeepsTheKeyWhileAnotherToolUsesIt(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	kf := r.keyFile()
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", kf, "--yes")
	r.run("connect", "codex", "--url", g.URL, "--model", "m", "--key-file", kf, "--yes")
	if code := r.run("disconnect", "codex", "--yes"); code != 0 || r.kc[g.URL] == "" || strings.Contains(r.stdout.String(), "revoke") {
		t.Fatalf("code %d, keys %v\n%s", code, r.kc, r.stdout.String())
	}
	// --keep-key keeps it for the last tool too, and prints no revoke line.
	if code := r.run("disconnect", "claude-code", "--keep-key", "--yes"); code != 0 || r.kc[g.URL] == "" || strings.Contains(r.stdout.String(), "revoke") {
		t.Fatalf("--keep-key: code %d, keys %v\n%s", code, r.kc, r.stdout.String())
	}
}

func TestDisconnectRevokeLineWithoutAConsoleURL(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	r.run("disconnect", "claude-code", "--yes")
	want := "next: revoke the key in the console (Settings > Keys); deleting it here does not revoke it"
	if !strings.Contains(r.stdout.String(), want) {
		t.Fatalf("no revoke line:\n%s", r.stdout.String())
	}
	// --console-url at disconnect overrides.
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--console-url", "https://old.example.com")
	r.run("disconnect", "claude-code", "--yes", "--console-url", "https://new.example.com/")
	if !strings.Contains(r.stdout.String(), "revoke the key in the console: https://new.example.com/keys") {
		t.Fatalf("override:\n%s", r.stdout.String())
	}
}

func TestDisconnectUsageAndNothingRecorded(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{{"disconnect"}, {"disconnect", "codex", "--all"}, {"disconnect", "vim"}, {"disconnect", "codex", "--revoke"}, {"connect", "vim", "--url", "https://x.example.com", "--model", "m"},
		{"connect", "codex", "--model", "m"}, {"connect", "codex", "--url", "https://x.example.com"}, {"connect", "codex", "--url", "ftp://x", "--model", "m"}, {"connect", "codex", "--url", "https://x.example.com", "--model", "m", "--output", "json"}} {
		if code := r.run(args...); code != 2 {
			t.Errorf("%v: code %d, want 2\n%s%s", args, code, r.stdout.String(), r.stderr.String())
		}
	}
	if code := r.run("disconnect", "codex", "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "✓ codex is not connected (nothing recorded)") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if code := r.run("disconnect", "--all", "--yes"); code != 0 {
		t.Fatalf("--all with nothing: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
}

// The markers prove ownership, so a Codex block is removed even when the state file is gone.
func TestDisconnectRemovesMarkedBlocksWithoutTheStateFile(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	codex := r.write(".codex/config.toml", seedCodex)
	r.run("connect", "codex", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	os.Remove(r.statePath)
	if code := r.run("disconnect", "codex", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	got := r.readFile(codex)
	if strings.Contains(got, "ambit") || !strings.Contains(got, "[tui]\nnotifications = true") {
		t.Fatalf("after disconnect:\n%s", got)
	}
}

func TestConnectCursorWritesNothing(t *testing.T) {
	r := newRig(t)
	if code := r.run("connect", "cursor", "--url", "http://llm.localtest.me:8080", "--model", "m"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.stdout.String()
	for _, want := range []string{"Override OpenAI Base URL: http://llm.localtest.me:8080/v1", "Model name: m", "is not a public HTTPS address"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(r.kc) != 0 {
		t.Error("cursor stored a key")
	}
	r.kc["https://llm.example.com"] = testAmbitKey
	r.state("https://llm.example.com", connect.StoreKeychain)
	r.run("connect", "cursor", "--url", "https://llm.example.com")
	if !strings.Contains(r.stdout.String(), "Copy the key with: ambit key print --url https://llm.example.com --reveal") || !strings.Contains(r.stdout.String(), "<model-id>") {
		t.Fatalf("with a stored key:\n%s", r.stdout.String())
	}
	st, _ := connect.LoadState(r.statePath)
	if !st.CursorShown || len(st.Tools) != 0 {
		t.Fatalf("state: %+v", st)
	}
	if code := r.run("disconnect", "--all", "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "Turn off \"Override OpenAI Base URL\"") {
		t.Fatalf("--all reminder: code %d\n%s", code, r.stdout.String())
	}
	if code := r.run("disconnect", "cursor"); code != 0 || !strings.Contains(r.stdout.String(), "Turn off") {
		t.Fatalf("disconnect cursor: code %d\n%s", code, r.stdout.String())
	}
}

// Spec 14 item 9: run with --verbose and a key; the key is in the store only.
func TestConnectNeverPrintsOrWritesTheKey(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	files := []string{r.write(".claude/settings.json", seedClaude), r.write(".codex/config.toml", seedCodex), r.write(".config/opencode/opencode.json", seedOpenCode), r.write(".zshrc", seedZsh)}
	var all strings.Builder
	for _, args := range [][]string{
		{"connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--verbose"},
		{"connect", "codex", "--url", g.URL, "--model", "m", "--yes", "--verbose"},
		{"connect", "opencode", "--url", g.URL, "--model", "m", "--yes", "--verbose"},
		{"connect", "claude-code", "--url", g.URL, "--model", "m", "--dry-run", "--verbose"},
		{"disconnect", "--all", "--yes", "--verbose"},
	} {
		r.run(args...)
		all.WriteString(r.stdout.String() + r.stderr.String())
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				all.Write(b)
			}
			if b, err := os.ReadFile(f + ".ambit.bak"); err == nil {
				all.Write(b)
			}
		}
		if b, err := os.ReadFile(r.statePath); err == nil {
			all.Write(b)
		}
	}
	if strings.Contains(all.String(), testAmbitKey) || strings.Contains(all.String(), "ambit_0123") {
		t.Fatalf("the key leaked:\n%s", all.String())
	}
}

func TestConnectWriteFailureOfTheSecondFileRestoresTheFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file modes do not stop root")
	}
	g, r := newGateway(t), newRig(t)
	oc := r.write(".config/opencode/opencode.json", seedOpenCode)
	zsh := r.write(".zshrc", seedZsh)
	os.Chmod(zsh, 0o400)
	defer os.Chmod(zsh, 0o644)
	code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	if code != 1 || r.readFile(oc) != seedOpenCode || !strings.Contains(r.stdout.String()+r.stderr.String(), "chmod u+w "+zsh) {
		t.Fatalf("code %d\n%s%s\n%s", code, r.stdout.String(), r.stderr.String(), r.readFile(oc))
	}
}
