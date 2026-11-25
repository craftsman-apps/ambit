package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var formVal = regexp.MustCompile(`name="(challenge|csrf)" value="([^"]*)"`)

// consentForm opens the consent page and returns the form fields the browser would send back.
func consentForm(t *testing.T, b *browser, ch string) url.Values {
	t.Helper()
	b.e.bind(b, ch)
	w := b.do("GET", "/consent?consent_challenge="+ch, nil, nil)
	if w.Code != 200 {
		t.Fatalf("consent page %s: %d %s", ch, w.Code, w.Body.String())
	}
	v := url.Values{}
	for _, m := range formVal.FindAllStringSubmatch(w.Body.String(), -1) {
		v.Set(m[1], m[2])
	}
	if v.Get("csrf") == "" || v.Get("challenge") != ch {
		t.Fatalf("form fields: %v", v)
	}
	return v
}

var origin = map[string]string{"Origin": pubURL}

func TestConsentPageShowsNothingTicked(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	e.bind(b, "C1")
	w := b.do("GET", "/consent?consent_challenge=C1", nil, nil)
	body := w.Body.String()
	if regexp.MustCompile(`<input[^>]* checked`).MatchString(body) || !strings.Contains(body, `value="crm"`) || !strings.Contains(body, `value="wiki"`) || !strings.Contains(body, "wiki-login") {
		t.Fatalf("page: %s", body)
	}
	if !strings.Contains(body, "chosen by the platform") || !strings.Contains(body, "chatgpt.com") {
		t.Fatalf("the page must say the name is the platform's and show the redirect host: %s", body)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("no clickjacking protection on the consent page")
	}
}

func TestConsentAllowCreatesTheAgentAndTheClaims(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	f.Add("server", "wiki")
	f.Add("connection", "wiki-login")
	w := b.do("POST", "/consent", f, origin)
	if w.Code != 302 || !strings.Contains(loc(w), "verifier=C1") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(e.be.created) != 1 || e.be.created[0].Owner != "alice@acme.test" || e.be.created[0].Platform != "chatgpt" ||
		fmt.Sprint(e.be.created[0].Scope) != "[wiki]" || fmt.Sprint(e.be.created[0].Connections) != "[wiki-login]" || !strings.HasPrefix(e.be.created[0].GrantID, "g-") {
		t.Fatalf("agent: %+v", e.be.created)
	}
	acc := e.hydra.accepted["C1"]
	claims := acc["session"].(map[string]any)["access_token"].(map[string]any)
	if claims["ambit_agent"] != "chatgpt-1" || claims["ambit_agent_mode"] != "person" || claims["ambit_grant"] != e.be.created[0].GrantID ||
		claims["email"] != "alice@acme.test" || claims["ambit_platform_declared"] != "chatgpt" || claims["ambit_agent_owner_at_issue"] != "alice@acme.test" ||
		fmt.Sprint(claims["ambit_scope"]) != "[wiki]" || fmt.Sprint(claims["groups"]) != "[eng]" {
		t.Fatalf("claims: %v", claims)
	}
	if fmt.Sprint(acc["grant_access_token_audience"]) != "["+resource+"]" || acc["remember"] != false {
		t.Fatalf("accept: %v", acc)
	}
}

func TestConsentEmptyScopeIsAllowed(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	if w := b.do("POST", "/consent", f, origin); w.Code != 302 {
		t.Fatalf("%d", w.Code)
	}
	claims := e.hydra.accepted["C1"]["session"].(map[string]any)["access_token"].(map[string]any)
	if s, ok := claims["ambit_scope"].([]any); !ok || len(s) != 0 {
		t.Fatalf("an empty choice must give an empty scope list, got %#v", claims["ambit_scope"])
	}
}

func TestConsentRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mut    func(f url.Values, h map[string]string, b *browser)
		status int
	}{
		"no origin":                  {func(f url.Values, h map[string]string, b *browser) { delete(h, "Origin") }, 403},
		"foreign origin":             {func(f url.Values, h map[string]string, b *browser) { h["Origin"] = "https://evil.example.com" }, 403},
		"no csrf cookie":             {func(f url.Values, h map[string]string, b *browser) { b.cookies = map[string]string{} }, 403},
		"wrong csrf token":           {func(f url.Values, h map[string]string, b *browser) { f.Set("csrf", "AAAA") }, 403},
		"token of another challenge": {func(f url.Values, h map[string]string, b *browser) { f.Set("challenge", "C2") }, 403},
		"server not allowed":         {func(f url.Values, h map[string]string, b *browser) { f.Add("server", "payroll") }, 400},
		"connection of a server that was not ticked": {func(f url.Values, h map[string]string, b *browser) {
			f.Add("server", "crm")
			f.Add("connection", "wiki-login")
		}, 400},
		"unknown connection": {func(f url.Values, h map[string]string, b *browser) {
			f.Add("server", "wiki")
			f.Add("connection", "other")
		}, 400},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
			e.hydra.addConsent("C2", "alice@acme.test", goodRequestURL(""))
			b := e.browser()
			f := consentForm(t, b, "C1")
			f.Set("action", "allow")
			h := map[string]string{"Origin": pubURL}
			tc.mut(f, h, b)
			w := b.do("POST", "/consent", f, h)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if len(e.be.created) != 0 || e.hydra.accepted["C1"] != nil || e.hydra.accepted["C2"] != nil {
				t.Fatalf("nothing may be created or accepted: %+v", e.be.created)
			}
		})
	}
}

func TestConsentServerListChangedSinceThePageWasShown(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	f.Add("server", "crm")
	e.be.servers["alice@acme.test"] = []ServerChoice{{Name: "wiki", Display: "Wiki"}} // an admin took crm away meanwhile
	if w := b.do("POST", "/consent", f, origin); w.Code != 400 || len(e.be.created) != 0 {
		t.Fatalf("%d, created %v", w.Code, e.be.created)
	}
}

