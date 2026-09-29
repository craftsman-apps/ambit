package console

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIssuer is a minimal OIDC provider standing in for Dex.
type fakeIssuer struct {
	srv        *httptest.Server
	key        *rsa.PrivateKey
	mu         sync.Mutex
	challenge  string // code_challenge from the last authorize request
	nonce      string
	connector  string
	email      string
	groups     []string
	badPKCE    bool // set when the token request's verifier did not match
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, connector: "local", email: BootstrapEmail}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		f.mu.Lock()
		defer f.mu.Unlock()
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			f.badPKCE = true
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		idt := f.sign(t, map[string]any{
			"iss": f.srv.URL, "sub": "user-1", "aud": "ambit-console",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": f.nonce,
			"email": f.email, "email_verified": true, "name": "Admin", "groups": f.groups,
			"federated_claims": map[string]string{"connector_id": f.connector, "user_id": "user-1"},
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := jws.CompactSerialize()
	return s
}

var testAuth *Auth // the Auth behind the most recent newTestServer

var testSPA = fstest.MapFS{
	"index.html":    {Data: []byte("<!doctype html><title>Ambit</title>")},
	"assets/app.js": {Data: []byte("console.log(1)")},
}

func newTestServer(t *testing.T, f *fakeIssuer) (*httptest.Server, *http.Client) {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewUnstartedServer(nil)
	ts.Start()
	auth, err := NewAuth(t.Context(), AuthConfig{
		Issuer: f.srv.URL, ClientID: "ambit-console", ClientSecret: "s3cret",
		PublicURL: ts.URL, SessionKey: []byte("0123456789abcdef0123456789abcdef"), SessionTTL: 8 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	testAuth = auth
	ts.Config.Handler = NewServer(auth, testSPA, LoginOptions{Bootstrap: true}, nil)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, u string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// signIn runs /auth/login → (fake Dex) → /auth/callback and returns the callback response.
func signIn(t *testing.T, ts *httptest.Server, c *http.Client, f *fakeIssuer, tamperState bool) *http.Response {
	t.Helper()
	resp := get(t, c, ts.URL+"/auth/login?connector=local")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), f.srv.URL+"/auth") || q.Get("connector_id") != "local" ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("bad authorize redirect: %s", loc)
	}
	for _, s := range []string{"openid", "email", "profile", "groups", "federated:id"} {
		if !strings.Contains(q.Get("scope"), s) {
			t.Fatalf("scope %q missing: %s", s, q.Get("scope"))
		}
	}
	f.mu.Lock()
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	f.mu.Unlock()
	state := q.Get("state")
	if tamperState {
		state = "forged"
	}
	return get(t, c, ts.URL+"/auth/callback?code=abc&state="+url.QueryEscape(state))
}

func TestUnauthenticatedAccess(t *testing.T) {
	ts, c := newTestServer(t, newFakeIssuer(t))
	if r := get(t, c, ts.URL+"/"); r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/login" {
		t.Errorf("GET /: got %d %q, want 302 /login", r.StatusCode, r.Header.Get("Location"))
	}
	if r := get(t, c, ts.URL+"/servers/github"); r.StatusCode != http.StatusFound {
		t.Errorf("deep link: got %d, want 302", r.StatusCode)
	}
	if r := get(t, c, ts.URL+"/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/me: got %d, want 401", r.StatusCode)
	}
	if r := get(t, c, ts.URL+"/login"); r.StatusCode != http.StatusOK {
		t.Errorf("GET /login: got %d, want 200", r.StatusCode)
	}
	if r := get(t, c, ts.URL+"/assets/app.js"); r.StatusCode != http.StatusOK {
		t.Errorf("assets: got %d, want 200", r.StatusCode)
	}
	if r := get(t, c, ts.URL+"/healthz"); r.StatusCode != http.StatusOK {
		t.Errorf("healthz: got %d", r.StatusCode)
	}
}

