package connect

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeDesktopConfigPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := filepath.FromSlash("/home/u")
	for _, tc := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "/home/u/Library/Application Support/Claude/claude_desktop_config.json"},
		{"windows", map[string]string{"APPDATA": filepath.FromSlash("/appdata")}, "/appdata/Claude/claude_desktop_config.json"},
		{"windows", nil, "/home/u/AppData/Roaming/Claude/claude_desktop_config.json"},
		{"linux", nil, "/home/u/.config/Claude/claude_desktop_config.json"},
		{"linux", map[string]string{"XDG_CONFIG_HOME": filepath.FromSlash("/xdg")}, "/xdg/Claude/claude_desktop_config.json"},
	} {
		if got := ClaudeDesktopConfigPath(tc.goos, env(tc.env), home); got != filepath.FromSlash(tc.want) {
			t.Errorf("%s %v: %s", tc.goos, tc.env, got)
		}
	}
}

func TestNeedsAllowHTTP(t *testing.T) {
	for addr, want := range map[string]bool{
		"http://mcp.localtest.me:8080/mcp": true, "https://mcp.example.com/mcp": false,
		"http://localhost:8080/mcp": false, "http://127.0.0.1/mcp": false, "http://[::1]:9000/mcp": false,
	} {
		if needsAllowHTTP(addr) != want {
			t.Errorf("%s: want %v", addr, want)
		}
	}
}

func TestPlanClaudeDesktopMCPKeepsOtherServersAndComments(t *testing.T) {
	file := "{\n  // mine\n  \"mcpServers\": {\n    \"files\": { \"command\": \"files-mcp\" }\n  }\n}\n"
	in := Input{Path: "c.json", File: []byte(file), Exists: true, URL: "https://mcp.example.com/mcp"}
	p, err := PlanClaudeDesktopMCP(in, "/h/.ambit/mcp-headers.txt")
	if err != nil || len(p.Conflicts) != 0 {
		t.Fatal(err, p.Conflicts)
	}
	got := string(p.After)
	for _, want := range []string{"// mine", `"files": { "command": "files-mcp" }`, `"command": "npx"`, `"mcp-remote@0.14.3",
        "https://mcp.example.com/mcp",
        "--header-file",
        "/h/.ambit/mcp-headers.txt"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--allow-http") {
		t.Error("--allow-http for https")
	}
	d, err := UnplanJSON(p.Record, p.After, true)
	if err != nil || string(d.After) != file {
		t.Fatalf("disconnect did not restore the file: %v\n%s", err, d.After)
	}
}

func TestUnplanSecretFile(t *testing.T) {
	const key = "ambit_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	wrote := MCPHeader(key)
	rec := FileRecord{Path: "h", Secret: true, SHA256: FileSum(wrote), Dirs: []string{"d"}}
	if d := UnplanSecretFile(rec, wrote, true); !d.Delete || len(d.RemoveDirs) != 1 {
		t.Fatalf("not deleted: %+v", d)
	}
	other := MCPHeader("ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg")
	if d := UnplanSecretFile(rec, other, true); d.Delete || len(d.Notes) != 1 || strings.Contains(d.Notes[0], "ambit_") {
		t.Fatalf("a changed file was deleted, or the note shows a key: %+v", d)
	}
	if d := UnplanSecretFile(rec, nil, false); d.Delete {
		t.Fatal("a missing file is deleted")
	}
}