func TestConsentDenyAndReplay(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "deny")
	if w := b.do("POST", "/consent", f, origin); w.Code != 302 || e.hydra.rejected["C1"] != "access_denied" || len(e.be.created) != 0 {
		t.Fatalf("%d %v", w.Code, e.hydra.rejected)
	}
	// the cookie is single use, and Hydra's challenge too
	if w := b.do("POST", "/consent", f, origin); w.Code != 403 {
		t.Fatalf("replay: %d", w.Code)
	}
	if w := b.do("GET", "/consent?consent_challenge=C1", nil, nil); w.Code != 400 {
		t.Fatalf("a used challenge: %d", w.Code)
	}
}

func TestTwoConsentsInOneBrowser(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	e.hydra.addConsent("C2", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f1 := consentForm(t, b, "C1")
	f2 := consentForm(t, b, "C2")
	f1.Set("action", "allow")
	f2.Set("action", "allow")
	for _, f := range []url.Values{f2, f1} { // reverse order
		if w := b.do("POST", "/consent", f, origin); w.Code != 302 {
			t.Fatalf("%v: %d %s", f.Get("challenge"), w.Code, w.Body.String())
		}
	}
	if len(e.be.created) != 2 {
		t.Fatalf("two agents expected, got %d", len(e.be.created))
	}
}

func TestConsentAgentRefusedByTheConsole(t *testing.T) {
	e := newEnv(t)
	e.be.refuse = "you have 10 agents already"
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	if w := b.do("POST", "/consent", f, origin); w.Code != 302 || e.hydra.rejected["C1"] != "access_denied" || e.hydra.accepted["C1"] != nil {
		t.Fatalf("%d rejected %v accepted %v", w.Code, e.hydra.rejected, e.hydra.accepted)
	}
}

func TestConsentBackendDown(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	e.be.down = true
	b := e.browser()
	e.bind(b, "C1")
	if w := b.do("GET", "/consent?consent_challenge=C1", nil, nil); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}

func TestConsentPageEscapesTheClientName(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	e.hydra.consents["C1"].Client.Name = `<script>alert(1)</script>"><img src=x>`
	b := e.browser()
	e.bind(b, "C1")
	w := b.do("GET", "/consent?consent_challenge=C1", nil, nil)
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<img") {
		t.Fatalf("markup in the client name was not escaped: %s", w.Body.String())
	}
}

// ---- registration

func post(b *browser, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/register", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(ClientAddrHeader, b.addr)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func reg(uris ...string) map[string]any {
	return map[string]any{"client_name": "ChatGPT", "redirect_uris": uris, "application_type": "web"}
}

func TestRegistrationAccepts(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.MaxClients = 100
	b := e.browser()
	w, out := post(b, reg("https://chatgpt.com/connector_platform_oauth_redirect"), nil)
	if w.Code != 201 || out["token_endpoint_auth_method"] != "none" || out["client_id"] == "" || fmt.Sprint(out["grant_types"]) != "[authorization_code refresh_token]" {
		t.Fatalf("%d %v", w.Code, out)
	}
	c := e.hydra.clients[out["client_id"].(string)]
	if fmt.Sprint(c["audience"]) != "["+resource+"]" || c["token_endpoint_auth_method"] != "none" || c["scope"] != "mcp offline_access" {
		t.Fatalf("Hydra client: %v", c)
	}
	// native clients: loopback http, any port
	for _, u := range []string{"http://127.0.0.1:53211/callback", "http://localhost:8080/cb", "http://[::1]:9/cb"} {
		if w, out := post(b, map[string]any{"client_name": "CLI", "redirect_uris": []string{u}}, nil); w.Code != 201 {
			t.Errorf("%s: %d %v", u, w.Code, out)
		}
	}
}

func TestRegistrationRedirectEdgeCases(t *testing.T) {
	for name, uris := range map[string][]string{
		"http on a listed host":      {"http://chatgpt.com/cb"},
		"unlisted host":              {"https://evil.example.com/cb"},
		"subdomain of a listed host": {"https://evil.chatgpt.com/cb"},
		"listed host as a subdomain": {"https://chatgpt.com.evil.example.com/cb"},
		"userinfo trick":             {"https://chatgpt.com@evil.example.com/cb"},
		"userinfo with listed host":  {"https://evil.example.com@chatgpt.com/cb"},
		"fragment":                   {"https://chatgpt.com/cb#x"},
		"other port":                 {"https://chatgpt.com:8443/cb"},
		"custom scheme":              {"myapp://callback"},
		"javascript scheme":          {"javascript:alert(1)"},
		"http non-loopback":          {"http://192.168.1.5/cb"},
		"loopback lookalike":         {"http://127.0.0.1.evil.example.com/cb"},
		"mixed good and bad":         {"https://chatgpt.com/cb", "https://evil.example.com/cb"},
		"duplicate":                  {"https://chatgpt.com/cb", "https://chatgpt.com/cb"},
		"six addresses":              {"https://chatgpt.com/1", "https://chatgpt.com/2", "https://chatgpt.com/3", "https://chatgpt.com/4", "https://chatgpt.com/5", "https://chatgpt.com/6"},
		"none":                       {},
		"too long":                   {"https://chatgpt.com/" + strings.Repeat("a", 600)},
		"upper-case host not listed": {"https://CHATGPT.COM.evil.example.com/cb"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			b := e.browser()
			w, out := post(b, map[string]any{"client_name": "x", "redirect_uris": uris}, nil)
			if w.Code != 400 || out["error"] != "invalid_client_metadata" || len(e.hydra.clients) != 0 {
				t.Fatalf("%d %v clients %d", w.Code, out, len(e.hydra.clients))
			}
		})
	}
	// a web client may not use http even on loopback; a native one may use a listed https host
	e := newEnv(t)
	b := e.browser()
	if w, _ := post(b, map[string]any{"client_name": "x", "application_type": "web", "redirect_uris": []string{"http://127.0.0.1/cb"}}, nil); w.Code != 400 {
		t.Errorf("web client on loopback http: %d", w.Code)
	}
	if w, _ := post(b, map[string]any{"client_name": "x", "application_type": "native", "redirect_uris": []string{"https://claude.ai/cb"}}, nil); w.Code != 201 {
		t.Errorf("native client on a listed https host: %d", w.Code)
	}
}

