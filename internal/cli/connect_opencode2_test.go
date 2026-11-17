package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

const otherAmbitKey = "ambit_ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvutsrqponmlkj"

func (r *connectRig) openCodeKeyPath() string {
	return filepath.Join(r.home, ".config", "ambit", "opencode-key")
}

// opencode 2 sends model calls from a background service that does not see the shell: connect writes the key to a
// user-only file, points the provider at it, adds no shell function, and says to restart the service. disconnect
// gives the config back and deletes the key file.
func TestConnectOpenCode2WritesTheKeyFileThenDisconnect(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.opencode = "opencode v2.2.5"
	oc := r.write(".config/opencode/opencode.json", seedOpenCode)
	zsh := r.write(".zshrc", seedZsh)
	kp := r.openCodeKeyPath()
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m/free", "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.stdout.String()
	conf := r.readFile(oc)
	ref, _ := json.Marshal("{file:" + kp + "}") // a Windows path has backslashes, which JSON escapes
	if !strings.Contains(conf, `"providers"`) || !strings.Contains(conf, `"apiKey": `+string(ref)) || strings.Contains(conf, "AMBIT_API_KEY") || strings.Contains(conf, testAmbitKey) {
		t.Fatalf("opencode.json:\n%s", conf)
	}
	if r.readFile(kp) != testAmbitKey || r.readFile(zsh) != seedZsh {
		t.Fatalf("key file %q, zshrc changed: %v", r.readFile(kp), r.readFile(zsh) != seedZsh)
	}
	if fi, _ := os.Stat(kp); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	for _, want := range []string{kp + ": holds the key, readable only by you (mode 0600)", "opencode service restart", "✓ test call:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testAmbitKey) || strings.Contains(out, "opencode() {") || strings.Contains(out, "install it later") {
		t.Fatalf("output shows the key or the shell function:\n%s", out)
	}
	// A second run changes nothing.
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m/free", "--yes"); code != 0 || !strings.Contains(r.stdout.String(), "opencode is already connected") {
		t.Fatalf("rerun: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if code := r.run("disconnect", "opencode", "--yes"); code != 0 {
		t.Fatalf("disconnect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(oc) != seedOpenCode || r.exists(kp) || len(r.kc) != 0 {
		t.Fatalf("after disconnect: opencode.json\n%s\nkey file %v, keys %v\n%s", r.readFile(oc), r.exists(kp), r.kc, r.stdout.String())
	}
}

// A file in the v2 format with another provider: Ambit's provider is added next to it, and disconnect restores it.
func TestConnectOpenCode2MergesIntoProviders(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.opencode = "2.2.5"
	seed := "{\n  \"providers\": {\n    \"anthropic\": { \"settings\": { \"apiKey\": \"{env:ANTHROPIC_API_KEY}\" } }\n  }\n}\n"
	oc := r.write(".config/opencode/opencode.json", seed)
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if c := r.readFile(oc); !strings.Contains(c, `"anthropic"`) || !strings.Contains(c, `"ambit":`) {
		t.Fatalf("opencode.json:\n%s", c)
	}
	if code := r.run("disconnect", "opencode", "--yes"); code != 0 || r.readFile(oc) != seed {
		t.Fatalf("disconnect: code %d\n%s", code, r.readFile(oc))
	}
}

// Connected for opencode 1 (shell function), then opencode is updated: connect again removes the function, writes
// the key file, and the state file no longer lists the profile.
func TestConnectOpenCode1ThenOpenCode2RemovesTheFunction(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	oc := r.write(".config/opencode/opencode.json", seedOpenCode)
	zsh := r.write(".zshrc", seedZsh)
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 || !strings.Contains(r.readFile(zsh), "opencode() {") {
		t.Fatalf("opencode 1: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	r.opencode = "opencode v2.2.5"
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--yes"); code != 0 {
		t.Fatalf("opencode 2: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(zsh) != seedZsh || r.readFile(r.openCodeKeyPath()) != testAmbitKey || strings.Contains(r.readFile(oc), "AMBIT_API_KEY") {
		t.Fatalf("zshrc:\n%s\nopencode.json:\n%s", r.readFile(zsh), r.readFile(oc))
	}
	if !strings.Contains(r.stdout.String(), "removed the opencode function from "+zsh) {
		t.Errorf("no line about the function:\n%s", r.stdout.String())
	}
	st, err := connect.LoadState(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Tools[connect.OpenCode].Files {
		if f.Path == zsh {
			t.Fatalf("the profile is still recorded: %+v", st.Tools[connect.OpenCode])
		}
	}
	if code := r.run("disconnect", "--all", "--yes"); code != 0 || r.readFile(oc) != seedOpenCode || r.readFile(zsh) != seedZsh || r.exists(r.openCodeKeyPath()) {
		t.Fatalf("disconnect: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
}

// opencode is not installed (or does not answer): connect sets up opencode 2 and says why. --shell is not used.
func TestConnectOpenCodeNotFoundSetsUpOpenCode2(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.opencode = ""
	zsh := r.write(".zshrc", seedZsh)
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes", "--shell", "zsh"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.stdout.String()
	for _, want := range []string{"ambit sets it up for opencode 2", "--shell is not used with opencode 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if r.readFile(zsh) != seedZsh || !r.exists(r.openCodeKeyPath()) {
		t.Fatalf("zshrc changed or no key file:\n%s", out)
	}
}

// ambit key set replaces the key in the opencode 2 key file too, so that the service reads the new key after a
// restart; disconnect still deletes the file. A key file changed by hand keeps its content.
func TestKeySetRenewsTheOpenCodeKeyFile(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.opencode = "2.2.5"
	kp := r.openCodeKeyPath()
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	r.env["AMBIT_API_KEY"] = otherAmbitKey
	if code := r.run("key", "set", "--url", g.URL); code != 0 {
		t.Fatalf("key set: code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.stdout.String()
	if r.readFile(kp) != otherAmbitKey || r.kc[g.URL] != otherAmbitKey || !strings.Contains(out, "wrote the new key to "+kp) || !strings.Contains(out, "opencode service restart") || strings.Contains(out, otherAmbitKey) {
		t.Fatalf("key file %q\n%s", r.readFile(kp), out)
	}
	// changed by hand: left alone, with a warning
	if err := os.WriteFile(kp, []byte("ambit_mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.env["AMBIT_API_KEY"] = testAmbitKey
	if code := r.run("key", "set", "--url", g.URL); code != 0 || r.readFile(kp) != "ambit_mine" || !strings.Contains(r.stdout.String(), kp+" was changed after connect") {
		t.Fatalf("changed file: code %d, %q\n%s", code, r.readFile(kp), r.stdout.String())
	}
	if err := os.WriteFile(kp, []byte(otherAmbitKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.run("disconnect", "opencode", "--yes"); code != 0 || r.exists(kp) {
		t.Fatalf("disconnect after key set: code %d, key file left: %v\n%s", code, r.exists(kp), r.stdout.String())
	}
}

// connect --replace-key is the other way to change the key: the key file gets the new key.
func TestConnectOpenCode2ReplaceKey(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	r.opencode = "2.2.5"
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	r.env["AMBIT_API_KEY"] = otherAmbitKey
	if code := r.run("connect", "opencode", "--url", g.URL, "--model", "m", "--replace-key", "--yes"); code != 0 || r.readFile(r.openCodeKeyPath()) != otherAmbitKey {
		t.Fatalf("code %d, key file %q\n%s%s", code, r.readFile(r.openCodeKeyPath()), r.stdout.String(), r.stderr.String())
	}
}
