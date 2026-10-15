package connect

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/out"
	"github.com/tailscale/hujson"
)

func opencodeInput(s goldenStep, cur []byte, exists bool, prev *FileRecord) Input {
	return Input{Path: "/home/a/.config/opencode/opencode.json", File: cur, Exists: exists, URL: s.URL, Model: s.Model, AmbitPath: ambitBin,
		GOOS: "darwin", Force: s.Force, Getenv: func(k string) string { return s.Env[k] }, Prev: prev}
}

func TestOpenCodeGolden(t *testing.T) {
	for _, c := range loadGolden(t, "opencode") {
		t.Run(filepath.Base(c.dir), func(t *testing.T) {
			cur, exists := c.before, c.exists
			var prev *FileRecord
			var last *Plan
			for _, s := range c.steps {
				p, err := PlanOpenCode(opencodeInput(s, cur, exists, prev))
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
			// The result is what opencode reads: JSON with comments, the provider and the model.
			var v struct {
				Model    string                    `json:"model"`
				Provider map[string]map[string]any `json:"provider"`
			}
			raw, err := hujson.Standardize([]byte(strings.TrimPrefix(string(last.After), bom))) // a copy: it blanks comments
			if err != nil || json.Unmarshal(raw, &v) != nil || v.Provider["ambit"]["npm"] != "@ai-sdk/openai-compatible" || !strings.HasPrefix(v.Model, "ambit/") {
				t.Fatalf("result does not read as intended: %v %+v", err, v)
			}
			d, err := UnplanJSON(*prev, cur, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(prev.RemovedSecrets) > 0 {
				c.check(t, "roundtrip", strp(string(d.After)))
				return
			}
			switch {
			case !c.exists:
				if !d.Delete {
					t.Fatalf("a file connect created must be deleted, got %q", d.After)
				}
			case d.Delete || string(d.After) != string(c.before):
				t.Fatalf("disconnect did not restore the file\n got  %q\n want %q", d.After, c.before)
			}
		})
	}
}

func TestOpenCodeProviderValue(t *testing.T) {
	p, err := PlanOpenCode(opencodeInput(goldenStep{URL: "https://llm.example.com", Model: "a/b:free"}, []byte("{}\n"), true, nil))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(p.After, &v); err != nil {
		t.Fatal(err)
	}
	want := `{"npm":"@ai-sdk/openai-compatible","name":"Ambit","options":{"baseURL":"https://llm.example.com/v1","apiKey":"{env:AMBIT_API_KEY}"},"models":{"a/b:free":{"name":"a/b:free"}}}`
	got, _ := json.Marshal(v["provider"].(map[string]any)["ambit"])
	var w, g any
	json.Unmarshal([]byte(want), &w)
	json.Unmarshal(got, &g)
	if !jsonEqual(want, string(got)) || v["model"] != "ambit/a/b:free" {
		t.Fatalf("provider = %s, model = %v", got, v["model"])
	}
}

// opencode v2 configs are refused with What, Why and next, and nothing is planned.
func TestOpenCodeRefusesV2(t *testing.T) {
	for name, file := range map[string]string{
		"providers":         `{"providers": {"x": {}}}`,
		"provider package":  `{"provider": {"x": {"package": "@ai-sdk/openai-compatible"}}}`,
		"provider settings": `{"provider": {"x": {"settings": {"baseURL": "u"}}}}` + "\n",
		"with comments":     "// c\n" + `{"providers": {}, }`,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := PlanOpenCode(opencodeInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte(file), true, nil))
			var e *out.Error
			if p != nil || !errors.As(err, &e) || e.Code != out.ExitFailed || !strings.Contains(e.What, "opencode v2 config format") || !strings.Contains(e.Why, "writes the v1 format") ||
				e.Next != "add the Ambit provider by hand (see docs/guides/codex-opencode-hermes.md), or wait for a newer ambit" {
				t.Fatalf("p = %v, err = %#v", p, err)
			}
		})
	}
}