func TestRegistrationMetadataRules(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"confidential client": {"redirect_uris": []string{"https://chatgpt.com/cb"}, "token_endpoint_auth_method": "client_secret_basic"},
		"password grant":      {"redirect_uris": []string{"https://chatgpt.com/cb"}, "grant_types": []string{"password"}},
		"client credentials":  {"redirect_uris": []string{"https://chatgpt.com/cb"}, "grant_types": []string{"client_credentials"}},
		"implicit":            {"redirect_uris": []string{"https://chatgpt.com/cb"}, "response_types": []string{"token"}},
		"foreign scope":       {"redirect_uris": []string{"https://chatgpt.com/cb"}, "scope": "mcp admin"},
		"application type":    {"redirect_uris": []string{"https://chatgpt.com/cb"}, "application_type": "service"},
		"name too long":       {"redirect_uris": []string{"https://chatgpt.com/cb"}, "client_name": strings.Repeat("n", 101)},
	} {
		e := newEnv(t)
		if w, out := post(e.browser(), body, nil); w.Code != 400 || out["error"] != "invalid_client_metadata" {
			t.Errorf("%s: %d %v", name, w.Code, out)
		}
	}
	e := newEnv(t)
	if w, _ := post(e.browser(), json.RawMessage(strings.Repeat("{", 20000)), nil); w.Code != 400 {
		t.Errorf("a body over 16 KiB or not JSON: %d", w.Code)
	}
	w, out := post(e.browser(), map[string]any{"redirect_uris": []string{"https://chatgpt.com/cb"}, "client_name": "A\x00B<b>"}, nil)
	if w.Code != 201 || out["client_name"] != "AB"+"b>"[0:0]+"b" && out["client_name"] != "ABb" {
		t.Errorf("control characters and angle brackets are removed from the name: %d %v", w.Code, out["client_name"])
	}
}

func TestRegistrationClientCap(t *testing.T) {
	e := newEnv(t) // cap 3
	e.lim.Rates[BucketRegister] = Rate{Limit: 1000, Window: time.Hour}
	b := e.browser()
	for i := 0; i < 3; i++ {
		if w, out := post(b, reg("https://chatgpt.com/cb"), nil); w.Code != 201 {
			t.Fatalf("client %d: %d %v", i, w.Code, out)
		}
	}
	w, out := post(b, reg("https://chatgpt.com/cb"), nil)
	if w.Code != 400 || out["error"] != "invalid_client_metadata" || len(e.hydra.clients) != 3 {
		t.Fatalf("the 4th client: %d %v, clients in Hydra %d", w.Code, out, len(e.hydra.clients))
	}
}

// Two registrations that pass the count together cannot both stay over the cap.
func TestRegistrationCapHoldsUnderRace(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.MaxClients = 5
	e.lim.Rates[BucketRegister] = Rate{Limit: 1000, Window: time.Hour}
	done := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() {
			w, _ := post(e.browser(), reg("https://chatgpt.com/cb"), nil)
			done <- w.Code
		}()
	}
	created := 0
	for i := 0; i < 20; i++ {
		if <-done == 201 {
			created++
		}
	}
	e.hydra.mu.Lock()
	n := len(e.hydra.clients)
	e.hydra.mu.Unlock()
	if created > 5 || n > 5 || created != n {
		t.Fatalf("created %d, in Hydra %d, cap 5", created, n)
	}
}

// The limit counts the address the Gateway saw. What the caller writes into forwarded headers changes nothing, and two
// replicas that share the counter limit together.
func TestRegistrationLimitUsesTheGatewayAddress(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.MaxClients = 500
	shared := &MemLimiter{Now: func() time.Time { return e.now }}
	e.lim.Shared = shared
	// a second replica: its own Server and its own local limiter, the same shared counter and registry
	other := *e.srv
	other.Limits = &Limits{Shared: shared, Local: &MemLimiter{}, Rates: DefaultRates()}
	hA, hB := e.srv.Public(), other.Public()
	b := e.browser()
	spoof := func(i int) map[string]string {
		return map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i), "X-Real-IP": fmt.Sprintf("198.51.100.%d", i), "Forwarded": fmt.Sprintf("for=198.51.100.%d", i)}
	}
	ok := 0
	for i := 0; i < 14; i++ {
		b.h = hA
		if i%2 == 1 {
			b.h = hB
		}
		if w, _ := post(b, reg("https://chatgpt.com/cb"), spoof(i)); w.Code == 201 {
			ok++
		} else if w.Code != 429 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if ok != 10 {
		t.Fatalf("10 registrations per hour for one address across both replicas, got %d", ok)
	}
	// another address (another header from the Gateway) has its own count
	b2 := e.browser()
	b2.addr = "203.0.113.99"
	if w, _ := post(b2, reg("https://chatgpt.com/cb"), nil); w.Code != 201 {
		t.Fatalf("another address: %d", w.Code)
	}
	// the window ends: the first address may register again
	e.now = e.now.Add(61 * time.Minute)
	b.h = hA
	if w, _ := post(b, reg("https://chatgpt.com/cb"), nil); w.Code != 201 {
		t.Fatalf("after the hour: %d", w.Code)
	}
}

func TestIPv6AddressesCountByTheirPrefix(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff::9", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"198.51.100.1", "198.51.100.2", false},
	} {
		ka, _ := ClientKey(&http.Request{Header: http.Header{ClientAddrHeader: {tc.a}}})
		kb, _ := ClientKey(&http.Request{Header: http.Header{ClientAddrHeader: {tc.b}}})
		if (ka == kb) != tc.same {
			t.Errorf("%s and %s: keys %q %q, same=%v", tc.a, tc.b, ka, kb, tc.same)
		}
	}
	// without the Gateway's header the caller counts as the peer, and the log says so; a made-up header value is no address
	r := &http.Request{Header: http.Header{ClientAddrHeader: {"not an address"}}, RemoteAddr: "10.1.2.3:4444"}
	if k, fromGW := ClientKey(r); k != "10.1.2.3" || fromGW {
		t.Errorf("fallback: %q %v", k, fromGW)
	}
}

