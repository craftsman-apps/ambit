package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	pubURL   = "https://auth.example.com"
	resource = "https://mcp.example.com/mcp"
	idpIss   = "https://idp.example.com"
)

// ---- fakes: Hydra (the admin and public parts ambit-auth uses), the identity provider, the console

type fakeHydra struct {
	mu       sync.Mutex
	logins   map[string]*LoginRequest
	consents map[string]*ConsentRequest
	used     map[string]bool
	accepted map[string]map[string]any // challenge -> what was sent
	rejected map[string]string         // challenge -> error code
	clients  map[string]map[string]any
	seq      int
	authLoc  string // what the fake /oauth2/auth answers with as Location
}

func newFakeHydra() *fakeHydra {
	return &fakeHydra{logins: map[string]*LoginRequest{}, consents: map[string]*ConsentRequest{}, used: map[string]bool{},
		accepted: map[string]map[string]any{}, rejected: map[string]string{}, clients: map[string]map[string]any{}}
}

func (f *fakeHydra) addLogin(ch, requestURL string) {
	f.logins[ch] = &LoginRequest{Challenge: ch, RequestURL: requestURL, Client: Client{ID: "c1", Name: "ChatGPT"}}
}

func (f *fakeHydra) addConsent(ch, subject, requestURL string, redirects ...string) {
	if len(redirects) == 0 {
		redirects = []string{"https://chatgpt.com/connector_platform_oauth_redirect"}
	}
	f.consents[ch] = &ConsentRequest{Challenge: ch, Subject: subject, RequestURL: requestURL, RequestedScope: []string{"mcp", "offline_access"},
		Client: Client{ID: "c1", Name: "ChatGPT", RedirectURIs: redirects}, Context: map[string]any{"groups": []any{"eng"}, "bind": "bind-of-" + ch}}
}

func (f *fakeHydra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
	}
	p := r.URL.Path
	switch {
	case p == "/oauth2/auth":
		if f.authLoc != "" {
			w.Header().Set("Location", f.authLoc)
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Location", pubURL+"/login?login_challenge=x")
		w.WriteHeader(302)
	case strings.HasPrefix(p, "/admin/oauth2/auth/requests/login") && r.Method == "GET":
		ch := r.URL.Query().Get("login_challenge")
		if l, ok := f.logins[ch]; ok && !f.used[ch] {
			reply(200, l)
			return
		}
		reply(404, nil)
	case strings.HasPrefix(p, "/admin/oauth2/auth/requests/consent") && r.Method == "GET":
		ch := r.URL.Query().Get("consent_challenge")
		if c, ok := f.consents[ch]; ok && !f.used[ch] {
			reply(200, c)
			return
		}
		reply(404, nil)
	case strings.HasSuffix(p, "/accept") || strings.HasSuffix(p, "/reject"):
		ch := r.URL.Query().Get("login_challenge") + r.URL.Query().Get("consent_challenge")
		if f.used[ch] || (f.logins[ch] == nil && f.consents[ch] == nil) {
			reply(410, nil)
			return
		}
		f.used[ch] = true
		if strings.HasSuffix(p, "/accept") {
			f.accepted[ch] = body
		} else {
			f.rejected[ch], _ = body["error"].(string)
		}
		reply(200, map[string]string{"redirect_to": pubURL + "/oauth2/auth?verifier=" + ch})
	case p == "/admin/clients" && r.Method == "POST":
		f.seq++
		id := fmt.Sprintf("dcr-%d", f.seq)
		body["client_id"] = id
		body["created_at"] = time.Now().UTC().Format(time.RFC3339)
		f.clients[id] = body
		reply(201, body)
	case p == "/admin/clients" && r.Method == "GET":
		var l []map[string]any
		for _, c := range f.clients {
			l = append(l, c)
		}
		reply(200, l)
	case strings.HasPrefix(p, "/admin/clients/"):
		id := strings.TrimPrefix(p, "/admin/clients/")
		c, ok := f.clients[id]
		switch {
		case !ok:
			reply(404, nil)
		case r.Method == "GET":
			reply(200, c)
		case r.Method == "PUT":
			body["client_id"] = id
			f.clients[id] = body
			reply(200, body)
		case r.Method == "DELETE":
			delete(f.clients, id)
			w.WriteHeader(204)
		}
	default:
		reply(404, nil)
	}
}