func TestSignInSetsSecureSessionAndMe(t *testing.T) {
	f := newFakeIssuer(t)
	ts, c := newTestServer(t, f)
	r := signIn(t, ts, c, f, false)
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/" {
		t.Fatalf("callback: got %d %q (badPKCE=%v)", r.StatusCode, r.Header.Get("Location"), f.badPKCE)
	}
	var sess *http.Cookie
	for _, ck := range r.Cookies() {
		if ck.Name == SessionCookie {
			sess = ck
		}
	}
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Fatalf("session cookie flags wrong: %+v", sess)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/me", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	if resp.StatusCode != 200 || me["email"] != BootstrapEmail || me["role"] != RoleAdmin || me["bootstrap"] != true {
		t.Fatalf("/api/me: %d %v", resp.StatusCode, me)
	}
	if r := get(t, c, ts.URL+"/"); r.StatusCode != http.StatusOK {
		t.Errorf("GET / after sign-in: got %d", r.StatusCode)
	}
}

func TestCallbackRejectsForgedState(t *testing.T) {
	f := newFakeIssuer(t)
	ts, c := newTestServer(t, f)
	r := signIn(t, ts, c, f, true)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", r.StatusCode)
	}
	for _, ck := range r.Cookies() {
		if ck.Name == SessionCookie && ck.Value != "" {
			t.Fatal("session cookie set on forged state")
		}
	}
}

func TestCallbackWithoutLoginTransaction(t *testing.T) {
	ts, c := newTestServer(t, newFakeIssuer(t))
	if r := get(t, c, ts.URL+"/auth/callback?code=abc&state=x"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", r.StatusCode)
	}
}

func TestSSOUserWithoutGroupsIsViewer(t *testing.T) {
	f := newFakeIssuer(t)
	f.connector, f.email, f.groups = "okta", "alice@acme.com", []string{"eng"}
	ts, c := newTestServer(t, f)
	signIn(t, ts, c, f, false)
	req, _ := http.NewRequest("GET", ts.URL+"/api/me", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	if me["role"] != RoleViewer || me["bootstrap"] != false {
		t.Fatalf("got %v", me)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	f := newFakeIssuer(t)
	ts, c := newTestServer(t, f)
	signIn(t, ts, c, f, false)
	if r := get(t, c, ts.URL+"/auth/logout"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET logout: got %d, want 405", r.StatusCode)
	}
	resp, err := c.Post(ts.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout: got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if r := get(t, c, ts.URL+"/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: got %d, want 401", r.StatusCode)
	}
	if r := get(t, c, ts.URL+"/"); r.StatusCode != http.StatusFound {
		t.Errorf("after logout GET /: got %d, want 302", r.StatusCode)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	f := newFakeIssuer(t)
	ts, c := newTestServer(t, f)
	codec := Codec{Key: []byte("0123456789abcdef0123456789abcdef")}
	old := codec.Encode(Session{Email: BootstrapEmail, Role: RoleAdmin, Exp: time.Now().Add(-time.Minute).Unix()})
	u, _ := url.Parse(ts.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: SessionCookie, Value: old, Path: "/"}})
	if r := get(t, c, ts.URL+"/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", r.StatusCode)
	}
}

func TestLoginOptionsArePublic(t *testing.T) {
	f := newFakeIssuer(t)
	ts := httptest.NewServer(nil)
	defer ts.Close()
	auth, err := NewAuth(t.Context(), AuthConfig{Issuer: f.srv.URL, ClientID: "ambit-console", ClientSecret: "s",
		PublicURL: ts.URL, SessionKey: []byte("0123456789abcdef0123456789abcdef"), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	opts := LoginOptions{Bootstrap: false, SSO: []Connector{{ID: "keycloak", Name: "Acme (Keycloak)"}}}
	ts.Config.Handler = NewServer(auth, testSPA, opts, nil)
	resp, err := http.Get(ts.URL + "/api/login-options")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got LoginOptions
	json.NewDecoder(resp.Body).Decode(&got)
	if resp.StatusCode != 200 || got.Bootstrap || len(got.SSO) != 1 || got.SSO[0].ID != "keycloak" {
		t.Fatalf("got %d %+v", resp.StatusCode, got)
	}
}

func TestParseConnectors(t *testing.T) {
	got, err := ParseConnectors(`[{"id":"keycloak","name":"Acme"},{"id":"okta","name":"Okta"}]`)
	if err != nil || len(got) != 2 || got[1].ID != "okta" {
		t.Fatalf("got %+v %v", got, err)
	}
	if got, err := ParseConnectors(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: got %+v %v", got, err)
	}
	if _, err := ParseConnectors(`[{"id":""}]`); err == nil {
		t.Fatal("connector without id accepted")
	}
	if _, err := ParseConnectors(`{not json`); err == nil {
		t.Fatal("bad JSON accepted")
	}
}
