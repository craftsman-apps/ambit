package connect

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ambit", "connect.json")
	st, err := LoadState(path)
	if err != nil || st.Version != 1 || len(st.Tools) != 0 {
		t.Fatalf("missing file: %+v, %v", st, err)
	}
	prev := `"old"`
	st.SetKey("https://llm.example.com", KeyRecord{Store: StoreKeychain, ConsoleURL: "https://console.example.com"})
	st.Tools["claude-code"] = ToolRecord{URL: "https://llm.example.com", Files: []FileRecord{{
		Path: "/Users/a/.claude/settings.json", Backup: "/Users/a/.claude/settings.json.ambit.bak",
		Entries: []Entry{{Pointer: "/env/ANTHROPIC_MODEL", Wrote: `"m"`, Previous: &prev}, {Pointer: "/env", Wrote: `{}`, CreatedContainer: true}},
	}}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v", fi.Mode().Perm())
		}
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Tools["claude-code"].Files[0].Entries[0]
	if e.Previous == nil || *e.Previous != `"old"` || got.Keys["https://llm.example.com"].Store != StoreKeychain {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Tools["claude-code"].Files[0].Entries[1].Previous != nil {
		t.Fatal("previous null must stay null")
	}
}

func TestStateUsesURL(t *testing.T) {
	st := NewState()
	st.Tools["codex"] = ToolRecord{URL: "https://a"}
	st.Tools["opencode"] = ToolRecord{URL: "https://b"}
	if !st.UsesURL("https://a", "") || st.UsesURL("https://c", "") {
		t.Fatal("UsesURL wrong")
	}
	if st.UsesURL("https://a", "codex") { // the tool being disconnected does not count
		t.Fatal("a tool must not count as another user of the key")
	}
}

func TestLoadStateReportsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connect.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := LoadState(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	os.WriteFile(path, []byte(`{"version": 2}`), 0o600)
	if _, err := LoadState(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v, want a newer-version message", err)
	}
}

func TestStateFileNeverHoldsTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connect.json")
	st := NewState()
	st.SetKey("https://llm.example.com", KeyRecord{Store: StoreFile})
	st.Save(path)
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "ambit_") {
		t.Fatalf("state file holds a key: %s", b)
	}
}