func TestOpenCodeInvalidJSONChangesNothing(t *testing.T) {
	_, err := PlanOpenCode(opencodeInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte("{\n  \"a\": \n}\n"), true, nil))
	if err == nil || !strings.Contains(err.Error(), "is not valid JSON") || !strings.Contains(err.Error(), "line") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeConflictLinesAndRecordNeverHoldAValue(t *testing.T) {
	const secret = "sk-plain-text-secret"
	before := `{"model": "other/model", "provider": {"ambit": {"options": {"apiKey": "` + secret + `"}}}}` + "\n"
	p, err := PlanOpenCode(opencodeInput(goldenStep{URL: "https://llm.example.com", Model: "m"}, []byte(before), true, nil))
	if err != nil || len(p.Conflicts) != 2 || p.After != nil {
		t.Fatalf("%v %+v", err, p)
	}
	for _, c := range p.Conflicts {
		if strings.Contains(c.String(), secret) || strings.Contains(c.String(), "other/model") {
			t.Errorf("conflict line prints a value: %s", c)
		}
	}
	p, _ = PlanOpenCode(opencodeInput(goldenStep{URL: "https://llm.example.com", Model: "m", Force: true}, []byte(before), true, nil))
	rec, _ := json.Marshal(p.Record)
	if strings.Contains(string(rec), secret) || strings.Contains(string(p.After), secret) {
		t.Fatalf("the old value is only in the backup:\n%s\n%s", rec, p.After)
	}
	if len(p.Record.RemovedSecrets) != 1 || p.Record.RemovedSecrets[0] != "/provider/ambit" {
		t.Fatalf("RemovedSecrets = %v", p.Record.RemovedSecrets)
	}
	p.Record.Backup = "/b.ambit.bak"
	d, _ := UnplanJSON(p.Record, p.After, true)
	if len(d.Notes) != 1 || d.Notes[0] != "provider.ambit was removed at connect; its old value is in /b.ambit.bak" {
		t.Fatalf("notes = %q", d.Notes)
	}
}

func TestOpenCodePath(t *testing.T) {
	dir := t.TempDir()
	none := func(string) string { return "" }
	has := func(m map[string]bool) func(string) bool { return func(p string) bool { return m[p] } }
	j := filepath.Join(dir, ".config", "opencode", "opencode.json")
	jc := filepath.Join(dir, ".config", "opencode", "opencode.jsonc")
	if got, err := OpenCodePath(none, dir, has(nil)); err != nil || got != j {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := OpenCodePath(none, dir, has(map[string]bool{jc: true})); err != nil || got != jc {
		t.Fatalf("%q %v", got, err)
	}
	_, err := OpenCodePath(none, dir, has(map[string]bool{j: true, jc: true}))
	var e *out.Error
	if !errors.As(err, &e) || e.Next != "merge them into one file, then run the command again" || e.Code != out.ExitFailed {
		t.Fatalf("both files: %#v", err)
	}
	x := filepath.Join(dir, "xdg")
	got, err := OpenCodePath(func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return x
		}
		return ""
	}, dir, has(nil))
	if err != nil || got != filepath.Join(x, "opencode", "opencode.json") {
		t.Fatalf("%q %v", got, err)
	}
}

// ---- shell wrappers (spec 5.3)

func TestDetectShell(t *testing.T) {
	for _, tt := range []struct {
		goos, env string
		want      Shell
	}{
		{"darwin", "/bin/zsh", Zsh}, {"linux", "/usr/bin/bash", Bash}, {"linux", "/usr/bin/fish", Fish},
		{"linux", "/bin/tcsh", NoShell}, {"linux", "", NoShell}, {"windows", "", PowerShell}, {"windows", `C:\Program Files\Git\bin\bash.exe`, PowerShell},
	} {
		if got := DetectShell(tt.goos, tt.env); got != tt.want {
			t.Errorf("DetectShell(%s, %q) = %q, want %q", tt.goos, tt.env, got, tt.want)
		}
	}
	for _, ok := range []string{"bash", "zsh", "fish", "powershell", "none"} {
		if s, err := ParseShell(ok); err != nil || string(s) != ok {
			t.Errorf("ParseShell(%s) = %q, %v", ok, s, err)
		}
	}
	if _, err := ParseShell("tcsh"); err == nil {
		t.Error("an unknown shell must be refused")
	}
}