type fakeIdP struct {
	mu     sync.Mutex
	nonces map[string]string // state -> nonce
	claims map[string]IDClaims
}

func (f *fakeIdP) AuthURL(state, nonce, verifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nonces == nil {
		f.nonces = map[string]string{}
	}
	f.nonces[state] = nonce
	return idpIss + "/auth?state=" + url.QueryEscape(state)
}

func (f *fakeIdP) IssRequired() bool { return false }

// Exchange: the code is "<name>|<state>"; the claims are looked up by name, and carry the nonce of the state.
func (f *fakeIdP) Exchange(_ context.Context, code, _ string) (IDClaims, error) {
	name, state, _ := strings.Cut(code, "|")
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "fail" {
		return IDClaims{}, errors.New("code exchange failed")
	}
	c, ok := f.claims[name]
	if !ok {
		return IDClaims{}, errors.New("unknown code")
	}
	if c.Nonce == "" {
		c.Nonce = f.nonces[state]
	}
	return c, nil
}

type fakeBackend struct {
	mu      sync.Mutex
	servers map[string][]ServerChoice
	created []AgentInput
	refuse  string
	groups  []string
	down    bool
}

func (b *fakeBackend) Servers(_ context.Context, email string) ([]ServerChoice, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errors.New("down")
	}
	return b.servers[email], nil
}
func (b *fakeBackend) CreateAgent(_ context.Context, in AgentInput) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse != "" {
		return "", ErrRefused{b.refuse}
	}
	b.created = append(b.created, in)
	return "chatgpt-" + fmt.Sprint(len(b.created)), nil
}
func (b *fakeBackend) Groups(context.Context, string) ([]string, error) {
	if b.down {
		return nil, errors.New("down")
	}
	return b.groups, nil
}

type fakeDeny struct {
	off, stopA, stopG map[string]bool
	err               error
}

func (d *fakeDeny) Offboarded(e string) (bool, error) { return d.off[e], d.err }
func (d *fakeDeny) Stopped(a, g string) (bool, error) { return d.stopA[a] || d.stopG[g], d.err }

