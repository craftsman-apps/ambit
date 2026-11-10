package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (f *keysFixture) revokeSelf(t *testing.T, bearer string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/keys/self", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestAKeyRevokesItselfAndOnlyItself(t *testing.T) {
	f := newKeysFixture(t)
	ck := f.cookie("user09@acme.com", RoleViewer)
	a, b := f.create(t, ck, "both", 90), f.create(t, ck, "llm", 90)
	r := f.revokeSelf(t, a.Key, nil)
	if r.Code != http.StatusNoContent || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(r.Code, r.Body.String())
	}
	keys, _ := f.keys.List(context.Background(), "user09@acme.com")
	if len(keys) != 1 || keys[0].ID != b.View["id"] {
		t.Fatalf("keys left: %+v", keys)
	}
	ev, _ := f.keys.Events(context.Background())
	if len(ev) == 0 || ev[0].Action != "revoke-self" || ev[0].KeyID != a.View["id"] || ev[0].Actor != "user09@acme.com" {
		t.Fatalf("History: %+v", ev)
	}
	if r := f.revokeSelf(t, a.Key, nil); r.Code != http.StatusUnauthorized {
		t.Fatal("a revoked key revoked again:", r.Code)
	}
	if strings.Contains(r.Body.String(), a.Key) {
		t.Fatal("the answer holds the key")
	}
}

func TestRevokeSelfRefusals(t *testing.T) {
	f := newKeysFixture(t)
	ck := f.cookie("user09@acme.com", RoleAdmin)
	k := f.create(t, ck, "both", 90)
	unknown := f.revokeSelf(t, "ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg", nil)
	malformed := f.revokeSelf(t, "sk-not-a-key", nil)
	session := f.revokeSelf(t, "", ck) // a signed-in admin without the key
	for name, r := range map[string]*httptest.ResponseRecorder{"unknown": unknown, "malformed": malformed, "session only": session} {
		if r.Code != http.StatusUnauthorized || r.Body.String() != unknown.Body.String() {
			t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
		}
	}
	req := httptest.NewRequest("DELETE", "/api/keys/self", nil)
	req.Header.Set("Authorization", "Basic "+k.Key) // not a bearer token
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Error("Basic accepted:", rec.Code)
	}
	if keys, _ := f.keys.List(context.Background(), "user09@acme.com"); len(keys) != 1 {
		t.Fatalf("a refused request deleted a key: %+v", keys)
	}
	// A browser never gets permission for a cross-site request with Authorization: no CORS headers.
	pre := httptest.NewRequest("OPTIONS", "/api/keys/self", nil)
	pre.Header.Set("Origin", "https://evil.example.com")
	pre.Header.Set("Access-Control-Request-Method", "DELETE")
	pre.Header.Set("Access-Control-Request-Headers", "authorization")
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, pre)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Headers") != "" {
		t.Fatalf("preflight allowed: %v", rec.Header())
	}
}

func TestRevokeSelfIsRateLimited(t *testing.T) {
	f := newKeysFixture(t)
	codes := map[int]int{}
	var last *httptest.ResponseRecorder
	for range 12 {
		last = f.revokeSelf(t, "ambit_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg", nil)
		codes[last.Code]++
	}
	if codes[http.StatusUnauthorized] != 10 || codes[http.StatusTooManyRequests] != 2 || last.Header().Get("Retry-After") != "60" {
		t.Fatalf("codes %v", codes)
	}
}

func TestRevokeSelfNotLeader(t *testing.T) {
	f := newKeysFixture(t)
	k := f.create(t, f.cookie("user09@acme.com", RoleViewer), "both", 90)
	f.keys.EndTerm()
	if r := f.revokeSelf(t, k.Key, nil); r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") == "" {
		t.Fatal(r.Code, r.Header())
	}
}
