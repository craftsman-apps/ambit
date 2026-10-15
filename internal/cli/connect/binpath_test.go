package connect

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

func touch(t *testing.T, p string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(exe, onPath, tmp string) BinEnv {
	return BinEnv{
		Executable: func() (string, error) { return exe, nil },
		LookPath: func(string) (string, error) {
			if onPath == "" {
				return "", errors.New("not found")
			}
			return onPath, nil
		},
		TempDir: tmp,
		GOOS:    runtime.GOOS,
	}
}

func TestBinaryPathKeepsAHomebrewStyleSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	root := t.TempDir()
	real := touch(t, filepath.Join(root, "Cellar", "ambit", "0.2.0", "bin", "ambit"))
	link := filepath.Join(root, "bin", "ambit")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(real, link)
	// The running binary is the Cellar file; PATH finds the link. The link survives `brew upgrade`.
	got, err := env(real, link, "/nowhere").Path()
	if err != nil || got != link {
		t.Fatalf("Path = %q, %v; want %q", got, err, link)
	}
}

func TestBinaryPathUsesTheRunningBinaryWhenPathHasAnotherAmbit(t *testing.T) {
	root := t.TempDir()
	mine := touch(t, filepath.Join(root, "mine", "ambit"))
	other := touch(t, filepath.Join(root, "other", "ambit"))
	for name, onPath := range map[string]string{"another file": other, "not on PATH": ""} {
		got, err := env(mine, onPath, "/nowhere").Path()
		if err != nil || got != mine {
			t.Errorf("%s: Path = %q, %v; want %q", name, got, err, mine)
		}
	}
}

func TestBinaryPathRefusesNpxAndTemporaryCopies(t *testing.T) {
	root := t.TempDir()
	npx := touch(t, filepath.Join(root, ".npm", "_npx", "abc123", "node_modules", "ambit", "bin", "ambit"))
	_, err := env(npx, "", "/nowhere").Path()
	var e *out.Error
	if !errors.As(err, &e) || e.Code != out.ExitFailed || !strings.Contains(e.What, "temporary npx copy") || !strings.Contains(e.Next, "npm install -g @craftsman-apps/ambit") {
		t.Fatalf("npx: err = %#v", err)
	}
	tmp := filepath.Join(root, "tmp")
	inTmp := touch(t, filepath.Join(tmp, "go-build123", "ambit"))
	if _, err := env(inTmp, "", tmp).Path(); !errors.As(err, &e) || !strings.Contains(e.What, "temporary") {
		t.Fatalf("temp dir: err = %#v", err)
	}
	// A binary in the person's home is fine even when the temp dir is a parent of nothing.
	ok := touch(t, filepath.Join(root, "home", "bin", "ambit"))
	if got, err := env(ok, "", tmp).Path(); err != nil || got != ok {
		t.Fatalf("Path = %q, %v", got, err)
	}
}

func TestQuote(t *testing.T) {
	for _, tt := range []struct{ goos, in, want string }{
		{"darwin", "/opt/homebrew/bin/ambit", `'/opt/homebrew/bin/ambit'`},
		{"linux", "/home/a b/bin/ambit", `'/home/a b/bin/ambit'`},
		{"linux", "/home/it's/ambit", `'/home/it'\''s/ambit'`},
		{"windows", `C:\Users\First Last\bin\ambit.exe`, `"C:\Users\First Last\bin\ambit.exe"`},
		{"windows", `C:\Users\O'Brien\ambit.exe`, `"C:\Users\O'Brien\ambit.exe"`},
		// A space, a quote and $ in one word: nothing in it may be read by the shell.
		{"linux", `https://h/a b'c"d$(x)`, `'https://h/a b'\''c"d$(x)'`},
		// PowerShell reads typographic quotes as quotes: ’ ends a single-quoted string, “ ” „ end a double-quoted one.
		{"windows", `C:\Users\O’Brien\ambit.exe`, `"C:\Users\O’Brien\ambit.exe"`},
		{"windows", `https://h/a“b’c`, `'https://h/a“b’’c'`},
		{"windows", `https://h/a„b`, `'https://h/a„b'`},
		{"windows", `https://h/a b'c"d$(x)` + "`e", `'https://h/a b''c"d$(x)` + "`e'"},
	} {
		if got := Quote(tt.goos, tt.in); got != tt.want {
			t.Errorf("Quote(%s, %q) = %s, want %s", tt.goos, tt.in, got, tt.want)
		}
	}
}
