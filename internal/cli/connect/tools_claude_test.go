package connect

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const ambitBin = "/opt/homebrew/bin/ambit"

func claudeInput(c goldenCase, s goldenStep, cur []byte, exists bool, prev *FileRecord) Input {
	return Input{Path: "/home/a/.claude/settings.json", File: cur, Exists: exists, URL: s.URL, Model: s.Model, AmbitPath: ambitBin,
		GOOS: "darwin", Force: s.Force, Getenv: func(k string) string { return s.Env[k] }, Prev: prev}
}

func TestClaudeCodeGolden(t *testing.T) {
	for _, c := range loadGolden(t, "claude-code") {
		t.Run(filepath.Base(c.dir), func(t *testing.T) {
			cur, exists := c.before, c.exists
			var prev *FileRecord
			var last *Plan
			for _, s := range c.steps {
				p, err := PlanClaudeCode(claudeInput(c, s, cur, exists, prev))
				if err != nil {
					t.Fatal(err)
				}
				last = p
				if p.After == nil {
					break
				}
				cur, exists, prev = p.After, true, &p.Record
			}
			c.check(t, "conflicts", conflictLines(last.Conflicts))
			if last.After == nil {
				c.check(t, "after", nil)
				return
			}
			c.check(t, "after", strp(string(last.After)))
			// connect then disconnect gives the file back.
			d, err := UnplanJSON(*prev, cur, true)
			if err != nil {
				t.Fatal(err)
			}
			want, wantExists := c.before, c.exists
			if len(prev.RemovedSecrets) > 0 {
				c.check(t, "roundtrip", strp(string(d.After)))
				return
			}
			switch {
			case !wantExists:
				if !d.Delete {
					t.Fatalf("a file connect created must be deleted, got %q", d.After)
				}
			case d.Delete || string(d.After) != string(want):
				t.Fatalf("disconnect did not restore the file\n got  %q\n want %q", d.After, want)
			}
		})
	}
}

// Section 14 item 9, for this tool: the key is never in a file, and the state record holds no secret.
func TestClaudeCodeNeverWritesAKey(t *testing.T) {
	const token = "ambit_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	before := `{"env": {"ANTHROPIC_AUTH_TOKEN": "` + token + `", "FOO": "1"}}` + "\n"
	in := claudeInput(goldenCase{}, goldenStep{URL: "https://llm.example.com", Model: "m", Force: true}, []byte(before), true, nil)
	p, err := PlanClaudeCode(in)
	if err != nil || p.After == nil {
		t.Fatalf("plan: %v, %+v", err, p)
	}
	rec, _ := json.Marshal(p.Record)
	for name, s := range map[string]string{"the new file": string(p.After), "the state record": string(rec)} {
		if strings.Contains(s, token) || strings.Contains(s, "ambit_") {
			t.Errorf("%s holds a key: %s", name, s)
		}
	}
	if len(p.Record.RemovedSecrets) != 1 || p.Record.RemovedSecrets[0] != "/env/ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("RemovedSecrets = %v", p.Record.RemovedSecrets)
	}
	// Disconnect says where the old value is, and does not put it back.
	p.Record.Backup = "/home/a/.claude/settings.json.ambit.bak"
	d, err := UnplanJSON(p.Record, p.After, true)
	if err != nil || strings.Contains(string(d.After), token) {
		t.Fatalf("disconnect: %v %q", err, d.After)
	}
	if len(d.Notes) != 1 || d.Notes[0] != "env.ANTHROPIC_AUTH_TOKEN was removed at connect; its old value is in /home/a/.claude/settings.json.ambit.bak" {
		t.Fatalf("notes = %q", d.Notes)
	}
}

func TestClaudeCodeShellEnvWarning(t *testing.T) {
	in := claudeInput(goldenCase{}, goldenStep{URL: "https://llm.example.com", Model: "m", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"}}, nil, false, nil)
	p, _ := PlanClaudeCode(in)
	if len(p.Warnings) != 1 || p.Warnings[0] != "ANTHROPIC_AUTH_TOKEN is set in your shell and outranks Ambit's key; remove it from your shell profile" {
		t.Fatalf("warnings = %q", p.Warnings)
	}
}

func TestClaudeCodeWindowsQuoting(t *testing.T) {
	in := claudeInput(goldenCase{}, goldenStep{URL: "https://llm.example.com", Model: "m"}, nil, false, nil)
	in.GOOS, in.AmbitPath = "windows", `C:\Users\First Last\bin\ambit.exe`
	p, _ := PlanClaudeCode(in)
	want := `"apiKeyHelper": "\"C:\\Users\\First Last\\bin\\ambit.exe\" key print --url https://llm.example.com"`
	if !strings.Contains(string(p.After), want) {
		t.Fatalf("got %s", p.After)
	}
}

func TestClaudeCodeInvalidJSONChangesNothing(t *testing.T) {
	in := claudeInput(goldenCase{}, goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("{\n  \"env\": {\n    \"A\": \n}\n"), true, nil)
	_, err := PlanClaudeCode(in)
	if err == nil || !strings.Contains(err.Error(), "settings.json is not valid JSON") || !strings.Contains(err.Error(), "line") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeSettingsPath(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := ClaudeSettingsPath(get(nil), "/home/a"); got != filepath.FromSlash("/home/a/.claude/settings.json") {
		t.Fatal(got)
	}
	if got := ClaudeSettingsPath(get(map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}), "/home/a"); got != filepath.FromSlash("/cfg/settings.json") {
		t.Fatal(got)
	}
}

func TestClaudeCodeDisconnectLeavesAnEditedEntry(t *testing.T) {
	in := claudeInput(goldenCase{}, goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("{\n  \"theme\": \"dark\"\n}\n"), true, nil)
	p, _ := PlanClaudeCode(in)
	edited := strings.Replace(string(p.After), `"ANTHROPIC_MODEL": "m"`, `"ANTHROPIC_MODEL": "mine"`, 1)
	d, err := UnplanJSON(p.Record, []byte(edited), true)
	if err != nil || len(d.Notes) != 1 || d.Notes[0] != "env.ANTHROPIC_MODEL was changed after connect; left as it is" || !strings.Contains(string(d.After), "mine") {
		t.Fatalf("%v %q %s", err, d.Notes, d.After)
	}
}

func TestClaudeCodeFileGoneAtDisconnect(t *testing.T) {
	d, err := UnplanJSON(FileRecord{Path: "/x"}, nil, false)
	if err != nil || d.After != nil || d.Delete {
		t.Fatalf("%v %+v", err, d)
	}
}
