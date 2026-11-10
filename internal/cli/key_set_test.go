package cli

import (
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

const keySetURL = "https://llm.example.com"

func TestKeySetStoresTheKeyWithoutATool(t *testing.T) {
	for name, setup := range map[string]func(r *connectRig) []string{
		"key file":    func(r *connectRig) []string { return []string{"--key-file", r.keyFile()} },
		"environment": func(r *connectRig) []string { r.env["AMBIT_API_KEY"] = testAmbitKey; return nil },
		"prompt":      func(r *connectRig) []string { r.secret = testAmbitKey; return nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			args := append([]string{"key", "set", "--url", keySetURL + "/v1/", "--console-url", "https://console.example.com/"}, setup(r)...)
			if code := r.run(args...); code != 0 {
				t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
			}
			if r.kc[keySetURL] != testAmbitKey {
				t.Fatalf("keychain: %v", r.kc)
			}
			st, err := connect.LoadState(r.statePath)
			if err != nil {
				t.Fatal(err)
			}
			if kr := st.Keys[keySetURL]; kr.Store != connect.StoreKeychain || kr.ConsoleURL != "https://console.example.com" || len(st.Tools) != 0 {
				t.Fatalf("state: %+v", st)
			}
			out := r.stdout.String() + r.stderr.String()
			if !strings.Contains(out, "key for "+keySetURL+" stored in") || strings.Contains(out, testAmbitKey) {
				t.Fatalf("output:\n%s", out)
			}
			if code := r.run("key", "print", "--url", keySetURL); code != 0 || r.stdout.String() != testAmbitKey+"\n" {
				t.Fatalf("key print: code %d %q", code, r.stdout.String())
			}
		})
	}
}

func TestKeySetReplacesTheKeyAndDeletesItFromTheOtherStore(t *testing.T) {
	r := newRig(t)
	r.env["AMBIT_API_KEY"] = testAmbitKey
	if code := r.run("key", "set", "--url", keySetURL); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	const other = "ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg"
	r.env["AMBIT_API_KEY"] = other
	if code := r.run("key", "set", "--url", keySetURL, "--key-store", "file"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.file[keySetURL] != other || len(r.kc) != 0 {
		t.Fatalf("file %v, keychain %v", r.file, r.kc)
	}
	st, _ := connect.LoadState(r.statePath)
	if st.Keys[keySetURL].Store != connect.StoreFile {
		t.Fatalf("state: %+v", st.Keys)
	}
}

func TestKeySetKeepsTheConsoleURLAndIsReusedByConnect(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.env["AMBIT_API_KEY"] = testAmbitKey
	r.run("key", "set", "--url", g.URL, "--console-url", "https://console.example.com")
	r.env["AMBIT_API_KEY"] = ""
	if code := r.run("key", "set", "--url", g.URL, "--key-file", r.keyFile()); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	st, _ := connect.LoadState(r.statePath)
	if st.Keys[g.URL].ConsoleURL != "https://console.example.com" {
		t.Fatalf("a second key set lost the console address: %+v", st.Keys)
	}
	if code := r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "key: reused") {
		t.Fatalf("connect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
}

func TestKeySetRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  string
		want string
	}{
		"no url":             {[]string{"key", "set"}, testAmbitKey, "--url"},
		"bad url":            {[]string{"key", "set", "--url", "llm.example.com"}, testAmbitKey, "must start with http"},
		"not an Ambit key":   {[]string{"key", "set", "--url", keySetURL}, "sk-or-v1-0123456789abcdef", "this is not an Ambit key"},
		"no key, no prompt":  {[]string{"key", "set", "--url", keySetURL}, "", "AMBIT_API_KEY"},
		"bad key store":      {[]string{"key", "set", "--url", keySetURL, "--key-store", "vault"}, testAmbitKey, "must be keychain or file"},
		"json output":        {[]string{"key", "set", "--url", keySetURL, "--output", "json"}, testAmbitKey, "json"},
		"positional address": {[]string{"key", "set", keySetURL}, testAmbitKey, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.env["AMBIT_API_KEY"] = tc.env
			code := r.run(tc.args...)
			out := r.stdout.String() + r.stderr.String()
			if code != 2 || !strings.Contains(out, tc.want) || strings.Contains(out, "0123456789abcdef") {
				t.Fatalf("code %d\n%s", code, out)
			}
			if len(r.kc)+len(r.file) != 0 || r.exists(r.statePath) {
				t.Fatalf("something was stored: %v %v", r.kc, r.file)
			}
		})
	}
}

func TestDisconnectAllDeletesAKeyFromKeySet(t *testing.T) {
	r := newRig(t)
	r.env["AMBIT_API_KEY"] = testAmbitKey
	r.run("key", "set", "--url", keySetURL)
	if code := r.run("disconnect", "--all", "--yes"); code != 0 || len(r.kc) != 0 {
		t.Fatalf("code %d, keychain %v\n%s%s", code, r.kc, r.stdout.String(), r.stderr.String())
	}
}