type env struct {
	hydra   *fakeHydra
	hsrv    *httptest.Server
	idp     *fakeIdP
	be      *fakeBackend
	deny    *fakeDeny
	reg     *MemRegistry
	lim     *Limits
	srv     *Server
	handler http.Handler
	now     time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{hydra: newFakeHydra(), idp: &fakeIdP{claims: map[string]IDClaims{
		"alice":  {Issuer: idpIss, Subject: "s1", Email: "Alice@Acme.test", EmailVerified: true, Groups: []string{"eng"}},
		"bob":    {Issuer: idpIss, Subject: "s2", Email: "bob@acme.test", EmailVerified: true},
		"carol":  {Issuer: idpIss, Subject: "s3", Email: "carol@acme.test", EmailVerified: false},
		"svc":    {Issuer: idpIss, Subject: "s4", Email: "x@svc.ambit.internal", EmailVerified: true},
		"other":  {Issuer: "https://evil.example.com", Subject: "s5", Email: "alice@acme.test", EmailVerified: true},
		"noat":   {Issuer: idpIss, Subject: "s6", Email: "alice.acme.test", EmailVerified: true},
		"badnon": {Issuer: idpIss, Subject: "s7", Email: "alice@acme.test", EmailVerified: true, Nonce: "forged"},
	}}, be: &fakeBackend{servers: map[string][]ServerChoice{
		"alice@acme.test": {{Name: "crm", Display: "CRM"}, {Name: "wiki", Display: "Wiki", Connection: "wiki-login"}},
	}, groups: []string{"eng", "new-group"}}, deny: &fakeDeny{off: map[string]bool{"bob@acme.test": true}, stopA: map[string]bool{}, stopG: map[string]bool{}},
		reg: &MemRegistry{}, now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	e.hsrv = httptest.NewServer(e.hydra)
	t.Cleanup(e.hsrv.Close)
	sealer, err := NewSealer([]byte(strings.Repeat("k", 40)))
	if err != nil {
		t.Fatal(err)
	}
	e.lim = &Limits{Local: &MemLimiter{Now: func() time.Time { return e.now }}, Rates: DefaultRates()}
	e.srv = &Server{Cfg: Config{PublicURL: pubURL, Resource: resource, IdPIssuer: idpIss, IdPClientID: "ambit", IdPClientSecret: "s",
		RegistrationHosts: []string{"chatgpt.com", "claude.ai"}, MaxClients: 3, UnusedClientDays: 30},
		Sealer: sealer, Hydra: &HydraAdmin{Base: e.hsrv.URL}, HydraPub: e.hsrv.URL, Backend: e.be, Deny: e.deny, IdP: e.idp,
		Limits: e.lim, Clients: e.reg, Now: func() time.Time { return e.now }}
	if err := e.srv.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e.handler = e.srv.Public()
	return e
}

// browser keeps cookies like a browser does, by name.
type browser struct {
	e       *env
	h       http.Handler
	cookies map[string]string
	addr    string
}

func (e *env) browser() *browser {
	return &browser{e: e, h: e.handler, cookies: map[string]string{}, addr: "203.0.113.7"}
}

// bind gives the browser the cookie that the callback gives the person who signed in for this consent request.
func (e *env) bind(b *browser, consent string) {
	id, _ := e.hydra.consents[consent].Context["bind"].(string)
	v, err := e.srv.Sealer.Seal(Tx{Login: id, Exp: e.now.Add(time.Minute).Unix()}, bindName(id))
	if err != nil {
		panic(err)
	}
	b.cookies[bindName(id)] = v
}

func (b *browser) do(method, target string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set(ClientAddrHeader, b.addr)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for n, v := range b.cookies {
		r.AddCookie(&http.Cookie{Name: n, Value: v})
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return w
}

func loc(w *httptest.ResponseRecorder) string { return w.Header().Get("Location") }

func goodRequestURL(extra string) string {
	return "https://auth.example.com/oauth2/auth?client_id=c1&response_type=code&redirect_uri=" + url.QueryEscape("https://chatgpt.com/connector_platform_oauth_redirect") + "&resource=" + url.QueryEscape(resource) + extra
}

// validAuthPath is an authorization request that passes the front's checks (a continuation carries these parameters again).
func validAuthPath(extra string) string {
	return strings.TrimPrefix(goodRequestURL("&state=state-0123&code_challenge="+strings.Repeat("a", 43)+"&code_challenge_method=S256&scope=mcp+offline_access"+extra), pubURL)
}

// startLogin runs /login and returns the state that went to the identity provider.
func (e *env) startLogin(t *testing.T, b *browser, ch string) string {
	t.Helper()
	w := b.do("GET", "/login?login_challenge="+ch, nil, nil)
	if w.Code != 302 || !strings.HasPrefix(loc(w), idpIss+"/auth") {
		t.Fatalf("login %s: %d %s %q", ch, w.Code, w.Body.String(), loc(w))
	}
	u, _ := url.Parse(loc(w))
	return u.Query().Get("state")
}

// ---- sign-in

func TestTwoSignInsInOneBrowserCompleteInReverseOrder(t *testing.T) {
	e := newEnv(t)
	e.hydra.addLogin("A", goodRequestURL(""))
	e.hydra.addLogin("B", goodRequestURL(""))
	b := e.browser()
	stA := e.startLogin(t, b, "A")
	stB := e.startLogin(t, b, "B")
	if stA == stB || len(b.cookies) != 2 {
		t.Fatalf("two sign-ins need two transactions: %d cookies", len(b.cookies))
	}
	// B finishes first, then A
	for _, c := range []struct{ ch, st, who string }{{"B", stB, "alice"}, {"A", stA, "alice"}} {
		w := b.do("GET", "/callback?code="+c.who+"|"+url.QueryEscape(c.st)+"&state="+url.QueryEscape(c.st), nil, nil)
		if w.Code != 302 || !strings.Contains(loc(w), "verifier="+c.ch) {
			t.Fatalf("callback %s: %d %s %q", c.ch, w.Code, w.Body.String(), loc(w))
		}
		if e.hydra.accepted[c.ch]["subject"] != "alice@acme.test" {
			t.Fatalf("login %s accepted for %v", c.ch, e.hydra.accepted[c.ch]["subject"])
		}
	}
	for n := range b.cookies {
		if strings.HasPrefix(n, txPrefix) {
			t.Fatalf("the transaction cookies must be gone: %v", b.cookies)
		}
	}
	// replay of a finished callback: no cookie, refused
	if w := b.do("GET", "/callback?code=alice|"+url.QueryEscape(stA)+"&state="+url.QueryEscape(stA), nil, nil); w.Code != 400 {
		t.Fatalf("replay: %d", w.Code)
	}
}

func TestTransactionCookiesAreBounded(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for i := 0; i < 20; i++ {
		ch := fmt.Sprintf("L%d", i)
		e.hydra.addLogin(ch, goodRequestURL(""))
		e.startLogin(t, b, ch)
	}
	if len(b.cookies) > maxTxLive {
		t.Fatalf("%d pending sign-ins kept in one browser, at most %d", len(b.cookies), maxTxLive)
	}
}

func TestCallbackRefusals(t *testing.T) {
	type tc struct {
		name   string
		code   string // who signs in
		extra  string // extra query on the callback
		mutate func(st string, b *browser) string
		status int    // 302 with a Hydra reject, or 400
		reject string // expected Hydra reject code ("" for status 400 pages)
	}
	for _, c := range []tc{
		{name: "ok", code: "alice", status: 302},
		{name: "wrong state", code: "alice", mutate: func(st string, _ *browser) string { return "x" + st }, status: 400},
		{name: "no cookie", code: "alice", mutate: func(st string, b *browser) string { b.cookies = map[string]string{}; return st }, status: 400},
		{name: "tampered cookie", code: "alice", mutate: func(st string, b *browser) string {
			for k, v := range b.cookies {
				b.cookies[k] = v[:len(v)-3] + "AAA"
			}
			return st
		}, status: 400},
		{name: "idp iss differs", code: "alice", extra: "&iss=https://evil.example.com", status: 302, reject: "access_denied"},
		{name: "idp error", code: "alice", extra: "&error=access_denied", status: 302, reject: "access_denied"},
		{name: "code exchange fails", code: "fail", status: 302, reject: "access_denied"},
		{name: "id token from another issuer", code: "other", status: 302, reject: "access_denied"},
		{name: "forged nonce", code: "badnon", status: 302, reject: "access_denied"},
		{name: "email not verified", code: "carol", status: 302, reject: "access_denied"},
		{name: "service account domain", code: "svc", status: 302, reject: "access_denied"},
		{name: "offboarded", code: "bob", status: 302, reject: "access_denied"},
		{name: "no usable email", code: "noat", status: 302, reject: "access_denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.hydra.addLogin("A", goodRequestURL(""))
			b := e.browser()
			st := e.startLogin(t, b, "A")
			use := st
			if c.mutate != nil {
				use = c.mutate(st, b)
			}
			w := b.do("GET", "/callback?code="+c.code+"|"+url.QueryEscape(st)+"&state="+url.QueryEscape(use)+c.extra, nil, nil)
			if w.Code != c.status {
				t.Fatalf("status %d, want %d: %s", w.Code, c.status, w.Body.String())
			}
			if c.reject != "" && e.hydra.rejected["A"] != c.reject {
				t.Fatalf("Hydra got reject %q, want %q (accepted: %v)", e.hydra.rejected["A"], c.reject, e.hydra.accepted["A"])
			}
			if c.name != "ok" && e.hydra.accepted["A"] != nil {
				t.Fatalf("the sign-in was accepted: %v", e.hydra.accepted["A"])
			}
			if c.name == "ok" {
				ctx := e.hydra.accepted["A"]["context"].(map[string]any)
				if ctx["idp_iss"] != idpIss || ctx["idp_sub"] != "s1" || e.hydra.accepted["A"]["remember"] != false {
					t.Fatalf("accepted with %v", e.hydra.accepted["A"])
				}
			}
			if strings.Contains(w.Body.String(), "access_denied") || strings.Contains(w.Body.String(), "evil") {
				t.Fatal("the answer repeats request text")
			}
		})
	}
}

func TestDenyListUnreadableFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.hydra.addLogin("A", goodRequestURL(""))
	b := e.browser()
	st := e.startLogin(t, b, "A")
	e.deny.err = errors.New("stale")
	w := b.do("GET", "/callback?code=alice|"+url.QueryEscape(st)+"&state="+url.QueryEscape(st), nil, nil)
	if w.Code != 503 || e.hydra.accepted["A"] != nil {
		t.Fatalf("%d, accepted %v", w.Code, e.hydra.accepted["A"])
	}
}