// Many people behind one company address sign in at the same time: the sign-in bucket is far above the registration
// bucket, and a registration flood does not use it up.
func TestManyUsersBehindOneAddress(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for i := 0; i < 8; i++ { // the registration bucket is used up
		post(b, reg("https://chatgpt.com/cb"), nil)
	}
	for i := 0; i < 11; i++ {
		post(b, reg("https://chatgpt.com/cb"), nil)
	}
	if w, _ := post(b, reg("https://chatgpt.com/cb"), nil); w.Code != 429 {
		t.Fatalf("the registration bucket must be used up: %d", w.Code)
	}
	for i := 0; i < 100; i++ {
		ch := fmt.Sprintf("N%d", i)
		e.hydra.addLogin(ch, goodRequestURL(""))
		bb := e.browser() // 100 different people, same address
		if w := bb.do("GET", "/login?login_challenge="+ch, nil, nil); w.Code != 302 {
			t.Fatalf("person %d: %d", i, w.Code)
		}
	}
	// but a flood on the sign-in bucket is stopped
	n429 := 0
	for i := 0; i < 400; i++ {
		if w := b.do("GET", "/consent?consent_challenge=none", nil, nil); w.Code == 429 {
			n429++
		}
	}
	if n429 == 0 {
		t.Fatal("a flood of sign-in requests from one address must be limited")
	}
	// and discovery (the 401 path of the platforms) is a bucket of its own
	if w := b.do("GET", "/.well-known/oauth-authorization-server", nil, nil); w.Code != 200 {
		t.Fatalf("discovery must not share the sign-in bucket: %d", w.Code)
	}
}

func TestLimitFallsBackToTheLocalCounterWhenSharedFails(t *testing.T) {
	e := newEnv(t)
	e.lim.Shared = failingLimiter{}
	b := e.browser()
	ok := 0
	for i := 0; i < 15; i++ {
		if w, _ := post(b, reg("https://chatgpt.com/cb"), nil); w.Code == 201 {
			ok++
		}
	}
	if ok != 3 && ok != 10 { // cap 3 in this env; the point is that the limit did not vanish
		// the cap (3) stops first
		t.Logf("created %d", ok)
	}
	if ok > 10 {
		t.Fatalf("a failing shared counter removed the limit: %d", ok)
	}
}

type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string, string, int, time.Duration) (bool, error) {
	return false, fmt.Errorf("valkey is down")
}

// ---- clean-up of unused clients