func TestShellFile(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	profile := func() (string, error) { return `C:\Users\First Last\OneDrive\Documents\PowerShell\profile.ps1`, nil }
	for _, tt := range []struct {
		sh   Shell
		goos string
		env  map[string]string
		want string
	}{
		{Zsh, "darwin", nil, "/h/.zshrc"},
		{Zsh, "darwin", map[string]string{"ZDOTDIR": "/z"}, "/z/.zshrc"},
		{Bash, "linux", nil, "/h/.bashrc"},
		{Bash, "darwin", nil, "/h/.bash_profile"},
		{Fish, "linux", nil, "/h/.config/fish/functions/opencode.fish"},
		{Fish, "linux", map[string]string{"XDG_CONFIG_HOME": "/x"}, "/x/fish/functions/opencode.fish"},
		{PowerShell, "windows", nil, `C:\Users\First Last\OneDrive\Documents\PowerShell\profile.ps1`},
	} {
		got, err := ShellFile(tt.sh, tt.goos, get(tt.env), "/h", profile)
		if err != nil || filepath.ToSlash(got) != filepath.ToSlash(tt.want) {
			t.Errorf("ShellFile(%s, %s) = %q, %v; want %s", tt.sh, tt.goos, got, err, tt.want)
		}
	}
	if _, err := ShellFile(PowerShell, "windows", get(nil), "/h", func() (string, error) { return "", errors.New("no pwsh") }); err == nil {
		t.Error("a missing PowerShell must be an error")
	}
}

func TestPowerShellProfileFromFakeShell(t *testing.T) {
	var asked []string
	run := func(name string, args ...string) ([]byte, error) {
		asked = append(asked, name)
		if name == "pwsh" {
			return nil, errors.New("not found")
		}
		return []byte(`C:\Users\First Last\Documents\WindowsPowerShell\profile.ps1` + "\r\n"), nil
	}
	got, err := powerShellProfile(run)
	if err != nil || got != `C:\Users\First Last\Documents\WindowsPowerShell\profile.ps1` || strings.Join(asked, ",") != "pwsh,powershell.exe" {
		t.Fatalf("%q %v %v", got, err, asked)
	}
}

func TestPowerShellPolicyFromFakeShell(t *testing.T) {
	var asked []string
	run := func(name string, args ...string) ([]byte, error) {
		asked = append(asked, name+" "+args[len(args)-1])
		if name == "pwsh" {
			return nil, errors.New("not found")
		}
		if args[len(args)-1] == "Get-ExecutionPolicy" {
			return []byte("Restricted\r\n"), nil
		}
		return []byte(`C:\Users\a\Documents\WindowsPowerShell\profile.ps1` + "\r\n"), nil
	}
	got, err := powerShellPolicy(run)
	if err != nil || got != "Restricted" || asked[len(asked)-1] != "powershell.exe Get-ExecutionPolicy" {
		t.Fatalf("%q %v %v", got, err, asked)
	}
	if _, err := powerShellPolicy(func(string, ...string) ([]byte, error) { return nil, errors.New("none") }); err == nil {
		t.Error("no PowerShell must be an error")
	}
	for p, want := range map[string]bool{"Restricted": true, "allsigned": true, "RemoteSigned": false, "Unrestricted": false, "Bypass": false, "": false} {
		if PolicyBlocksProfile(p) != want {
			t.Errorf("PolicyBlocksProfile(%q) != %v", p, want)
		}
	}
}

func TestWrapperLines(t *testing.T) {
	for _, tt := range []struct {
		sh, goos, want string
	}{
		{"zsh", "darwin", `opencode() { AMBIT_API_KEY="$('/opt/homebrew/bin/ambit' key print --url 'https://llm.example.com')" command opencode "$@"; }`},
		{"bash", "linux", `opencode() { AMBIT_API_KEY="$('/opt/homebrew/bin/ambit' key print --url 'https://llm.example.com')" command opencode "$@"; }`},
		{"fish", "linux", `function opencode; AMBIT_API_KEY=('/opt/homebrew/bin/ambit' key print --url 'https://llm.example.com') command opencode $argv; end`},
		{"powershell", "windows", `function opencode { $had = Test-Path Env:AMBIT_API_KEY; $old = $env:AMBIT_API_KEY; $env:AMBIT_API_KEY = (& 'C:\Users\First Last\ambit.exe' key print --url 'https://llm.example.com'); try { & (Get-Command opencode -CommandType Application)[0] @args } finally { if ($had) { $env:AMBIT_API_KEY = $old } else { Remove-Item Env:AMBIT_API_KEY -ErrorAction SilentlyContinue } } }`},
	} {
		path := "/opt/homebrew/bin/ambit"
		if tt.goos == "windows" {
			path = `C:\Users\First Last\ambit.exe`
		}
		got := WrapperLines(Shell(tt.sh), tt.goos, path, "https://llm.example.com")
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("%s: got %q\n want %q", tt.sh, got, tt.want)
		}
	}
	// A quote, a space and $ in the path or the URL stay inside the quotes.
	got := WrapperLines(Zsh, "linux", "/home/o'b rien/ambit", "https://h/a$(x)")[0]
	if !strings.Contains(got, `'/home/o'\''b rien/ambit' key print --url 'https://h/a$(x)'`) {
		t.Errorf("zsh quoting: %s", got)
	}
	got = WrapperLines(PowerShell, "windows", `C:\o'b\ambit.exe`, "https://h/a$(x)")[0]
	if !strings.Contains(got, `& 'C:\o''b\ambit.exe' key print --url 'https://h/a$(x)'`) {
		t.Errorf("powershell quoting: %s", got)
	}
}