// ---- the resource parameter: exactly one, and ours

func TestResourceChecks(t *testing.T) {
	good := url.Values{"client_id": {"c1"}, "redirect_uri": {"https://chatgpt.com/cb"}, "response_type": {"code"}, "code_challenge_method": {"S256"},
		"code_challenge": {strings.Repeat("a", 43)}, "state": {"s"}, "resource": {resource}}
	e := newEnv(t)
	b := e.browser()
	try := func(mut func(v url.Values)) int {
		v := url.Values{}
		for k, x := range good {
			v[k] = append([]string(nil), x...)
		}
		mut(v)
		return b.do("GET", "/oauth2/auth?"+v.Encode(), nil, nil).Code
	}
	if c := try(func(url.Values) {}); c != 302 {
		t.Fatalf("a good request: %d", c)
	}
	for name, mut := range map[string]func(v url.Values){
		"resource missing":    func(v url.Values) { v.Del("resource") },
		"resource twice":      func(v url.Values) { v.Add("resource", resource) },
		"resource other":      func(v url.Values) { v.Set("resource", "https://other.example.com/mcp") },
		"resource + other":    func(v url.Values) { v.Add("resource", "https://other.example.com/mcp") },
		"resource with slash": func(v url.Values) { v.Set("resource", resource+"/") },
		"no pkce":             func(v url.Values) { v.Del("code_challenge") },
		"plain pkce":          func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"short challenge":     func(v url.Values) { v.Set("code_challenge", "abc") },
		"implicit":            func(v url.Values) { v.Set("response_type", "token") },
		"fragment mode":       func(v url.Values) { v.Set("response_mode", "fragment") },
		"form_post mode":      func(v url.Values) { v.Set("response_mode", "form_post") },
		"state twice":         func(v url.Values) { v.Add("state", "t") },
		"redirect_uri twice":  func(v url.Values) { v.Add("redirect_uri", "https://claude.ai/cb") },
		"request object":      func(v url.Values) { v.Set("request", "eyJ") },
		"request_uri":         func(v url.Values) { v.Set("request_uri", "https://x/y") },
		"unknown scope":       func(v url.Values) { v.Set("scope", "mcp admin") },
		"no client":           func(v url.Values) { v.Del("client_id") },
	} {
		if c := try(mut); c != 400 {
			t.Errorf("%s: HTTP %d, want 400", name, c)
		}
	}
	// the default scope is added, and query_mode explicitly is fine
	if c := try(func(v url.Values) { v.Set("response_mode", "query"); v.Set("scope", "mcp offline_access") }); c != 302 {
		t.Errorf("explicit query mode and scopes: %d", c)
	}
	// the login and the consent step check the request Hydra stored, not only the front
	for name, ru := range map[string]string{"missing": "https://auth.example.com/oauth2/auth?client_id=c1", "twice": goodRequestURL("&resource=" + url.QueryEscape(resource)),
		"other": "https://auth.example.com/oauth2/auth?resource=https%3A%2F%2Fother.example.com%2Fmcp"} {
		e.hydra.addLogin("L"+name, ru)
		if w := b.do("GET", "/login?login_challenge=L"+name, nil, nil); w.Code != 302 || e.hydra.rejected["L"+name] != "invalid_target" {
			t.Errorf("login %s: %d, Hydra reject %q", name, w.Code, e.hydra.rejected["L"+name])
		}
		e.hydra.addConsent("C"+name, "alice@acme.test", ru)
		e.bind(b, "C"+name)
		if w := b.do("GET", "/consent?consent_challenge=C"+name, nil, nil); w.Code != 302 || e.hydra.rejected["C"+name] != "invalid_target" {
			t.Errorf("consent %s: %d, Hydra reject %q", name, w.Code, e.hydra.rejected["C"+name])
		}
	}
}

