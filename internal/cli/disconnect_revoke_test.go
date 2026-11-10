package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
)

// fakeConsole answers DELETE /api/keys/self with status and records the bearer tokens it got.
type fakeConsole struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	bearers []string
}

func newFakeConsole(t *testing.T, status int) *fakeConsole {
	c := &fakeConsole{status: status}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Method != http.MethodDelete || r.URL.Path != "/api/keys/self" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		c.bearers = append(c.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.Close)
	return c
}

func TestDisconnectRevokeRevokesThenDeletes(t *testing.T) {
	g, c, r := newGateway(t), newFakeConsole(t, http.StatusNoContent), newRig(t)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--console-url", c.URL+"/", "--yes")
	if code := r.run("disconnect", "claude-code", "--revoke", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	out := r.stdout.String() + r.stderr.String()
	if len(c.bearers) != 1 || c.bearers[0] != testAmbitKey || len(r.kc) != 0 {
		t.Fatalf("bearers %d, keys %v", len(c.bearers), r.kc)
	}
	if !strings.Contains(out, "key for "+g.URL+" revoked") || strings.Contains(out, "next: revoke") || strings.Contains(out, testAmbitKey) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDisconnectRevokeOfAnUnknownKeyStillDeletesIt(t *testing.T) {
	g, c, r := newGateway(t), newFakeConsole(t, http.StatusUnauthorized), newRig(t)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	if code := r.run("disconnect", "claude-code", "--revoke", "--console-url", c.URL, "--yes"); code != 0 || len(r.kc) != 0 || !strings.Contains(r.stdout.String(), "does not know the key") {
		t.Fatalf("code %d, keys %v\n%s%s", code, r.kc, r.stdout.String(), r.stderr.String())
	}
}

func TestDisconnectRevokeFailureKeepsTheKey(t *testing.T) {
	g, c, r := newGateway(t), newFakeConsole(t, http.StatusInternalServerError), newRig(t)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--console-url", c.URL, "--yes")
	if code := r.run("disconnect", "claude-code", "--revoke", "--yes"); code != 1 {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	st, _ := connect.LoadState(r.statePath)
	if r.kc[g.URL] != testAmbitKey || st.Keys[g.URL].Store == "" || len(st.Tools) != 0 {
		t.Fatalf("keys %v, state %+v", r.kc, st)
	}
	out := r.stdout.String() + r.stderr.String()
	if !strings.Contains(out, "ambit disconnect --all --revoke") || strings.Contains(out, testAmbitKey) {
		t.Fatalf("output:\n%s", out)
	}
	c.status = http.StatusNoContent
	if code := r.run("disconnect", "--all", "--revoke", "--yes"); code != 0 || len(r.kc) != 0 || len(c.bearers) != 2 {
		t.Fatalf("retry: code %d, keys %v\n%s%s", code, r.kc, r.stdout.String(), r.stderr.String())
	}
}

func TestDisconnectRevokeNeedsTheConsoleAndChangesNothingWithout(t *testing.T) {
	g, r := newGateway(t), newRig(t)
	settings := r.write(".claude/settings.json", seedClaude)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--yes")
	before := r.readFile(settings)
	if code := r.run("disconnect", "claude-code", "--revoke", "--yes"); code != 2 || !strings.Contains(r.stdout.String()+r.stderr.String(), "console address") {
		t.Fatalf("code %d\n%s%s", code, r.stdout.String(), r.stderr.String())
	}
	if r.readFile(settings) != before || len(r.kc) != 1 {
		t.Fatal("a refused --revoke changed something")
	}
}

func TestDisconnectRevokeOnlyKeysThatAreDeleted(t *testing.T) {
	g, c, r := newGateway(t), newFakeConsole(t, http.StatusNoContent), newRig(t)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--console-url", c.URL, "--yes")
	r.run("connect", "codex", "--url", g.URL, "--model", "m", "--yes")
	if code := r.run("disconnect", "codex", "--revoke", "--yes"); code != 0 || len(c.bearers) != 0 || len(r.kc) != 1 {
		t.Fatalf("a key that claude-code still uses was revoked: code %d, bearers %d\n%s", code, len(c.bearers), r.stdout.String())
	}
	if code := r.run("disconnect", "claude-code", "--revoke", "--dry-run"); code != 0 || len(c.bearers) != 0 || !strings.Contains(r.stdout.String(), "would revoke the key for "+g.URL+" at "+c.URL) {
		t.Fatalf("dry run: code %d, bearers %d\n%s", code, len(c.bearers), r.stdout.String())
	}
}

func TestDisconnectRevokeDoesNotFollowARedirect(t *testing.T) {
	g, other, r := newGateway(t), newFakeConsole(t, http.StatusNoContent), newRig(t)
	redirect := httptest.NewServer(http.RedirectHandler(other.URL+"/api/keys/self", http.StatusTemporaryRedirect))
	t.Cleanup(redirect.Close)
	r.run("connect", "claude-code", "--url", g.URL, "--model", "m", "--key-file", r.keyFile(), "--console-url", redirect.URL, "--yes")
	if code := r.run("disconnect", "claude-code", "--revoke", "--yes"); code != 1 || len(other.bearers) != 0 || len(r.kc) != 1 {
		t.Fatalf("code %d, the redirect target got %d keys, keys %v\n%s%s", code, len(other.bearers), r.kc, r.stdout.String(), r.stderr.String())
	}
}
