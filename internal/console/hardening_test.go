package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	ts, c := newTestServer(t, newFakeIssuer(t))
	for _, p := range []string{"/login", "/api/me", "/healthz", "/assets/app.js", "/"} {
		r := get(t, c, ts.URL+p)
		h := r.Header
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") && strings.Contains(strings.SplitN(csp, "script-src", 2)[1], "unsafe-inline") {
			t.Errorf("%s: inline scripts allowed", p)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s: missing frame/nosniff/referrer headers: %v", p, h)
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS must not be sent over plain http", p)
		}
	}
}

func TestLogoutInvalidatesACopiedCookie(t *testing.T) {
	f := newFakeIssuer(t)
	ts, c := newTestServer(t, f)
	signIn(t, ts, c, f, false)
	u, _ := url.Parse(ts.URL)
	var stolen *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == SessionCookie {
			stolen = ck
		}
	}
	if stolen == nil {
		t.Fatal("no session cookie")
	}
	resp, err := c.Post(ts.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/api/me", nil)
	req.AddCookie(stolen)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a cookie copied before logout still works: %d", r.StatusCode)
	}
}

func TestRevokeUserEndsEverySessionOfThatUser(t *testing.T) {
	f := newFakeIssuer(t)
	f.connector, f.email, f.groups = "okta", "priya@acme.com", []string{"ambit-admins"}
	ts, c := newTestServer(t, f)
	signIn(t, ts, c, f, false)
	if r := get(t, c, ts.URL+"/api/me"); r.StatusCode != 200 {
		t.Fatalf("not signed in: %d", r.StatusCode)
	}
	if n := testAuth.Sessions.RevokeUser("priya@acme.com"); n != 1 {
		t.Fatalf("revoked %d sessions, want 1", n)
	}
	if r := get(t, c, ts.URL+"/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked user still signed in: %d", r.StatusCode)
	}
}

func TestSessionStoreDropsExpired(t *testing.T) {
	s := NewSessionStore()
	s.Add("a", "x@acme.com", 1) // expired long ago
	s.Add("b", "x@acme.com", 1<<62)
	if s.Valid("a") || !s.Valid("b") || s.Len() != 1 {
		t.Fatalf("valid(a)=%v valid(b)=%v len=%d", s.Valid("a"), s.Valid("b"), s.Len())
	}
}

func TestUpdateRecordsWhoChangedIt(t *testing.T) {
	h, c, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	if r := do(t, h, "PUT", "/api/access-rules/everyone-gemma", admin, RuleInput{Groups: []string{"eng"}, Models: []string{"gemma-4-31b"}}); r.Code != 200 {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	var got v1.AccessRule
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everyone-gemma"}, &got)
	if got.Annotations["ambit.craftsmanapps.com/updated-by"] != "admin@acme.test" {
		t.Fatalf("updated-by not recorded: %v", got.Annotations)
	}
}

var _ = httptest.NewRecorder