func TestAuthorizeAddsIssOnlyToTheResponseThatLeaves(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for name, tc := range map[string]struct{ from, want string }{
		"code":          {"https://chatgpt.com/cb?code=abc&state=s", "https://chatgpt.com/cb?code=abc&state=s&iss=" + url.QueryEscape(pubURL)},
		"error":         {"https://chatgpt.com/cb?error=access_denied&state=s", "https://chatgpt.com/cb?error=access_denied&state=s&iss=" + url.QueryEscape(pubURL)},
		"to login":      {pubURL + "/login?login_challenge=x", pubURL + "/login?login_challenge=x"},
		"to consent":    {pubURL + "/consent?consent_challenge=x", pubURL + "/consent?consent_challenge=x"},
		"other no code": {"https://chatgpt.com/cb?state=s", "https://chatgpt.com/cb?state=s"},
	} {
		e.hydra.authLoc = tc.from
		w := b.do("GET", validAuthPath("&login_verifier=v"), nil, nil)
		if loc(w) != tc.want {
			t.Errorf("%s: Location %q, want %q", name, loc(w), tc.want)
		}
	}
	e.hydra.authLoc = "https://chatgpt.com/cb?code=abc&iss=https%3A%2F%2Fhydra"
	if w := b.do("GET", validAuthPath("&login_verifier=v"), nil, nil); w.Code != 502 {
		t.Errorf("Hydra sent its own iss: %d, want 502 (never two)", w.Code)
	}
}

