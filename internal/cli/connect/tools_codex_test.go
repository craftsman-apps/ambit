package connect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func codexInput(s goldenStep, cur []byte, exists bool, prev *FileRecord) Input {
	return Input{Path: "/home/a/.codex/config.toml", File: cur, Exists: exists, URL: s.URL, Model: s.Model, AmbitPath: ambitBin,
		GOOS: "darwin", Force: s.Force, Getenv: func(k string) string { return s.Env[k] }, Prev: prev}
}

func TestCodexGolden(t *testing.T) {
	for _, c := range loadGolden(t, "codex") {
		t.Run(filepath.Base(c.dir), func(t *testing.T) {
			cur, exists := c.before, c.exists
			var prev *FileRecord
			var last *Plan
			for _, s := range c.steps {
				p, err := PlanCodex(codexInput(s, cur, exists, prev))
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
			// The result is valid TOML, and the file reads as Codex expects.
			var m struct {
				Model         string `toml:"model"`
				ModelProvider string `toml:"model_provider"`
			}
			if _, err := toml.Decode(strings.TrimPrefix(string(last.After), bom), &m); err != nil || m.ModelProvider != "ambit" {
				t.Fatalf("decode: %v %+v", err, m)
			}
			// connect then disconnect gives the file back.
			d, err := UnplanBlocks(prev, cur, true)
			if err != nil {
				t.Fatal(err)
			}
			if !c.exists {
				if !d.Delete {
					t.Fatalf("a file connect created must be deleted, got %q", d.After)
				}
				return
			}
			if d.Delete || string(d.After) != string(c.before) {
				t.Fatalf("disconnect did not restore the file\n got  %q\n want %q", d.After, c.before)
			}
		})
	}
}

func TestCodexInvalidTOMLChangesNothing(t *testing.T) {
	_, err := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("a = 1\nb = \n"), true, nil))
	if err == nil || !strings.Contains(err.Error(), "config.toml is not valid TOML — line 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexUnterminatedBlockIsAnError(t *testing.T) {
	_, err := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("a = 1\n"+codexBegin+"\nx = 1\n"), true, nil))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexConflictLinesNeverPrintAValue(t *testing.T) {
	before := "model = \"secret-model-name\"\nmodel_provider = \"secret-provider\"\n[model_providers.ambit]\nname = \"secret-name\"\n"
	p, err := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte(before), true, nil))
	if err != nil || len(p.Conflicts) != 3 {
		t.Fatalf("%v %+v", err, p)
	}
	for _, c := range p.Conflicts {
		if strings.Contains(c.String(), "secret") {
			t.Errorf("conflict line prints a value: %s", c)
		}
	}
}

func TestCodexWindowsPathIsAnEscapedTOMLString(t *testing.T) {
	in := codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, nil, false, nil)
	in.GOOS, in.AmbitPath = "windows", `C:\Users\First Last\bin\ambit.exe`
	p, err := PlanCodex(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.After), `command = "C:\\Users\\First Last\\bin\\ambit.exe"`) {
		t.Fatalf("got %s", p.After)
	}
	var m struct {
		P map[string]struct{ Auth struct{ Command string } } `toml:"model_providers"`
	}
	if _, err := toml.Decode(string(p.After), &m); err != nil || m.P["ambit"].Auth.Command != in.AmbitPath {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestCodexNoKeyInTheFile(t *testing.T) {
	p, _ := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, nil, false, nil))
	if strings.Contains(string(p.After), "ambit_") || strings.Contains(string(p.After), "env_key") {
		t.Fatalf("%s", p.After)
	}
}

func TestCodexDisconnectLeavesAnEditedBlock(t *testing.T) {
	p, _ := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("a = 1\n\n[tui]\nx = 1\n"), true, nil))
	edited := strings.Replace(string(p.After), `name = "Ambit"`, `name = "Mine"`, 1)
	d, err := UnplanBlocks(&p.Record, []byte(edited), true)
	if err != nil || len(d.Notes) != 1 || d.Notes[0] != "the Ambit block in /home/a/.codex/config.toml was edited after connect; left as it is" {
		t.Fatalf("%v %q", err, d.Notes)
	}
	if !strings.Contains(string(d.After), "Mine") || strings.Contains(string(d.After), `model_provider = "ambit"`) {
		t.Fatalf("the edited block stays and the other one goes:\n%s", d.After)
	}
}

func TestCodexDisconnectWithoutARecordRemovesTheMarkedBlocks(t *testing.T) {
	before := "a = 1\n\n[tui]\nx = 1\n"
	p, _ := PlanCodex(codexInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte(before), true, nil))
	d, err := UnplanBlocks(nil, p.After, true)
	if err != nil || strings.Contains(string(d.After), "ambit") {
		t.Fatalf("%v %q", err, d.After)
	}
	if !strings.HasPrefix(string(d.After), "a = 1\n") || !strings.Contains(string(d.After), "[tui]\nx = 1\n") {
		t.Fatalf("the person's lines must stay:\n%s", d.After)
	}
}

func TestCodexFileGoneAtDisconnect(t *testing.T) {
	d, err := UnplanBlocks(&FileRecord{Path: "/x"}, nil, false)
	if err != nil || d.After != nil || d.Delete {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestCodexConfigPath(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := CodexConfigPath(get(nil), "/home/a"); got != filepath.FromSlash("/home/a/.codex/config.toml") {
		t.Fatal(got)
	}
	if got := CodexConfigPath(get(map[string]string{"CODEX_HOME": "/cx"}), "/home/a"); got != filepath.FromSlash("/cx/config.toml") {
		t.Fatal(got)
	}
}

// Any file that parses must survive connect then disconnect byte for byte (spec 14 item 2).
func FuzzRoundTripTOML(f *testing.F) {
	for _, c := range loadGoldenFuzz("codex") {
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		var m map[string]any
		if _, err := toml.Decode(strings.TrimPrefix(string(src), bom), &m); err != nil {
			t.Skip()
		}
		if strings.Contains(string(src), "# >>> ambit") || strings.Contains(string(src), "# <<< ambit") {
			t.Skip() // the markers claim the lines between them as Ambit's
		}
		for _, force := range []bool{false, true} { // with --force the conflicting lines are replaced and must come back
			in := codexInput(goldenStep{URL: "https://llm.example.com", Model: "m", Force: force}, src, true, nil)
			p, err := PlanCodex(in)
			if err != nil || p.After == nil {
				continue // refused: nothing was written
			}
			d, err := UnplanBlocks(&p.Record, p.After, true)
			if err != nil || d.Delete || string(d.After) != string(src) {
				t.Fatalf("round trip differs (force=%v)\n src  %q\n mid  %q\n back %q (%v)", force, src, p.After, d.After, err)
			}
		}
	})
}

// loadGoldenFuzz is the seed corpus: the `before` file of every golden case of the tool.
func loadGoldenFuzz(tool string) [][]byte {
	files, _ := filepath.Glob(filepath.Join("testdata", tool, "*", "before"))
	var out [][]byte
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			out = append(out, b)
		}
	}
	return out
}
