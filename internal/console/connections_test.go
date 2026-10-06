package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeSaaS is an OAuth 2.0 provider such as Jira: it issues tokens per code, renews them, revokes them.
type fakeSaaS struct {
	srv            *httptest.Server
	mu             sync.Mutex
	refuseRenew    bool
	revoked        []string
	issued         int
	lastVerifier   string
	rotate         bool // renewal returns a new refresh token "rt2-<user>"
	noRefreshToken bool // the code exchange issues no refresh token
	renewed        int
}

func newFakeSaaS(t *testing.T) *fakeSaaS {
	f := &fakeSaaS{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if u, p, ok := r.BasicAuth(); !(ok && u == "ambit" && p == "conn-secret") && r.PostForm.Get("client_secret") != "conn-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			f.lastVerifier = r.PostForm.Get("code_verifier")
			user := r.PostForm.Get("code") // the test uses the user's name as the code
			f.issued++
			out := map[string]any{"access_token": "at-" + user, "refresh_token": "rt-" + user, "token_type": "Bearer", "expires_in": 3600}
			if f.noRefreshToken {
				delete(out, "refresh_token")
			}
			json.NewEncoder(w).Encode(out)
		case "refresh_token":
			f.renewed++
			if f.refuseRenew {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Session not active"})
				return
			}
			rt := r.PostForm.Get("refresh_token")
			user := strings.TrimPrefix(rt, "rt-")
			out := map[string]any{"access_token": "renewed-" + user, "token_type": "Bearer", "expires_in": 3600}
			if f.rotate {
				out["refresh_token"] = "rt2-" + user
			}
			json.NewEncoder(w).Encode(out)
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm.Get("token"))
		f.mu.Unlock()
	})
	// GitHub's format: DELETE /applications/{client_id}/grant, app credentials, the access token in JSON.
	mux.HandleFunc("DELETE /applications/{client}/grant", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccessToken string `json:"access_token"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if u, p, ok := r.BasicAuth(); !ok || u != "ambit" || p != "conn-secret" || r.PathValue("client") != "ambit" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.revoked = append(f.revoked, in.AccessToken)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func connFixture(t *testing.T, saas *fakeSaaS) (*Connections, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "jira", Namespace: testNS}, Spec: v1.ConnectionSpec{
			DisplayName: "Jira", AuthorizationURL: saas.srv.URL + "/authorize", TokenURL: saas.srv.URL + "/token",
			RevocationURL: saas.srv.URL + "/revoke", ClientID: "ambit", ClientSecretRef: v1.SecretRef{Name: "ambit-conn-jira"}, Scopes: []string{"read"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-conn-jira", Namespace: "ambit-grants"}, Data: map[string][]byte{"clientSecret": []byte("conn-secret")}},
	).Build()
	return &Connections{Client: c, Namespace: testNS, PublicURL: "https://console.acme.test", Codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")},
		Store: &GrantStore{Client: c, Namespace: "ambit-grants", Key: []byte("fedcba9876543210fedcba9876543210")}}, c
}

// connect walks the flow as a browser would: start -> provider (skipped: the code is the user name) -> callback.
func connect(t *testing.T, conns *Connections, a *Auth, email, code string) *http.Response {
	t.Helper()
	mux := http.NewServeMux()
	conns.register(mux, a)
	ck := user(a, email, RoleViewer)
	req := httptest.NewRequest("GET", "/connections/jira/connect", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("redirect_uri") != "https://console.acme.test/connections/callback" {
		t.Fatalf("authorize URL %s", loc)
	}
	req = httptest.NewRequest("GET", "/connections/callback?code="+code+"&state="+loc.Query().Get("state"), nil)
	req.AddCookie(ck)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Result()
}

func testAuthOnly() *Auth {
	return &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
}

func TestConnectStoresEachUsersOwnGrantSealed(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	a := testAuthOnly()
	if r := connect(t, conns, a, "alice@acme.test", "alice"); r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/connections?connected=jira") {
		t.Fatalf("callback: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	connect(t, conns, a, "bob@acme.test", "bob")
	if saas.lastVerifier == "" {
		t.Fatal("no PKCE verifier sent")
	}
	ga, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	gb, _ := conns.Store.Get(t.Context(), "bob@acme.test", "jira")
	if ga == nil || gb == nil || ga.AccessToken != "at-alice" || gb.AccessToken != "at-bob" {
		t.Fatalf("grants %+v %+v", ga, gb)
	}
	var secrets corev1.SecretList
	c.List(t.Context(), &secrets, client.InNamespace("ambit-grants"), client.HasLabels{grantConnLabel})
	for _, s := range secrets.Items {
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), "at-alice") || strings.Contains(string(raw), "rt-alice") || strings.Contains(string(raw), "alice@") {
			t.Fatalf("grant Secret %s shows the token or the email in the clear", s.Name)
		}
	}
}

func TestCallbackRefusesAForeignState(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	a := testAuthOnly()
	mux := http.NewServeMux()
	conns.register(mux, a)
	req := httptest.NewRequest("GET", "/connections/callback?code=alice&state=forged", nil)
	req.AddCookie(user(a, "alice@acme.test", RoleViewer))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || saas.issued != 0 {
		t.Fatalf("forged state: %d, tokens issued %d", rec.Code, saas.issued)
	}
}

// brokerCall is the gateway's extAuth request: the user named by the validated jwt.email claim.
func brokerCall(t *testing.T, b *Broker, email string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/connection/jira", nil)
	if email != "" {
		req.Header.Set(userHeader, email)
	}
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	return rec
}

func TestBrokerGivesEachUserTheirOwnToken(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	a := testAuthOnly()
	connect(t, conns, a, "alice@acme.test", "alice")
	connect(t, conns, a, "bob@acme.test", "bob")
	b := &Broker{Conns: conns}
	for who, want := range map[string]string{"alice@acme.test": "Bearer at-alice", "bob@acme.test": "Bearer at-bob"} {
		if r := brokerCall(t, b, who); r.Code != 200 || r.Header().Get("Authorization") != want {
			t.Fatalf("%s: %d %q", who, r.Code, r.Header().Get("Authorization"))
		}
	}
	// No grant or no user: no token, so the server refuses and only it drops out of the session.
	for _, who := range []string{"carol@acme.test", ""} {
		r := brokerCall(t, b, who)
		if r.Code != 200 || r.Header().Get("Authorization") != "" || r.Header().Get("X-Ambit-Connect") == "" {
			t.Fatalf("%q: %d auth=%q", who, r.Code, r.Header().Get("Authorization"))
		}
	}
	if r := brokerCall(t, b, "carol@acme.test"); !strings.Contains(r.Header().Get("X-Ambit-Connect"), "https://console.acme.test/connections/jira/connect") {
		t.Fatalf("link %q", r.Header().Get("X-Ambit-Connect"))
	}
}

func TestExpiredTokenIsRenewedAndAFailedRenewalMarksTheGrantBroken(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	a := testAuthOnly()
	connect(t, conns, a, "alice@acme.test", "alice")
	conns.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past expiry
	tok, g, err := conns.Token(t.Context(), "alice@acme.test", "jira")
	if err != nil || tok != "renewed-alice" || g.State != grantConnectedState {
		t.Fatalf("renew: %q %+v %v", tok, g, err)
	}
	saas.refuseRenew = true
	checked, broken := conns.CheckAll(t.Context(), true)
	g, _ = conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	if checked != 1 || broken != 1 || g.State != grantBrokenState || !strings.Contains(g.Error, "Session not active") {
		t.Fatalf("check: %d %d %+v", checked, broken, g)
	}
	if tok, _, _ := conns.Token(t.Context(), "alice@acme.test", "jira"); tok != "" {
		t.Fatal("a broken grant still produced a token")
	}
}

func TestDisconnectRevokesAtTheProvider(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	a := testAuthOnly()
	connect(t, conns, a, "alice@acme.test", "alice")
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	revoked, err := conns.Revoke(t.Context(), g)
	if err != nil || !revoked || len(saas.revoked) != 1 || saas.revoked[0] != "rt-alice" {
		t.Fatalf("revoke: %v %v %v", revoked, err, saas.revoked)
	}
	if g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira"); g != nil {
		t.Fatal("grant kept after revocation")
	}
}

// A provider format that sends the access token gets a current one: an expired grant is renewed first.
func TestRevokeRenewsAnExpiredAccessTokenFirst(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.RevocationURL = saas.srv.URL + "/applications/{client_id}/grant"
	if err := c.Update(t.Context(), &conn); err != nil {
		t.Fatal(err)
	}
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	g.Expiry = time.Now().Add(-time.Hour)
	_ = conns.Store.Put(t.Context(), g)

	revoked, err := conns.Revoke(t.Context(), g)
	if err != nil || !revoked || len(saas.revoked) != 1 || saas.revoked[0] != "renewed-alice" {
		t.Fatalf("revoke: %v %v %v", revoked, err, saas.revoked)
	}
	if g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira"); g != nil {
		t.Fatal("grant kept after revocation")
	}
	// A refusal is reported, and the grant is kept (nothing was revoked).
	connect(t, conns, testAuthOnly(), "bob@acme.test", "bob")
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.RevocationURL = saas.srv.URL + "/applications/x/grant"
	conn.Spec.ClientID = "not-ambit"
	_ = c.Update(t.Context(), &conn)
	g, _ = conns.Store.Get(t.Context(), "bob@acme.test", "jira")
	if revoked, err := conns.Revoke(t.Context(), g); revoked || err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("refused revocation: %v %v", revoked, err)
	}
}

// A provider that echoes the token in its error does not put it into the error Ambit records.
func TestRevokeErrorsNeverContainTokens(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		http.Error(w, `{"error":"invalid_token","token":"`+r.PostForm.Get("token")+`"}`, http.StatusBadRequest)
	}))
	t.Cleanup(echo.Close)
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.RevocationURL = echo.URL + "/revoke"
	_ = c.Update(t.Context(), &conn)
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	_, err := conns.Revoke(t.Context(), g)
	if err == nil || strings.Contains(err.Error(), "rt-alice") || !strings.Contains(err.Error(), "[token]") {
		t.Fatalf("error: %v", err)
	}
}

func TestAuthorizeSendsResourceAndParams(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.Resource = "https://mcp.jira.test/mcp"
	conn.Spec.AuthorizeParams = map[string]string{"prompt": "consent"}
	if err := c.Update(t.Context(), &conn); err != nil {
		t.Fatal(err)
	}
	a := testAuthOnly()
	mux := http.NewServeMux()
	conns.register(mux, a)
	req := httptest.NewRequest("GET", "/connections/jira/connect", nil)
	req.AddCookie(user(a, "alice@acme.test", RoleViewer))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("resource") != "https://mcp.jira.test/mcp" || loc.Query().Get("prompt") != "consent" {
		t.Fatalf("authorize URL %s", loc)
	}
}

// startThenChange starts a connect flow, runs change while the user is at the provider, then calls back.
func startThenChange(t *testing.T, change func(c client.Client, saas *fakeSaaS)) (*httptest.ResponseRecorder, *fakeSaaS) {
	t.Helper()
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	a := testAuthOnly()
	mux := http.NewServeMux()
	conns.register(mux, a)
	ck := user(a, "alice@acme.test", RoleViewer)
	req := httptest.NewRequest("GET", "/connections/jira/connect", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	change(c, saas)
	req = httptest.NewRequest("GET", "/connections/callback?code=alice&state="+loc.Query().Get("state"), nil)
	req.AddCookie(ck)
	for _, k := range rec.Result().Cookies() {
		req.AddCookie(k)
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	return rec2, saas
}

func TestCallbackRefusesWhenAppChanged(t *testing.T) {
	rec, saas := startThenChange(t, func(c client.Client, _ *fakeSaaS) {
		// The admin changes the client secret while alice is at the provider.
		var sec corev1.Secret
		_ = c.Get(t.Context(), client.ObjectKey{Namespace: "ambit-grants", Name: "ambit-conn-jira"}, &sec)
		sec.Data["clientSecret"] = []byte("rotated")
		_ = c.Update(t.Context(), &sec)
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed while you were connecting") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	if saas.issued != 0 {
		t.Fatal("the code was exchanged with the new configuration")
	}
}

func TestCallbackRefusesRecreatedApp(t *testing.T) {
	rec, _ := startThenChange(t, func(c client.Client, _ *fakeSaaS) {
		// The app is deleted and added again under the same name (new UID) while alice is at the provider.
		var conn v1.Connection
		_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
		_ = c.Delete(t.Context(), &conn)
		conn.ResourceVersion, conn.UID = "", "new-uid"
		_ = c.Create(t.Context(), &conn)
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("callback after recreate: %d %s", rec.Code, rec.Body)
	}
}

func TestCallbackRefusesChangedGeneration(t *testing.T) {
	rec, _ := startThenChange(t, func(c client.Client, _ *fakeSaaS) {
		var conn v1.Connection
		_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
		conn.Spec.Scopes = []string{"read", "write"}
		conn.Generation++ // the fake client does not bump generation on spec changes
		_ = c.Update(t.Context(), &conn)
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("callback after edit: %d %s", rec.Code, rec.Body)
	}
}

func TestGrantForOldResourceIsBroken(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice") // grant issued with no resource
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.Resource = "https://mcp.other.test/mcp"
	_ = c.Update(t.Context(), &conn)
	tok, g, err := conns.Token(t.Context(), "alice@acme.test", "jira")
	if err != nil || tok != "" || g == nil || g.State != grantBrokenState || !strings.Contains(g.Error, "app's server changed") {
		t.Fatalf("token %q grant %+v err %v", tok, g, err)
	}
}

// A vendor may ignore resource (or the preset sends none): the server URL alone must also invalidate old grants.
func TestGrantForOldServerURLIsBroken(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.ServerURL = "https://mcp.jira.test/mcp"
	_ = c.Update(t.Context(), &conn)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.ServerURL = "https://mcp.elsewhere.test/mcp" // resource unchanged (empty)
	_ = c.Update(t.Context(), &conn)
	if tok, g, _ := conns.Token(t.Context(), "alice@acme.test", "jira"); tok != "" || g.State != grantBrokenState {
		t.Fatalf("old grant used for a new server URL: %q %+v", tok, g)
	}
}

func expireAll(conns *Connections) {
	conns.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
}

func TestRefreshTransientErrorKeepsGrant(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	saas.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) })
	expireAll(conns)
	_, g, err := conns.Token(t.Context(), "alice@acme.test", "jira")
	if err == nil || g == nil || g.State != grantConnectedState {
		t.Fatalf("grant %+v err %v", g, err)
	}
}

func TestRotatedRefreshTokenIsStored(t *testing.T) {
	saas := newFakeSaaS(t)
	saas.rotate = true // fake returns refresh_token "rt2-<user>" on renewal (add this field to fakeSaaS)
	conns, _ := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	expireAll(conns)
	if _, _, err := conns.Token(t.Context(), "alice@acme.test", "jira"); err != nil {
		t.Fatal(err)
	}
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	if g.RefreshToken != "rt2-alice" {
		t.Fatalf("refresh token %q", g.RefreshToken)
	}
}

func TestNoRefreshTokenMeansReconnectAfterExpiry(t *testing.T) {
	saas := newFakeSaaS(t)
	saas.noRefreshToken = true // fake issues no refresh_token at the code exchange (add this field)
	conns, _ := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	if tok, _, _ := conns.Token(t.Context(), "alice@acme.test", "jira"); tok != "at-alice" {
		t.Fatalf("valid token refused: %q", tok)
	}
	conns.CheckAll(t.Context(), true) // a forced check must not break a still-valid grant it cannot renew
	if g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira"); g.State != grantConnectedState {
		t.Fatalf("forced check broke a valid grant: %+v", g)
	}
	expireAll(conns)
	tok, g, _ := conns.Token(t.Context(), "alice@acme.test", "jira")
	if tok != "" || g.State != grantBrokenState || !strings.Contains(g.Error, "reconnect") {
		t.Fatalf("token %q grant %+v", tok, g)
	}
}

func TestCheckAllAppliesResourceRule(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.Resource = "https://mcp.jira.test/mcp"
	_ = c.Update(t.Context(), &conn)
	conns.CheckAll(t.Context(), true)
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	if g.State != grantBrokenState || saas.renewed != 0 { // add a renewal counter to fakeSaaS
		t.Fatalf("grant %+v renewals %d", g, saas.renewed)
	}
}

func TestRefreshAndRevokeNeverOverlap(t *testing.T) {
	saas := newFakeSaaS(t)
	conns, _ := connFixture(t, saas)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	expireAll(conns)
	g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); conns.Token(t.Context(), "alice@acme.test", "jira") }()
	go func() { defer wg.Done(); conns.Revoke(t.Context(), g) }()
	go func() { conns.CheckAll(t.Context(), true) }() // the hourly check racing the same grant
	wg.Wait()
	conns.CheckAll(t.Context(), true)
	if g, _ := conns.Store.Get(t.Context(), "alice@acme.test", "jira"); g != nil {
		t.Fatal("a renewal recreated the grant after it was revoked")
	}
}