func TestMetadata(t *testing.T) {
	e := newEnv(t)
	w := e.browser().do("GET", "/.well-known/oauth-authorization-server", nil, nil)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["issuer"] != pubURL || m["authorization_response_iss_parameter_supported"] != true || m["registration_endpoint"] != pubURL+"/register" ||
		fmt.Sprint(m["code_challenge_methods_supported"]) != "[S256]" || fmt.Sprint(m["response_modes_supported"]) != "[query]" || fmt.Sprint(m["grant_types_supported"]) != "[authorization_code refresh_token]" {
		t.Fatalf("metadata: %v", m)
	}
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("header %s missing", h)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("clickjacking headers: %v", w.Header())
	}
}

// A cookie is sealed for its own name: the record of one sign-in, copied under the name of another, does not open.
func TestTransactionCookieIsBoundToItsName(t *testing.T) {
	e := newEnv(t)
	e.hydra.addLogin("A", goodRequestURL(""))
	e.hydra.addLogin("B", goodRequestURL(""))
	b := e.browser()
	stA := e.startLogin(t, b, "A")
	hA, _, _ := strings.Cut(stA, ".")
	delete(b.cookies, txPrefix+hA)
	var bv string
	stB := e.startLogin(t, b, "B")
	hB, _, _ := strings.Cut(stB, ".")
	bv = b.cookies[txPrefix+hB]
	b.cookies = map[string]string{txPrefix + hA: bv} // B's record under A's name
	w := b.do("GET", "/callback?code=alice|"+url.QueryEscape(stA)+"&state="+url.QueryEscape(stA), nil, nil)
	if w.Code != 400 || e.hydra.accepted["A"] != nil || e.hydra.accepted["B"] != nil {
		t.Fatalf("%d, accepted %v %v", w.Code, e.hydra.accepted["A"], e.hydra.accepted["B"])
	}
}
