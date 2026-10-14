package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

const testAmbitKey = "ambit_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

// memStore is an in-memory connect.Store for the flow tests.
type memStore map[string]string

func (m memStore) Get(u string) (string, error) {
	if k, ok := m[u]; ok {
		return k, nil
	}
	return "", connect.ErrNotFound
}
func (m memStore) Set(u, k string) error { m[u] = k; return nil }
func (m memStore) Delete(u string) error { delete(m, u); return nil }

type connectRig struct {
	t         *testing.T
	kc, file  memStore
	statePath string
	home      string
	env       map[string]string // the environment the commands see
	secret    string            // what the hidden prompt returns; "" means there is no terminal to ask in
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	tty       bool
	stdin     string
}

func newRig(t *testing.T) *connectRig {
	home := t.TempDir()
	return &connectRig{t: t, kc: memStore{}, file: memStore{}, statePath: filepath.Join(home, "state", "connect.json"), home: home,
		env: map[string]string{"SHELL": "/bin/zsh"}}
}

const fakeAmbit = "/usr/local/bin/ambit-under-test"

func (r *connectRig) run(args ...string) int {
	r.t.Helper()
	r.stdout.Reset()
	r.stderr.Reset()
	d := &connectDeps{Keychain: r.kc, File: r.file, StatePath: r.statePath, GOOS: runtime.GOOS, Home: r.home,
		Getenv:  func(k string) string { return r.env[k] },
		Bin:     connect.BinEnv{Executable: func() (string, error) { return fakeAmbit, nil }, LookPath: func(string) (string, error) { return "", errors.New("not on PATH") }, GOOS: runtime.GOOS},
		Profile: func() (string, error) { return "", errors.New("no PowerShell here") },
		HTTP:    func(string) *http.Client { return http.DefaultClient }, CallLimit: 300 * time.Millisecond, CallEvery: 20 * time.Millisecond}
	if r.secret != "" {
		d.ReadSecret = func(context.Context, string) (string, error) { return r.secret, nil }
	}
	a := &app{in: strings.NewReader(r.stdin), out: &r.stdout, errw: &r.stderr, tty: r.tty, testConnect: d}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(&r.stdout)
	root.SetErr(&r.stderr)
	return a.exit(root.Execute())
}

// state writes a state file that says where the key of url is.
func (r *connectRig) state(url, store string) {
	r.t.Helper()
	st := connect.NewState()
	st.SetKey(url, connect.KeyRecord{Store: store})
	if err := st.Save(r.statePath); err != nil {
		r.t.Fatal(err)
	}
}

func TestKeyPrintPrintsOnlyTheKey(t *testing.T) {
	r := newRig(t)
	r.kc["https://llm.example.com"] = testAmbitKey
	r.state("https://llm.example.com", connect.StoreKeychain)
	// The address is normalised, so a trailing /v1 and capitals find the same key.
	for _, u := range []string{"https://llm.example.com", "HTTPS://LLM.example.com/v1/"} {
		if code := r.run("key", "print", "--url", u); code != 0 || r.stdout.String() != testAmbitKey+"\n" || r.stderr.String() != "" {
			t.Fatalf("%s: code %d, stdout %q, stderr %q", u, code, r.stdout.String(), r.stderr.String())
		}
	}
}

func TestKeyPrintReadsTheFileStoreWhenTheStateSaysSo(t *testing.T) {
	r := newRig(t)
	r.file["https://llm.example.com"] = testAmbitKey
	r.kc["https://llm.example.com"] = "ambit_wrong" // one place only: the state file decides
	r.state("https://llm.example.com", connect.StoreFile)
	if code := r.run("key", "print", "--url", "https://llm.example.com"); code != 0 || r.stdout.String() != testAmbitKey+"\n" {
		t.Fatalf("code %d, stdout %q", code, r.stdout.String())
	}
}

func TestKeyPrintNothingStored(t *testing.T) {
	r := newRig(t)
	code := r.run("key", "print", "--url", "https://llm.example.com")
	if code != 1 || r.stdout.String() != "" {
		t.Fatalf("code %d, stdout %q", code, r.stdout.String())
	}
	for _, want := range []string{"✗ no Ambit key stored for https://llm.example.com", "next: ambit connect <tool> --url https://llm.example.com"} {
		if !strings.Contains(r.stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", r.stderr.String(), want)
		}
	}
	// The state file names the store, but the store lost the key.
	r.state("https://llm.example.com", connect.StoreKeychain)
	if code := r.run("key", "print", "--url", "https://llm.example.com"); code != 1 || !strings.Contains(r.stderr.String(), "no Ambit key stored") {
		t.Fatalf("code %d, stderr %q", code, r.stderr.String())
	}
}

func TestKeyPrintRefusesOnATerminalWithoutReveal(t *testing.T) {
	r := newRig(t)
	r.kc["https://llm.example.com"] = testAmbitKey
	r.state("https://llm.example.com", connect.StoreKeychain)
	r.tty = true
	code := r.run("key", "print", "--url", "https://llm.example.com")
	if code != 2 || strings.Contains(r.stdout.String()+r.stderr.String(), testAmbitKey) || !strings.Contains(r.stderr.String(), "--reveal") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, r.stdout.String(), r.stderr.String())
	}
	if code := r.run("key", "print", "--url", "https://llm.example.com", "--reveal"); code != 0 || r.stdout.String() != testAmbitKey+"\n" {
		t.Fatalf("--reveal: code %d, stdout %q", code, r.stdout.String())
	}
}

func TestKeyPrintUsageErrors(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{{"key", "print"}, {"key", "print", "--url", "llm.example.com"}, {"key", "print", "--url", "https://x", "--output", "json"}} {
		if code := r.run(args...); code != 2 {
			t.Errorf("%v: code %d, want 2; stderr %q", args, code, r.stderr.String())
		}
	}
}

func TestKeyPrintIsFast(t *testing.T) {
	r := newRig(t)
	r.kc["https://llm.example.com"] = testAmbitKey
	r.state("https://llm.example.com", connect.StoreKeychain)
	start := time.Now()
	r.run("key", "print", "--url", "https://llm.example.com")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("key print took %v; tools run it at every start", d)
	}
}