func TestShellBlockRoundTrip(t *testing.T) {
	for name, before := range map[string]string{
		"missing": "", "empty-line": "\n", "export lines": "export PATH=\"$HOME/bin:$PATH\"\nalias ll='ls -l'\n",
		"no final newline": "alias ll='ls -l'", "CRLF": "$env:A = 1\r\nSet-Alias x y\r\n", "BOM": "\xef\xbb\xbfSet-Alias x y\r\n",
		"blank end": "a\n\n", "mixed": "a\r\nb\n",
	} {
		t.Run(name, func(t *testing.T) {
			in := Input{Path: "/h/.zshrc", File: []byte(before), Exists: name != "missing", URL: "https://llm.example.com", AmbitPath: ambitBin, GOOS: "darwin", Shell: string(Zsh)}
			p, err := PlanShell(in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(p.After), "# >>> ambit") || !strings.Contains(string(p.After), "# <<< ambit <<<") {
				t.Fatalf("no block: %q", p.After)
			}
			d, err := UnplanBlocks(&p.Record, p.After, true)
			if err != nil {
				t.Fatal(err)
			}
			if name == "missing" {
				if !d.Delete {
					t.Fatalf("a file connect made must be deleted: %q", d.After)
				}
				return
			}
			if d.Delete || string(d.After) != before {
				t.Fatalf("got %q, want %q", d.After, before)
			}
		})
	}
}

func TestShellBlockReconnectAndAlready(t *testing.T) {
	in := Input{Path: "/h/.zshrc", File: []byte("alias a=b\n"), Exists: true, URL: "https://old.example.com", AmbitPath: ambitBin, GOOS: "darwin", Shell: string(Zsh)}
	p1, _ := PlanShell(in)
	in2 := in
	in2.File, in2.Prev = p1.After, &p1.Record
	p2, err := PlanShell(in2) // same values: nothing to do
	if err != nil || !p2.Already {
		t.Fatalf("already: %v %+v", err, p2)
	}
	in2.URL = "https://new.example.com"
	p3, err := PlanShell(in2)
	if err != nil || p3.Already || strings.Contains(string(p3.After), "old.example.com") || strings.Count(string(p3.After), "# >>> ambit") != 1 {
		t.Fatalf("update: %v %s", err, p3.After)
	}
	d, _ := UnplanBlocks(&p3.Record, p3.After, true)
	if string(d.After) != "alias a=b\n" {
		t.Fatalf("after disconnect: %q", d.After)
	}
}

func TestShellBlockEditedAtDisconnectStays(t *testing.T) {
	in := Input{Path: "/h/.zshrc", File: []byte("alias a=b\n"), Exists: true, URL: "https://llm.example.com", AmbitPath: ambitBin, GOOS: "darwin", Shell: string(Zsh)}
	p, _ := PlanShell(in)
	edited := strings.Replace(string(p.After), "command opencode", "command opencode --pure", 1)
	d, err := UnplanBlocks(&p.Record, []byte(edited), true)
	if err != nil || len(d.Notes) != 1 || !strings.Contains(string(d.After), "--pure") {
		t.Fatalf("%v %q %s", err, d.Notes, d.After)
	}
}

