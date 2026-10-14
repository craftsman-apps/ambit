package connect

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

const testKey = "ambit_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func TestFileStoreRoundTripAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	s := FileStore{Dir: dir}
	if _, err := s.Get("https://llm.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Set: %v, want ErrNotFound", err)
	}
	if err := s.Set("https://llm.example.com", testKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("https://llm.example.com", testKey+"2"); err != nil { // replace
		t.Fatal(err)
	}
	got, err := s.Get("https://llm.example.com")
	if err != nil || got != testKey+"2" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if other, _ := s.Get("https://other.example.com"); other != "" {
		t.Fatalf("a different URL must not share a file, got %q", other)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{dir: 0o700, s.Path("https://llm.example.com"): 0o600} {
			fi, err := os.Stat(p)
			if err != nil || fi.Mode().Perm() != want {
				t.Fatalf("%s mode = %v, %v; want %v", p, fi.Mode().Perm(), err, want)
			}
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	if err := s.Delete("https://llm.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("https://llm.example.com"); err != nil { // deleting twice is fine
		t.Fatal(err)
	}
	if _, err := s.Get("https://llm.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

func TestFileStorePathIsHashOfURL(t *testing.T) {
	p := FileStore{Dir: "/d"}.Path("https://llm.example.com")
	// sha256("https://llm.example.com")[:16 hex]; the URL itself must not appear in a file name.
	if filepath.Dir(p) != filepath.FromSlash("/d") || len(filepath.Base(p)) != 16 || strings.Contains(p, "example") {
		t.Fatalf("path = %s", p)
	}
}

func TestKeychainRoundTripAndNotFound(t *testing.T) {
	keyring.MockInit()
	k := NewKeychain("ambit-test")
	if _, err := k.Get("https://llm.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Set: %v", err)
	}
	if err := k.Set("https://llm.example.com", testKey); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get("https://llm.example.com"); err != nil || got != testKey {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := k.Delete("https://llm.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("https://llm.example.com"); err != nil { // already gone is not an error
		t.Fatal(err)
	}
}

func TestKeychainTimesOutInsteadOfHanging(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	k := NewKeychain("ambit-test")
	k.Timeout = 50 * time.Millisecond
	k.get = func(string, string) (string, error) { <-block; return "", nil }
	start := time.Now()
	_, err := k.Get("https://llm.example.com")
	if err == nil || !strings.Contains(err.Error(), "did not answer") || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestPut(t *testing.T) {
	unavailable := errors.New(`The name org.freedesktop.secrets was not provided by any .service files`)
	locked := errors.New("failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'")
	tests := []struct {
		name, goos, mode string
		kcErr            error
		wantStore        string
		wantWarn, errHas string
	}{
		{"macOS keychain", "darwin", "keychain", nil, "keychain", "", ""},
		{"default mode is the keychain", "linux", "", nil, "keychain", "", ""},
		{"Linux without Secret Service falls back to a file", "linux", "keychain", unavailable, "file", "no keychain found", ""},
		{"Linux with a locked collection fails", "linux", "keychain", locked, "", "", "unlock your keychain"},
		{"macOS error is never a silent fallback", "darwin", "keychain", errors.New("User canceled the operation"), "", "", "unlock your keychain"},
		{"Windows error is never a silent fallback", "windows", "keychain", unavailable, "", "", "unlock your keychain"},
		{"forced file store", "darwin", "file", nil, "file", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.kcErr != nil {
				keyring.MockInitWithError(tt.kcErr)
			} else {
				keyring.MockInit()
			}
			fs := FileStore{Dir: filepath.Join(t.TempDir(), "keys")}
			which, warn, err := Put(NewKeychain("ambit-test"), fs, tt.goos, tt.mode, "https://llm.example.com", testKey)
			if tt.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.errHas)
				}
				if _, gerr := fs.Get("https://llm.example.com"); !errors.Is(gerr, ErrNotFound) {
					t.Fatalf("a failed keychain write must not create a file")
				}
				return
			}
			if err != nil || which != tt.wantStore || (tt.wantWarn == "") != (warn == "") || !strings.Contains(warn, tt.wantWarn) {
				t.Fatalf("Put = %q, %q, %v", which, warn, err)
			}
			if warn != "" && !strings.Contains(warn, fs.Path("https://llm.example.com")) {
				t.Fatalf("the warning must name the file: %q", warn)
			}
		})
	}
	if _, _, err := Put(NewKeychain("x"), FileStore{}, "linux", "vault", "u", testKey); err == nil {
		t.Fatal("an unknown --key-store value must fail")
	}
}
