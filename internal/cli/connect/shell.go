package connect

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// opencode has no setting that runs a command to read the key. So connect adds a function to the person's shell
// that sets AMBIT_API_KEY for the opencode process only (spec 5.3). The function sits in a marked block.

type Shell string

const (
	Bash       Shell = "bash"
	Zsh        Shell = "zsh"
	Fish       Shell = "fish"
	PowerShell Shell = "powershell"
	NoShell    Shell = "none" // print the function, write nothing
)

// ParseShell reads the value of --shell.
func ParseShell(s string) (Shell, error) {
	switch Shell(s) {
	case Bash, Zsh, Fish, PowerShell, NoShell:
		return Shell(s), nil
	}
	return "", fmt.Errorf("%q is not one of bash, zsh, fish, powershell, none", s)
}

// DetectShell is PowerShell on Windows, else the shell named by $SHELL. Any other shell is NoShell.
func DetectShell(goos, shellEnv string) Shell {
	if goos == "windows" {
		return PowerShell
	}
	switch Shell(filepath.Base(shellEnv)) {
	case Bash:
		return Bash
	case Zsh:
		return Zsh
	case Fish:
		return Fish
	}
	return NoShell
}

// ShellFile is the file that gets the function. profile reads the PowerShell profile path.
func ShellFile(sh Shell, goos string, getenv func(string) string, home string, profile func() (string, error)) (string, error) {
	switch sh {
	case Zsh:
		dir := home
		if z := getenv("ZDOTDIR"); z != "" {
			dir = z
		}
		return filepath.Join(dir, ".zshrc"), nil
	case Bash:
		if goos == "darwin" { // Terminal starts login shells, which read .bash_profile
			return filepath.Join(home, ".bash_profile"), nil
		}
		return filepath.Join(home, ".bashrc"), nil
	case Fish:
		cfg := filepath.Join(home, ".config")
		if x := getenv("XDG_CONFIG_HOME"); x != "" {
			cfg = x
		}
		return filepath.Join(cfg, "fish", "functions", "opencode.fish"), nil
	case PowerShell:
		return profile()
	}
	return "", fmt.Errorf("no file for the shell %q", sh)
}

// PowerShellProfile asks PowerShell for $PROFILE.CurrentUserAllHosts: Documents can be redirected to OneDrive, so
// the path cannot be guessed.
func PowerShellProfile() (string, error) { return powerShellProfile(runCommand) }

func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func powerShellProfile(run func(name string, args ...string) ([]byte, error)) (string, error) {
	for _, name := range []string{"pwsh", "powershell.exe"} {
		if b, err := run(name, "-NoProfile", "-Command", "$PROFILE.CurrentUserAllHosts"); err == nil {
			if p := strings.TrimSpace(string(b)); p != "" {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("cannot ask PowerShell for its profile path")
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// WrapperLines are the lines of the function, quoted for the shell. Everything the shell could read in the path
// or in the address stays inside quotes.
func WrapperLines(sh Shell, goos, ambitPath, url string) []string {
	switch sh {
	case Fish:
		return []string{"function opencode; AMBIT_API_KEY=(" + Quote("linux", ambitPath) + " key print --url " + Quote("linux", url) + ") command opencode $argv; end"}
	case PowerShell:
		return []string{"function opencode { $env:AMBIT_API_KEY = (& " + psQuote(ambitPath) + " key print --url " + psQuote(url) + "); try { & (Get-Command opencode -CommandType Application)[0] @args } finally { Remove-Item Env:AMBIT_API_KEY -ErrorAction SilentlyContinue } }"}
	}
	return []string{`opencode() { AMBIT_API_KEY="$(` + Quote("linux", ambitPath) + " key print --url " + Quote("linux", url) + `)" command opencode "$@"; }`}
}

// PlanShell puts the function of in.Shell into in.Path as a marked block at the end. An earlier block of Ambit's is
// taken out first, so a new --url replaces the old function. A block the person edited is an error.
func PlanShell(in Input) (*Plan, error) {
	t := ParseText(in.File)
	if _, err := t.Blocks(); err != nil {
		return nil, fmt.Errorf("%s: %w", in.Path, err)
	}
	if notes, _ := removeBlocks(t, in.Prev); len(notes) > 0 {
		return nil, fmt.Errorf("the Ambit block in %s was edited after connect — remove the lines between its markers by hand, then run the command again", in.Path)
	}
	body := WrapperLines(Shell(in.Shell), in.GOOS, in.AmbitPath, in.URL)
	rec := t.InsertBlockPadded(len(t.lines), blockBegin(OpenCode), body)
	p := &Plan{After: t.Bytes(), Record: FileRecord{Path: in.Path, Created: !in.Exists, Blocks: []BlockRecord{rec}, Wrote: []string{strings.Join(body, "\n")}}}
	if in.Prev != nil && in.Exists {
		p.Record.Created, p.Record.Dirs = in.Prev.Created, in.Prev.Dirs
	}
	if in.Exists && bytes.Equal(p.After, in.File) {
		p.Already = true
		if in.Prev != nil {
			p.Record = *in.Prev
		}
	}
	return p, nil
}