// The wrapper does what the spec says: the key is in the environment of opencode only, not of the shell.
func TestWrapperRunsInRealShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells")
	}
	dir := t.TempDir()
	ambit := filepath.Join(dir, "my ambit") // a space in the path
	os.WriteFile(ambit, []byte("#!/bin/sh\n[ \"$1 $2 $3\" = \"key print --url\" ] || exit 9\necho \"key-for-$4\"\n"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\necho \"opencode got $AMBIT_API_KEY args=$*\"\n"), 0o755)
	url := "https://llm.example.com/a b$HOME"
	for _, sh := range []Shell{Bash, Zsh, Fish} {
		t.Run(string(sh), func(t *testing.T) {
			exe, err := exec.LookPath(string(sh))
			if err != nil {
				t.Skip(sh, "is not installed")
			}
			rc := filepath.Join(dir, string(sh)+".rc")
			os.WriteFile(rc, []byte(strings.Join(WrapperLines(sh, runtime.GOOS, ambit, url), "\n")+"\n"), 0o644)
			script := ". " + rc + "\nopencode run --x\necho \"shell has [$AMBIT_API_KEY]\"\n"
			if sh == Fish {
				script = "source " + rc + "\nopencode run --x\necho \"shell has [$AMBIT_API_KEY]\"\n"
			}
			cmd := exec.Command(exe, "-c", script)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir}
			b, err := cmd.CombinedOutput()
			want := "opencode got key-for-" + url + " args=run --x\nshell has []\n"
			if err != nil || string(b) != want {
				t.Fatalf("%v\n got  %q\n want %q", err, b, want)
			}
		})
	}
}

func TestOpenCodeSecondRemovedProviderIsRefused(t *testing.T) {
	// An earlier --force connect removed an old provider.ambit; the person has added another one by hand.
	prev := &FileRecord{Path: "/c/opencode.json", RemovedSecrets: []string{"/provider/ambit"}, Backup: "/c/opencode.json.ambit.bak"}
	in := Input{Path: "/c/opencode.json", File: []byte(`{"provider":{"ambit":{"options":{"apiKey":"K2"}}}}`), Exists: true,
		URL: "https://b.example.com", Model: "m", GOOS: "darwin", Force: true, Prev: prev}
	if _, err := PlanOpenCode(in); err == nil {
		t.Fatal("a second removed provider.ambit must be refused")
	}
}

func TestWrapperLinesPowerShellTypographicQuotes(t *testing.T) {
	got := WrapperLines(PowerShell, "windows", `C:\Users\O’Brien\ambit.exe`, "https://h/a‘b")[0]
	if !strings.Contains(got, `& 'C:\Users\O’’Brien\ambit.exe'`) || !strings.Contains(got, `'https://h/a‘‘b'`) {
		t.Fatalf("typographic single quotes are not doubled: %s", got)
	}
}

// The PowerShell function must give opencode the Ambit key and then put back the key the person had set.
func TestPowerShellWrapperRestoresAMBIT_API_KEY(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("pwsh is not on PATH (or this is Windows, where the fake programs below do not run)")
	}
	bin := t.TempDir()
	for name, body := range map[string]string{"ambit": "echo ambit_wrapped", "opencode": `echo "opencode sees $AMBIT_API_KEY"`} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := WrapperLines(PowerShell, "linux", filepath.Join(bin, "ambit"), "https://llm.example.com")[0]
	for name, tt := range map[string]struct{ before, want string }{
		"set before the call":     {"mine", "opencode sees ambit_wrapped|after=mine|had=True"},
		"not set before the call": {"", "opencode sees ambit_wrapped|after=|had=False"},
	} {
		t.Run(name, func(t *testing.T) {
			script := wrapper + `; $r = opencode; "$r|after=$env:AMBIT_API_KEY|had=$(Test-Path Env:AMBIT_API_KEY)"`
			cmd := exec.Command(pwsh, "-NoProfile", "-Command", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Env = withoutEnv(cmd.Env, "AMBIT_API_KEY")
			if tt.before != "" {
				cmd.Env = append(cmd.Env, "AMBIT_API_KEY="+tt.before)
			}
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, got)
			}
			if strings.TrimSpace(string(got)) != tt.want {
				t.Fatalf("got %q, want %q", strings.TrimSpace(string(got)), tt.want)
			}
		})
	}
}

func withoutEnv(env []string, name string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return out
}
