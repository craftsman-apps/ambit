package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

const mcpURL = "http://mcp.localtest.me:8080/mcp"

const seedDesktop = "{\n  \"mcpServers\": {\n    \"files\": { \"command\": \"files-mcp\" }\n  },\n  \"globalShortcut\": \"\"\n}\n"

func (r *connectRig) desktopPaths() (config, header string) {
	return filepath.Join(r.home, ".config", "Claude", "claude_desktop_config.json"), filepath.Join(r.home, ".ambit", "mcp-headers.txt")
}

func TestConnectClaudeDesktopMCPThenDisconnect(t *testing.T) {
	r := newRig(t)
	config, header := r.desktopPaths()
	r.write(".config/Claude/claude_desktop_config.json", seedDesktop)
	if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	got := r.readFile(config)
	for _, want := range []string{`"files": { "command": "files-mcp" }`, `"mcp-remote@0.14.3"`, `"` + mcpURL + `"`, `"--allow-http"`, `"--header-file"`, jsonString(header)} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q:\n%s", want, got)
		}
	}
	if h := r.readFile(header); h != "Authorization: Bearer "+testAmbitKey+"\n" {
		t.Fatalf("header file: %q", h)
	}
	if fi, _ := os.Stat(header); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("header file mode %v", fi.Mode().Perm())
	}
	if r.kc[mcpURL] != testAmbitKey {
		t.Errorf("the key is not in the keychain: %v", r.kc)
	}
	out := r.stdout.String() + r.stderr.String()
	if strings.Contains(out, testAmbitKey) || !strings.Contains(out, header+": holds the key") || !strings.Contains(out, "Quit Claude Desktop") {
		t.Fatalf("output:\n%s", out)
	}
	st, _ := connect.LoadState(r.statePath)
	raw := r.readFile(r.statePath)
	if len(st.Tools["claude-desktop-mcp"].Files) != 2 || strings.Contains(raw, testAmbitKey) || !strings.Contains(raw, `"secret": true`) {
		t.Fatalf("state:\n%s", raw)
	}

	// A second connect changes nothing.
	if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "already connected") {
		t.Fatalf("again: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}

	if code := r.run("disconnect", "claude-desktop-mcp", "--yes"); code != 0 {
		t.Fatalf("disconnect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(config) != seedDesktop {
		t.Fatalf("config not restored:\n%s", r.readFile(config))
	}
	if r.exists(header) || r.exists(filepath.Dir(header)) {
		t.Error("the header file or ~/.ambit is still there")
	}
	if strings.Contains(r.stdout.String()+r.stderr.String(), testAmbitKey) || len(r.kc) != 0 {
		t.Fatalf("disconnect output shows the key, or the key is still stored:\n%s", r.stdout.String())
	}
}

func TestConnectClaudeDesktopMCPCreatesTheConfig(t *testing.T) {
	r := newRig(t)
	config, header := r.desktopPaths()
	if code := r.run("connect", "claude-desktop-mcp", "--url", "https://mcp.example.com/mcp", "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if strings.Contains(r.readFile(config), "--allow-http") || !strings.Contains(r.stdout.String(), "--model is not used") {
		t.Fatalf("config or warning:\n%s\n%s", r.readFile(config), r.stdout.String())
	}
	r.run("disconnect", "claude-desktop-mcp", "--yes")
	if r.exists(config) || r.exists(header) {
		t.Fatal("files made by connect are still there")
	}
}

func TestConnectClaudeDesktopMCPConflicts(t *testing.T) {
	t.Run("a foreign entry", func(t *testing.T) {
		r := newRig(t)
		config, _ := r.desktopPaths()
		r.write(".config/Claude/claude_desktop_config.json", `{"mcpServers": {"ambit": {"command": "npx", "env": {"AMBIT_AUTH": "Bearer x"}}}}`)
		if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--yes"); code != 1 || !strings.Contains(r.stdout.String(), "mcpServers.ambit already exists") {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--force", "--yes"); code != 0 || strings.Contains(r.readFile(config), "AMBIT_AUTH") {
			t.Fatalf("--force: code %d\n%s", code, r.readFile(config))
		}
	})
	t.Run("a foreign header file", func(t *testing.T) {
		r := newRig(t)
		_, header := r.desktopPaths()
		const old = "Authorization: Bearer ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg\n"
		r.write(".ambit/mcp-headers.txt", old) // mode 0644
		if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--yes"); code != 1 || !strings.Contains(r.stdout.String(), "ambit did not write it") {
			t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		if r.readFile(header) != old {
			t.Fatal("the header file changed without --force")
		}
		if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--force", "--yes"); code != 0 {
			t.Fatalf("--force: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		if fi, _ := os.Stat(header); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("a replaced header file has mode %v", fi.Mode().Perm())
		}
		if r.readFile(header+".ambit.bak") != old {
			t.Error("no backup of the old header file")
		}
		if strings.Contains(r.stdout.String()+r.stderr.String(), "ambit_ABCDEFG") {
			t.Fatal("the old key is in the output")
		}
		r.secret = "ambit_zyxwvutsrqponmlkjihgfedcbaZYXWVUTSRQPONMLKJ"
		if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--replace-key", "--yes"); code != 0 || r.readFile(header+".ambit.bak") != old {
			t.Fatalf("a re-connect lost the backup of the replaced header file: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
		}
		r.run("disconnect", "claude-desktop-mcp", "--yes")
		if r.exists(header) || !strings.Contains(r.stdout.String(), header+".ambit.bak") {
			t.Fatalf("disconnect:\n%s", r.stdout.String())
		}
	})
}

func TestDisconnectClaudeDesktopMCPLeavesAnEditedHeaderFile(t *testing.T) {
	r := newRig(t)
	_, header := r.desktopPaths()
	r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--key-file", r.keyFile(), "--yes")
	const mine = "Authorization: Bearer ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg\n"
	if err := os.WriteFile(header, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.run("disconnect", "claude-desktop-mcp", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(header) != mine || !strings.Contains(r.stdout.String(), "changed after connect") {
		t.Fatalf("an edited header file was not kept:\n%s", r.stdout.String())
	}
}

func TestConnectClaudeDesktopMCPDryRun(t *testing.T) {
	r := newRig(t)
	config, header := r.desktopPaths()
	if code := r.run("connect", "claude-desktop-mcp", "--url", mcpURL, "--dry-run"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.exists(config) || r.exists(header) || len(r.kc) != 0 || !strings.Contains(r.stdout.String(), header+": holds the key") {
		t.Fatalf("dry run wrote something:\n%s", r.stdout.String())
	}
}

// jsonString is s as a JSON string, as the config holds it (a Windows path has its backslashes doubled).
func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