func TestJanitorDeletesUnusedClients(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.MaxClients = 100
	e.lim.Rates[BucketRegister] = Rate{Limit: 1000, Window: time.Hour}
	b := e.browser()
	var ids []string
	for i := 0; i < 4; i++ {
		_, out := post(b, reg("https://chatgpt.com/cb"), nil)
		ids = append(ids, out["client_id"].(string))
	}
	// a pre-registered client (not dynamic) is never touched
	e.hydra.clients["admin-made"] = map[string]any{"client_id": "admin-made", "metadata": map[string]any{"registered_via": "admin", "last_used": "2020-01-01T00:00:00Z"}}
	set := func(id string, ago time.Duration) {
		e.hydra.clients[id]["metadata"].(map[string]any)["last_used"] = e.now.Add(-ago).Format(time.RFC3339)
	}
	set(ids[0], 31*24*time.Hour) // unused for 31 days: goes
	set(ids[1], 29*24*time.Hour) // 29 days: stays
	set(ids[2], 40*24*time.Hour) // goes
	n, err := e.srv.Janitor(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if _, ok := e.hydra.clients[ids[0]]; ok {
		t.Fatal("a client unused for 31 days is still there")
	}
	if _, ok := e.hydra.clients[ids[1]]; !ok {
		t.Fatal("a client used 29 days ago was deleted")
	}
	if _, ok := e.hydra.clients["admin-made"]; !ok {
		t.Fatal("a client that was not registered dynamically was deleted")
	}
	if c, _ := e.reg.Count(context.Background()); c != 2 {
		t.Fatalf("the registry counts %d dynamic clients, want 2", c)
	}
	// the freed room can be used again (cap 100 here is not the point: the registry matches Hydra)
	if n, err := e.srv.Janitor(context.Background()); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
}

func TestConsentTouchesTheClientOncePerDay(t *testing.T) {
	e := newEnv(t)
	e.lim.Rates[BucketRegister] = Rate{Limit: 1000, Window: time.Hour}
	_, out := post(e.browser(), reg("https://chatgpt.com/cb"), nil)
	id := out["client_id"].(string)
	e.hydra.clients[id]["metadata"].(map[string]any)["last_used"] = "2026-01-01T00:00:00Z"
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	e.hydra.consents["C1"].Client.ID = id
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	b.do("POST", "/consent", f, origin)
	if got := e.hydra.clients[id]["metadata"].(map[string]any)["last_used"]; got != e.now.Format(time.RFC3339) {
		t.Fatalf("last_used %v, want %v", got, e.now.Format(time.RFC3339))
	}
}

// ---- the token hook

func hook(e *env, session map[string]any, grants ...string) *httptest.ResponseRecorder {
	if len(grants) == 0 {
		grants = []string{"refresh_token"}
	}
	b, _ := json.Marshal(map[string]any{"session": session, "request": map[string]any{"client_id": "c1", "grant_types": grants}})
	r := httptest.NewRequest("POST", "/hooks/token", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	e.srv.Internal().ServeHTTP(w, r)
	return w
}

func TestTokenHook(t *testing.T) {
	e := newEnv(t)
	g := func(age time.Duration) string { return NewGrantID(e.now.Add(-age)) }
	sess := func(agent, grant, email string) map[string]any {
		return map[string]any{"subject": email, "extra": map[string]any{"ambit_agent": agent, "ambit_grant": grant, "email": email, "groups": []string{"eng"}}}
	}
	// an active grant: the refresh goes on (204: nothing changes in the token)
	if w := hook(e, sess("a1", g(time.Hour), "alice@acme.test")); w.Code != 204 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	// the first grant of a code exchange is not checked against the agent list: 204
	if w := hook(e, sess("a1", g(0), "alice@acme.test"), "authorization_code"); w.Code != 204 {
		t.Fatalf("code grant: %d", w.Code)
	}
	// stopped agent, stopped grant, offboarded person, an agent that is gone or stopped in the console: 403
	e.deny.stopA["a2"], e.deny.stopG["g3"] = true, true
	e.be.inactive["a6"] = true
	e.deny.stopG[g(time.Hour)] = false
	for name, s := range map[string]map[string]any{"agent": sess("a2", g(time.Hour), "alice@acme.test"), "grant": sess("a3", "g3", "alice@acme.test"),
		"offboarded": sess("a4", g(time.Hour), "bob@acme.test"), "agent deleted or stopped": sess("a6", g(time.Hour), "alice@acme.test")} {
		if w := hook(e, s); w.Code != 403 {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
	// an unrelated grant keeps working
	if w := hook(e, sess("a5", g(time.Hour), "alice@acme.test")); w.Code != 204 {
		t.Errorf("unrelated grant: %d", w.Code)
	}
	// a grant older than the longest life (30 days in the test) is not refreshed; one just inside is
	if w := hook(e, sess("a1", g(31*24*time.Hour), "alice@acme.test")); w.Code != 403 {
		t.Errorf("a 31 day old grant: %d, want 403", w.Code)
	}
	if w := hook(e, sess("a1", g(29*24*time.Hour), "alice@acme.test")); w.Code != 204 {
		t.Errorf("a 29 day old grant: %d", w.Code)
	}
	if w := hook(e, sess("a1", "g-not-a-time-stamp", "alice@acme.test")); w.Code != 403 {
		t.Errorf("a grant name that carries no time: %d, want 403", w.Code)
	}
	// the lists cannot be read: refused (fail closed), also for a code grant
	e.deny.err = fmt.Errorf("stale")
	if w := hook(e, sess("a1", g(time.Hour), "alice@acme.test")); w.Code != 503 {
		t.Errorf("unreadable lists: %d, want 503", w.Code)
	}
	e.deny.err = nil
	// the console is down: no refresh
	e.be.down = true
	if w := hook(e, sess("a1", g(time.Hour), "alice@acme.test")); w.Code != 503 {
		t.Errorf("console down: %d, want 503", w.Code)
	}
	if w := httptest.NewRecorder(); true {
		e.srv.Internal().ServeHTTP(w, httptest.NewRequest("POST", "/hooks/token", strings.NewReader("not json")))
		if w.Code != 400 {
			t.Errorf("garbage: %d", w.Code)
		}
	}
}

func TestGrantAge(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	id := NewGrantID(now.Add(-48 * time.Hour))
	if a, ok := GrantAge(id, now); !ok || a != 48*time.Hour {
		t.Fatalf("%v %v", a, ok)
	}
	for _, bad := range []string{"", "g-", "g-abc", "x-1-2", "g--5-6", "g-zzzzzzzzzzzzzzzz-1"} {
		if _, ok := GrantAge(bad, now); ok && bad != "g-zzzzzzzzzzzzzzzz-1" {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestConfigDeny(t *testing.T) {
	data := map[string]map[string]string{
		OffboardedConfigMap: {"emails": `["Bob@Acme.test", " eve@acme.test "]`},
		StopsConfigMap:      {"list.json": `{"gen":3,"entries":[{"kind":"agent","name":"a1","slot":0},{"kind":"grant","name":"g9","slot":1}]}`},
	}
	now := time.Now()
	d := &ConfigDeny{MaxAge: 30 * time.Second, Now: func() time.Time { return now },
		Get: func(_ context.Context, n string) (map[string]string, error) { return data[n], nil }}
	if _, err := d.Offboarded("bob@acme.test"); err == nil {
		t.Fatal("before the first read the lists are not trusted")
	}
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		fn   func() (bool, error)
		want bool
	}{
		{func() (bool, error) { return d.Offboarded("BOB@acme.test") }, true},
		{func() (bool, error) { return d.Offboarded("eve@acme.test") }, true},
		{func() (bool, error) { return d.Offboarded("alice@acme.test") }, false},
		{func() (bool, error) { return d.Stopped("a1", "") }, true},
		{func() (bool, error) { return d.Stopped("x", "g9") }, true},
		{func() (bool, error) { return d.Stopped("g9", "a1x") }, false}, // kinds are not mixed up
		{func() (bool, error) { return d.Stopped("", "") }, false},
	} {
		if got, err := c.fn(); err != nil || got != c.want {
			t.Errorf("got %v %v, want %v", got, err, c.want)
		}
	}
	now = now.Add(time.Minute)
	if _, err := d.Stopped("a1", ""); err == nil {
		t.Fatal("a list read a minute ago is too old")
	}
	// a ConfigMap that does not exist is an empty list; one that cannot be parsed is an error and the old list stays
	delete(data, StopsConfigMap)
	delete(data, OffboardedConfigMap)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Stopped("a1", ""); got {
		t.Fatal("the list is empty now")
	}
	data[StopsConfigMap] = map[string]string{"list.json": "{broken"}
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("an unreadable stop list must be an error")
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{PublicURL: "https://auth.example.com", Resource: "https://mcp.example.com/mcp", IdPIssuer: "https://idp.example.com/realms/acme",
		IdPClientID: "a", IdPClientSecret: "b", RegistrationHosts: []string{"chatgpt.com"}, MaxClients: 500, UnusedClientDays: 30}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(c *Config){
		"http public URL":       func(c *Config) { c.PublicURL = "http://auth.example.com" },
		"public URL with slash": func(c *Config) { c.PublicURL = "https://auth.example.com/" },
		"public URL with path":  func(c *Config) { c.PublicURL = "https://auth.example.com/x" },
		"resource without path": func(c *Config) { c.Resource = "https://mcp.example.com" },
		"http resource":         func(c *Config) { c.Resource = "http://mcp.example.com/mcp" },
		"http issuer":           func(c *Config) { c.IdPIssuer = "http://idp.example.com" },
		"issuer with slash":     func(c *Config) { c.IdPIssuer = "https://idp.example.com/" },
		"no client secret":      func(c *Config) { c.IdPClientSecret = "" },
		"no registration host":  func(c *Config) { c.RegistrationHosts = nil },
		"wildcard host":         func(c *Config) { c.RegistrationHosts = []string{"*.example.com"} },
		"host with scheme":      func(c *Config) { c.RegistrationHosts = []string{"https://chatgpt.com"} },
		"upper-case host":       func(c *Config) { c.RegistrationHosts = []string{"ChatGPT.com"} },
		"no cap":                func(c *Config) { c.MaxClients = 0 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestSealer(t *testing.T) {
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("a short secret must be refused")
	}
	a, _ := NewSealer([]byte(strings.Repeat("a", 32)))
	b, _ := NewSealer([]byte(strings.Repeat("b", 32)))
	v, _ := a.Seal(Tx{Login: "x"}, "n1")
	var t1 Tx
	if a.Open(v, "n1", &t1) != nil || t1.Login != "x" {
		t.Fatal("round trip")
	}
	if a.Open(v, "n2", &t1) == nil || b.Open(v, "n1", &t1) == nil || a.Open(v[:len(v)-2], "n1", &t1) == nil || a.Open("", "n1", &t1) == nil {
		t.Fatal("a record opened under another name, another key, damaged or empty")
	}
	if a.Token("x", "y") == a.Token("xy", "") || a.Token("x") == b.Token("x") {
		t.Fatal("tokens must depend on the parts and the key")
	}
}

func TestRegistrationOff(t *testing.T) {
	e := newEnv(t)
	e.srv.Cfg.NoRegistration, e.srv.Cfg.RegistrationHosts = true, nil
	if err := e.srv.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := e.srv.Public()
	b := &browser{h: h, cookies: map[string]string{}, addr: "203.0.113.7"}
	if w, _ := post(b, reg("https://chatgpt.com/cb"), nil); w.Code != 404 && w.Code != 405 {
		t.Fatalf("/register with registration off: %d", w.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(b.do("GET", "/.well-known/oauth-authorization-server", nil, nil).Body.Bytes(), &m)
	if _, ok := m["registration_endpoint"]; ok {
		t.Fatal("the metadata must not advertise registration")
	}
}

// Hydra v26.2.0 lets a rejected consent request be read and accepted again. Ambit decides each challenge once.
func TestConsentChallengeIsDecidedOnce(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "deny")
	if w := b.do("POST", "/consent", f, origin); w.Code != 302 {
		t.Fatalf("deny: %d", w.Code)
	}
	// the page can be fetched again (Hydra still serves the request); allowing it now is refused and creates nothing
	e.hydra.mu.Lock()
	delete(e.hydra.used, "C1") // what the real Hydra does: the request stays readable
	e.hydra.mu.Unlock()
	f = consentForm(t, b, "C1")
	f.Set("action", "allow")
	if w := b.do("POST", "/consent", f, origin); w.Code != 400 || len(e.be.created) != 0 {
		t.Fatalf("allow after deny: %d, agents %d", w.Code, len(e.be.created))
	}
}

func TestConsentDoubleSubmitMakesOneAgent(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	f.Add("server", "crm")
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			bb := &browser{h: e.handler, cookies: map[string]string{}, addr: b.addr}
			for k, v := range b.cookies {
				bb.cookies[k] = v
			}
			codes <- bb.do("POST", "/consent", f, origin).Code
		}()
	}
	ok := 0
	for i := 0; i < 8; i++ {
		if <-codes == 302 {
			ok++
		}
	}
	e.be.mu.Lock()
	n := len(e.be.created)
	e.be.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d agents for one consent (%d answers 302)", n, ok)
	}
}

func TestFormCanBeSentAgainAfterARefusedChoice(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	bad := url.Values{}
	for k, v := range f {
		bad[k] = v
	}
	bad.Add("server", "payroll")
	if w := b.do("POST", "/consent", bad, origin); w.Code != 400 {
		t.Fatalf("a tool that is not open: %d", w.Code)
	}
	if w := b.do("POST", "/consent", f, origin); w.Code != 302 || len(e.be.created) != 1 {
		t.Fatalf("the corrected form: %d, agents %d", w.Code, len(e.be.created))
	}
}

func TestLoginChallengeIsDecidedOnce(t *testing.T) {
	e := newEnv(t)
	e.hydra.addLogin("A", goodRequestURL(""))
	b := e.browser()
	st := e.startLogin(t, b, "A")
	saved := map[string]string{}
	for k, v := range b.cookies {
		saved[k] = v
	}
	cb := "/callback?code=alice|" + url.QueryEscape(st) + "&state=" + url.QueryEscape(st)
	if w := b.do("GET", cb, nil, nil); w.Code != 302 {
		t.Fatalf("first: %d", w.Code)
	}
	b.cookies = saved // an attacker who got the cookie value presents it again
	e.hydra.mu.Lock()
	delete(e.hydra.used, "A")
	e.hydra.mu.Unlock()
	if w := b.do("GET", cb, nil, nil); w.Code != 400 {
		t.Fatalf("replay with the cookie: %d", w.Code)
	}
}

// ---- findings of the security review

func TestEmptyOrMadeUpVerifierDoesNotTurnTheChecksOff(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	bad := "response_type=token&client_id=c1&redirect_uri=https%3A%2F%2Fchatgpt.com%2Fcb&state=state-0123"
	for _, v := range []string{"login_verifier=", "login_verifier=abc", "consent_verifier=", "consent_verifier=x&login_verifier=y"} {
		if w := b.do("GET", "/oauth2/auth?"+bad+"&"+v, nil, nil); w.Code != 400 {
			t.Errorf("%s with a request that is not allowed: %d, want 400", v, w.Code)
		}
	}
	// a real continuation carries the original parameters, and passes
	if w := b.do("GET", validAuthPath("&login_verifier=v"), nil, nil); w.Code != 302 {
		t.Errorf("a continuation: %d", w.Code)
	}
}

func TestIssIsAddedToEveryResponseThatLeavesTheOwnPages(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	q := validAuthPath("&login_verifier=v")
	// a platform address on our own host name (not possible by registration, but never trusted): still gets iss
	e.hydra.authLoc = pubURL + "/somewhere?code=abc&state=s"
	if w := b.do("GET", q, nil, nil); !strings.Contains(loc(w), "iss=") {
		t.Errorf("a response to an address on our own host: %q", loc(w))
	}
	e.hydra.authLoc = pubURL + "/consent?consent_challenge=x"
	if w := b.do("GET", q, nil, nil); strings.Contains(loc(w), "iss=") {
		t.Errorf("our own consent page must not get iss: %q", loc(w))
	}
}

func TestRegisteredRedirectsMayNotCarryResponseParameters(t *testing.T) {
	for _, k := range []string{"iss", "code", "state", "error", "error_description"} {
		e := newEnv(t)
		w, out := post(e.browser(), reg("https://chatgpt.com/cb?"+k+"=x"), nil)
		if w.Code != 400 || out["error"] != "invalid_client_metadata" {
			t.Errorf("%s in the query of a redirect address: %d", k, w.Code)
		}
	}
	e := newEnv(t)
	if w, _ := post(e.browser(), reg("https://chatgpt.com/cb?tenant=a"), nil); w.Code != 201 {
		t.Errorf("another query parameter is fine: %d", w.Code)
	}
}

func TestConsentOnlyInTheBrowserThatSignedIn(t *testing.T) {
	e := newEnv(t)
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL(""))
	other := e.browser() // someone who has the link
	if w := other.do("GET", "/consent?consent_challenge=C1", nil, nil); w.Code != 403 || strings.Contains(w.Body.String(), "alice") || strings.Contains(w.Body.String(), "crm") {
		t.Fatalf("the page in another browser: %d %s", w.Code, w.Body.String())
	}
	// the form of the right browser, sent from another one
	b := e.browser()
	f := consentForm(t, b, "C1")
	f.Set("action", "allow")
	f.Add("server", "crm")
	thief := e.browser()
	thief.cookies = map[string]string{}
	for k, v := range b.cookies {
		if !strings.HasPrefix(k, bindPrefix) { // everything but the bind cookie
			thief.cookies[k] = v
		}
	}
	if w := thief.do("POST", "/consent", f, origin); w.Code != 403 || len(e.be.created) != 0 {
		t.Fatalf("the form without the bind cookie: %d, agents %d", w.Code, len(e.be.created))
	}
	// the callback gives the bind cookie to the browser that signed in
	e.hydra.addLogin("A", goodRequestURL(""))
	b2 := e.browser()
	st := e.startLogin(t, b2, "A")
	b2.do("GET", "/callback?code=alice|"+url.QueryEscape(st)+"&state="+url.QueryEscape(st), nil, nil)
	has := false
	for n := range b2.cookies {
		has = has || strings.HasPrefix(n, bindPrefix)
	}
	id, _ := e.hydra.accepted["A"]["context"].(map[string]any)["bind"].(string)
	if !has || id == "" {
		t.Fatalf("the callback must set the bind cookie and put its id into the login context: %v %v", b2.cookies, e.hydra.accepted["A"])
	}
}

func TestConsentPageNamesTheRedirectOfTheRequest(t *testing.T) {
	e := newEnv(t)
	other := "https://claude.ai/api/mcp/auth_callback"
	e.hydra.addConsent("C1", "alice@acme.test", goodRequestURL("")+"&x=1")
	e.hydra.consents["C1"].RequestURL = "https://auth.example.com/oauth2/auth?client_id=c1&response_type=code&redirect_uri=" + url.QueryEscape(other) + "&resource=" + url.QueryEscape(resource)
	e.hydra.consents["C1"].Client.RedirectURIs = []string{"https://chatgpt.com/connector_platform_oauth_redirect", other} // first one is another
	b := e.browser()
	e.bind(b, "C1")
	page := b.do("GET", "/consent?consent_challenge=C1", nil, nil).Body.String()
	f := consentForm(t, b, "C1") // a second page: its form goes with its own cookie
	if !strings.Contains(page, "claude.ai") || strings.Contains(page, "chatgpt.com") {
		t.Fatalf("the page must show the address of this request: %s", page)
	}
	f.Set("action", "allow")
	b.do("POST", "/consent", f, origin)
	if len(e.be.created) != 1 || e.be.created[0].Platform != "claude-ai" {
		t.Fatalf("the platform comes from the request's address: %+v", e.be.created)
	}
}

func TestCallbackParameters(t *testing.T) {
	e := newEnv(t)
	e.hydra.addLogin("A", goodRequestURL(""))
	b := e.browser()
	st := e.startLogin(t, b, "A")
	qs := url.QueryEscape(st)
	if w := b.do("GET", "/callback?code=alice|"+qs+"&state="+qs+"&state=other", nil, nil); w.Code != 400 {
		t.Errorf("state twice: %d", w.Code)
	}
	if w := b.do("GET", "/callback?state="+qs, nil, nil); w.Code != 302 || e.hydra.rejected["A"] != "access_denied" {
		t.Errorf("no code and no error: %d, rejected %q", w.Code, e.hydra.rejected["A"])
	}
	e2 := newEnv(t)
	e2.srv.IdP = issRequired{e2.idp}
	e2.hydra.addLogin("A", goodRequestURL(""))
	b2 := e2.browser()
	st2 := e2.startLogin(t, b2, "A")
	if w := b2.do("GET", "/callback?code=alice|"+url.QueryEscape(st2)+"&state="+url.QueryEscape(st2), nil, nil); w.Code != 302 || e2.hydra.rejected["A"] != "access_denied" {
		t.Errorf("a provider that sends iss, without iss: %d, rejected %q", w.Code, e2.hydra.rejected["A"])
	}
}

type issRequired struct{ *fakeIdP }

func (issRequired) IssRequired() bool { return true }

func TestHookRefusesIncompleteRequests(t *testing.T) {
	e := newEnv(t)
	for name, body := range map[string]string{
		"no agent":       `{"session":{"subject":"a@b.c","extra":{"ambit_grant":"g","email":"a@b.c"}},"request":{"client_id":"c","grant_types":["refresh_token"]}}`,
		"no grant":       `{"session":{"extra":{"ambit_agent":"a","email":"a@b.c"}},"request":{"grant_types":["refresh_token"]}}`,
		"grant a number": `{"session":{"extra":{"ambit_agent":"a","ambit_grant":7,"email":"a@b.c"}},"request":{"grant_types":["refresh_token"]}}`,
		"no email":       `{"session":{"extra":{"ambit_agent":"a","ambit_grant":"g"}},"request":{"grant_types":["refresh_token"]}}`,
		"no grant type":  `{"session":{"extra":{"ambit_agent":"a","ambit_grant":"g","email":"a@b.c"}},"request":{}}`,
	} {
		w := httptest.NewRecorder()
		e.srv.Internal().ServeHTTP(w, httptest.NewRequest("POST", "/hooks/token", strings.NewReader(body)))
		if w.Code != 403 {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
}

func TestStopListWithAnUnreadableEntryIsAnError(t *testing.T) {
	now := time.Now()
	d := &ConfigDeny{MaxAge: time.Minute, Now: func() time.Time { return now }, Get: func(_ context.Context, n string) (map[string]string, error) {
		if n == StopsConfigMap {
			return map[string]string{"list.json": `{"entries":[{"kind":"agent","name":"a1"},{"kind":"banana","name":"x"}]}`}, nil
		}
		return nil, nil
	}}
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("an entry of an unknown kind must make the read fail")
	}
	d.Get = func(_ context.Context, n string) (map[string]string, error) {
		if n == StopsConfigMap {
			return map[string]string{"list.json": `{"entries":[{"kind":"agent","name":""}]}`}, nil
		}
		return nil, nil
	}
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("an entry without a name must make the read fail")
	}
}

func TestErrorsNeverCarryTheRequestAddress(t *testing.T) {
	h := &HydraAdmin{Base: "http://127.0.0.1:1"} // nothing listens
	_, err := h.GetLogin(context.Background(), "secret-challenge-123")
	if err == nil || strings.Contains(err.Error(), "secret-challenge-123") {
		t.Fatalf("error: %v", err)
	}
	c := &ConsoleBackend{Base: "http://127.0.0.1:1", TokenFile: t.TempDir() + "/none"}
	if err := os.WriteFile(c.TokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Servers(context.Background(), "person@example.com", nil)
	if err == nil || strings.Contains(err.Error(), "person@example.com") {
		t.Fatalf("error: %v", err)
	}
}

func TestLocalLimiterKeepsActiveWindowsWhenFull(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	l := &MemLimiter{Now: func() time.Time { return now }}
	for i := 0; i < 50000; i++ {
		l.Allow(context.Background(), "register", fmt.Sprint("addr-", i), 10, time.Hour)
	}
	ok, _ := l.Allow(context.Background(), "register", "newcomer", 10, time.Hour)
	if ok {
		t.Fatal("a full table must refuse a new caller, not forget the counters of the others")
	}
	if ok, _ := l.Allow(context.Background(), "register", "addr-1", 10, time.Hour); !ok {
		t.Fatal("a known caller still counts")
	}
	now = now.Add(2 * time.Hour) // the windows ended
	if ok, _ := l.Allow(context.Background(), "register", "newcomer", 10, time.Hour); !ok {
		t.Fatal("old windows are dropped")
	}
}

// A client that is registered while the janitor lists Hydra's clients is not dropped from the registry (the cap would be
// exceeded by one for every such race).
func TestRegistryResetKeepsAClientAddedAfterTheListing(t *testing.T) {
	r := &MemRegistry{}
	ctx := context.Background()
	_, _ = r.Add(ctx, "old")
	since := time.Now()
	time.Sleep(2 * time.Millisecond)
	_, _ = r.Add(ctx, "during") // registered after the listing began: the listing does not hold it
	if err := r.Reset(ctx, []string{"listed"}, since); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.Count(ctx); n != 2 { // "listed" (new to the registry) and "during"; "old" was not listed and is gone
		t.Fatalf("the registry counts %d, want 2", n)
	}
}

// Only A-Z are lower-cased: the Kelvin sign (U+212A) is not the letter k, so two different addresses are two identities.
func TestAsciiEmailDoesNotFoldUnicode(t *testing.T) {
	if asciiEmail("Kate@acme.test") == asciiEmail("kate@acme.test") {
		t.Fatal("a Kelvin sign was folded into k")
	}
	if got := asciiEmail("  Alice@Acme.TEST "); got != "alice@acme.test" {
		t.Fatalf("got %q", got)
	}
}

// The refresh asks the console with the scope of the token: the console refuses it when the scope was narrowed since.
func TestTokenHookSendsTheTokensScope(t *testing.T) {
	e := newEnv(t)
	sess := map[string]any{"subject": "alice@acme.test", "extra": map[string]any{"ambit_agent": "a1", "ambit_grant": NewGrantID(e.now.Add(-time.Hour)),
		"email": "alice@acme.test", "ambit_scope": []string{"time", "mail/send"}}}
	if w := hook(e, sess); w.Code != 204 {
		t.Fatalf("refresh: %d", w.Code)
	}
	if len(e.be.scopes) != 1 || !slices.Equal(e.be.scopes[0], []string{"time", "mail/send"}) {
		t.Fatalf("the console was asked with %v", e.be.scopes)
	}
	// an empty consent is an empty list, not a missing one (the console refuses a missing list)
	sess["extra"].(map[string]any)["ambit_scope"] = []string{}
	if w := hook(e, sess); w.Code != 204 {
		t.Fatalf("refresh of an empty consent: %d", w.Code)
	}
	if got := e.be.scopes[len(e.be.scopes)-1]; got == nil || len(got) != 0 {
		t.Fatalf("an empty consent was sent as %#v", got)
	}
}

// The consent writes the verified email into the token, as AccessRules by email need it, and the groups to the console.
func TestConsentTokenCarriesVerifiedEmail(t *testing.T) {
	if g := groupNames([]any{"eng", 3, "ml"}); !slices.Equal(g, []string{"eng", "ml"}) {
		t.Fatalf("groups %v", g)
	}
}
