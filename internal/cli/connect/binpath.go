package connect

import (
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
		if strings.ContainsAny(s, "\"$`") {
			return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
		}
		return `"` + s + `"`
	}
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}
