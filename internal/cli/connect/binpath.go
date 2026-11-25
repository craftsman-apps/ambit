package connect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// BinEnv is what Path needs to know about the machine; tests replace it.
type BinEnv struct {
	Executable func() (string, error)
	LookPath   func(string) (string, error)
	TempDir    string
	GOOS       string
}

func DefaultBinEnv() BinEnv {
	return BinEnv{Executable: os.Executable, LookPath: exec.LookPath, TempDir: os.TempDir(), GOOS: runtime.GOOS}
}

// Path is the absolute path of ambit that tool configs hold. Tools may start without the person's PATH (an app
// started from the Dock or the Start menu), so the configs cannot say just "ambit".
//
// When PATH finds the file that is running, that PATH entry is used, not the resolved file: for Homebrew it is
// /opt/homebrew/bin/ambit, which survives `brew upgrade`, where the resolved Cellar path would not. Otherwise the
// running binary is used. A copy in an npx cache or in the temporary directory is refused: it is gone later.
func (e BinEnv) Path() (string, error) {
	exe, err := e.Executable()
	if err != nil {
		return "", err
	}
	path := exe
	if found, err := e.LookPath("ambit"); err == nil {
		if abs, err := filepath.Abs(found); err == nil && sameFile(abs, exe) {
			path = abs
		}
	}
	if strings.Contains(path, "_npx") || e.TempDir != "" && isUnder(e.TempDir, path) {
		what := "ambit is running from a temporary copy"
		if strings.Contains(path, "_npx") {
			what = "ambit is running from a temporary npx copy"
		}
		return "", &out.Error{What: what, Why: "tools could not find it later",
			Next: "npm install -g @craftsman-apps/ambit (or brew install craftsman-apps/tap/ambit), then run the command again", Code: out.ExitFailed}
	}
	return path, nil
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func isUnder(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// Quote makes s one word for the shell a tool uses to run a command: POSIX single quotes, or double quotes on
// Windows, where paths hold spaces (C:\Users\First Last\). On Windows a string with a double quote, $ or a
// backtick (a URL can hold them; a path cannot hold a double quote) gets single quotes, in which PowerShell and
// Git Bash read nothing; a single quote inside is doubled.
func Quote(goos, s string) string {
	if goos == "windows" {
		if strings.ContainsAny(s, "\"$`“”„") { // PowerShell reads “ ” „ as double quotes
			return psQuote(s)
		}
		return `"` + s + `"`
	}
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// QuoteCmd quotes for tools that run the command with cmd.exe (Hermes Agent) or with Git Bash, else cmd.exe (pi).
// On Windows that is always double quotes: cmd.exe does not read single quotes. Double quotes are only safe for
// both shells when the text has none of the characters CheckCmdSafe refuses.
func QuoteCmd(goos, s string) string {
	if goos == "windows" {
		return `"` + s + `"`
	}
	return Quote(goos, s)
}

// CheckCmdSafe refuses, on Windows, a path or address that no quoting makes right for both cmd.exe and Git Bash:
// cmd.exe reads %, Git Bash reads $ and a backtick inside double quotes, and a double quote cannot be escaped.
func CheckCmdSafe(goos, tool string, vals ...string) error {
	if goos != "windows" {
		return nil
	}
	for i, v := range vals {
		if strings.ContainsAny(v, "\"$`%") {
			what, next := "the path of ambit", "install ambit in a folder whose path has none of them (npm install -g @craftsman-apps/ambit), then run the command again"
			if i > 0 {
				what, next = "the address", "use an address without them, then run the command again"
			}
			return &out.Error{What: tool + " cannot run a command with " + what, Why: fmt.Sprintf("%q has a %% $ ` or \" character, which cmd.exe and Git Bash read differently", v),
				Next: next, Code: out.ExitFailed}
		}
	}
	return nil
}
