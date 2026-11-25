package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// IDClaims are the facts of a verified ID token that Ambit uses.
type IDClaims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
	Nonce         string
}

// Identity is the company's identity provider. The implementation (OIDC) checks the signature, issuer, audience and
// expiry of the ID token; the server compares the nonce and the rest.
type Identity interface {
	AuthURL(state, nonce, verifier string) string
	// IssRequired: the provider says it sends iss in its authorization responses (RFC 9207), so a response without it is refused.
	IssRequired() bool
	Exchange(ctx context.Context, code, verifier string) (IDClaims, error)
}

// Registry counts the dynamic clients (the cap) in a place all replicas share.
type Registry interface {
	Add(ctx context.Context, id string) (int, error)
	Remove(ctx context.Context, id string) error
	Count(ctx context.Context) (int, error)
	Reset(ctx context.Context, ids []string) error
	// Once reports true the first time it is called for key within ttl (used to write "last used" once a day).
	Once(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Server is ambit-auth: the pages and endpoints of the sign-in host, and Hydra's token hook.
type Server struct {
	Cfg      Config
	Sealer   *Sealer
	Hydra    *HydraAdmin
	HydraPub string // Hydra's public API, for the authorize front
	Backend  Backend
	Deny     Deny
	IdP      Identity
	Limits   *Limits
	Clients  Registry
	Now      func() time.Time
	Log      *slog.Logger

	front *http.Client
}

var (
	mSignins = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_signins_total", Help: "Sign-ins at the company identity provider, by result."}, []string{"result"})
	mConsent = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_consents_total", Help: "Consent decisions, by result."}, []string{"result"})
	mRegs    = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_registrations_total", Help: "Dynamic client registrations, by result."}, []string{"result"})
	mHook    = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_token_hook_total", Help: "Token hook answers, by result."}, []string{"result"})
	mLimited = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_limited_total", Help: "Requests refused by a limit, by bucket."}, []string{"bucket"})
	mFront   = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_auth_authorize_refused_total", Help: "Authorization requests refused before Hydra, by reason."}, []string{"reason"})
)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Public is the handler of port 8080 (reachable from the public Gateway's pods only).
func (s *Server) Public() http.Handler {
	s.front = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	mux := http.NewServeMux()
	mux.Handle("GET /.well-known/oauth-authorization-server", s.limited(BucketDiscovery, s.metadata))
	mux.Handle("GET /oauth2/auth", s.limited(BucketSignIn, s.authorize))
	mux.Handle("GET /login", s.limited(BucketSignIn, s.login))
	mux.Handle("GET /callback", s.limited(BucketSignIn, s.callback))
	mux.Handle("GET /consent", s.limited(BucketSignIn, s.consentPage))
	mux.Handle("POST /consent", s.limited(BucketSignIn, s.consentSubmit))
	mux.Handle("GET /error", s.limited(BucketSignIn, s.errorPage))
	if !s.Cfg.NoRegistration {
		mux.Handle("POST /register", s.limited(BucketRegister, s.register))
	}
	return securityHeaders(mux)
}

// Internal is the handler of port 8081 (Hydra's pods only): the token hook.
func (s *Server) Internal() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/token", s.tokenHook)
	return mux
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.ServeHTTP(w, r)
	})
}

// limited counts the request against the caller's address in a bucket, then runs h.
func (s *Server) limited(bucket string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, fromGW := ClientKey(r)
		if !fromGW {
			s.log().Warn("no client address header from the Gateway: callers are counted together", "bucket", bucket)
		}
		if ok, _ := s.Limits.Allow(r.Context(), bucket, key); !ok {
			mLimited.WithLabelValues(bucket).Inc()
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, e, desc string) {
	writeJSON(w, code, map[string]string{"error": e, "error_description": desc})
}

// metadata is RFC 8414: Ambit's own document, not Hydra's (Hydra's would advertise endpoints that are not routed).
func (s *Server) metadata(w http.ResponseWriter, _ *http.Request) {
	p := s.Cfg.PublicURL
	m := map[string]any{
		"issuer": p, "authorization_endpoint": p + "/oauth2/auth", "token_endpoint": p + "/oauth2/token",
		"revocation_endpoint": p + "/oauth2/revoke", "jwks_uri": p + "/.well-known/jwks.json",
		"response_types_supported": []string{"code"}, "response_modes_supported": []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic"},
		"scopes_supported":                               Scopes,
		"authorization_response_iss_parameter_supported": true,
	}
	if !s.Cfg.NoRegistration {
		m["registration_endpoint"] = p + "/register"
	}
	writeJSON(w, 200, m)
}

var challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// authorize is the front of Hydra's /oauth2/auth. The first request of a sign-in (it has no Hydra verifier) must be
// exactly what Ambit serves: the code flow, PKCE with S256, the query response mode, our one resource, our scopes,
// and no parameter twice. The requests that come back from login and consent carry the same parameters and Hydra's
// verifiers, and are checked the same way.
// The response that leaves our host with a code or an error gets `iss` (RFC 9207).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Every request is checked, also the ones that come back from login and consent: they carry the original parameters
	// again, and an empty or made-up verifier must not turn the check off (Hydra reads such a request as a new one).
	if reason, msg := s.checkAuthorize(q); reason != "" {
		mFront.WithLabelValues(reason).Inc()
		s.log().Info("authorization request refused", "reason", reason)
		oauthError(w, 400, "invalid_request", msg)
		return
	}
	if !q.Has("scope") {
		q.Set("scope", strings.Join(Scopes, " "))
		r.URL.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", s.HydraPub+r.URL.Path+"?"+r.URL.RawQuery, nil)
	if err != nil {
		oauthError(w, 400, "invalid_request", "bad request")
		return
	}
	for _, h := range []string{"Cookie", "User-Agent", "Accept", "Accept-Language"} {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := s.front.Do(req)
	if err != nil {
		s.log().Error("authorize: Hydra did not answer", "err", err)
		oauthError(w, 503, "temporarily_unavailable", "the authorization server is not available")
		return
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if u, err := url.Parse(loc); loc != "" && err == nil && !s.internalStep(u) {
		// A response to the platform (it leaves our own pages): code or error, always with our one iss.
		if u.Query().Has("code") || u.Query().Has("error") {
			if u.Query().Has("iss") { // Hydra added its own: never send two
				s.log().Error("authorize: Hydra sent iss itself")
				oauthError(w, 502, "server_error", "unexpected answer of the authorization server")
				return
			}
			u.RawQuery += "&iss=" + url.QueryEscape(s.Cfg.PublicURL)
			loc = u.String()
		}
	}
	for k, v := range resp.Header {
		if k == "Location" || k == "Content-Length" {
			continue
		}
		w.Header()[k] = v
	}
	if loc != "" {
		w.Header().Set("Location", loc)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// internalStep: the redirect goes to one of our own pages (login, consent, error), not to the platform.
func (s *Server) internalStep(u *url.URL) bool {
	base, err := url.Parse(s.Cfg.PublicURL)
	if err != nil || !strings.EqualFold(u.Host, base.Host) || u.Scheme != "https" {
		return false
	}
	return u.Path == "/login" || u.Path == "/consent" || u.Path == "/error"
}

// checkAuthorize returns a short reason (a metric label) and a message, or "".
func (s *Server) checkAuthorize(q url.Values) (string, string) {
	for k, v := range q {
		if len(v) > 1 && k != "resource" {
			return "repeated_parameter", "the parameter " + k + " is sent more than once"
		}
	}
	if q.Has("request") || q.Has("request_uri") {
		return "request_object", "request objects are not supported"
	}
	switch {
	case q.Get("response_type") != "code":
		return "response_type", "only response_type=code is supported"
	case q.Get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Get("code_challenge")):
		return "pkce", "PKCE with code_challenge_method=S256 is required"
	case q.Has("response_mode") && q.Get("response_mode") != "query":
		return "response_mode", "only response_mode=query is supported"
	case q.Get("client_id") == "" || q.Get("redirect_uri") == "":
		return "client", "client_id and redirect_uri are required"
	}
	if res := q["resource"]; len(res) != 1 || res[0] != s.Cfg.Resource {
		return "resource", "invalid_target: the resource parameter must be given once, and be " + s.Cfg.Resource
	}
	if q.Has("scope") && !inScopes(strings.Fields(q.Get("scope"))) {
		return "scope", "invalid_scope: the scopes are " + strings.Join(Scopes, " ")
	}
	return "", ""
}

// resourceOK checks the resource parameter of the original authorization request, which Hydra keeps in request_url.
func (s *Server) resourceOK(requestURL string) bool {
	u, err := url.Parse(requestURL)
	if err != nil {
		return false
	}
	res := u.Query()["resource"]
	return len(res) == 1 && res[0] == s.Cfg.Resource
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request) {
	// Hydra sends the person here when it cannot send the error to the platform (an unknown client or redirect address).
	// The text is fixed: nothing of the request is repeated.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Ambit sign-in</title><p>The sign-in request cannot be completed. Start again from the platform you are connecting.</p>`)
}

// ---- dynamic client registration (RFC 7591), narrowed

var clientNameBad = func(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' }

type registration struct {
	ClientName      string   `json:"client_name"`
	RedirectURIs    []string `json:"redirect_uris"`
	ApplicationType string   `json:"application_type"`
	GrantTypes      []string `json:"grant_types"`
	ResponseTypes   []string `json:"response_types"`
	AuthMethod      string   `json:"token_endpoint_auth_method"`
	Scope           string   `json:"scope"`
}

func invalidMeta(w http.ResponseWriter, desc string) {
	oauthError(w, 400, "invalid_client_metadata", desc)
}

// checkRedirect applies the registration rules to one redirect URI: https on a named host (any path, port 443 only,
// no user, no fragment) for web clients; http on a loopback name for native clients.
func (s *Server) checkRedirect(raw, appType string) (host string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || len(raw) > 512 || u.Fragment != "" || u.User != nil || u.Host == "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("redirect_uri %q is not acceptable", raw)
	}
	// The answer to the platform adds code, state, error and iss to the address: it must not have them already.
	for _, k := range []string{"code", "state", "error", "error_description", "iss"} {
		if u.Query().Has(k) {
			return "", fmt.Errorf("redirect_uri must not have the query parameter %q", k)
		}
	}
	switch u.Scheme {
	case "https":
		if appType == "native" && !slices.Contains(s.Cfg.RegistrationHosts, u.Hostname()) {
			return "", fmt.Errorf("redirect_uri host %q is not on the list of allowed hosts", u.Hostname())
		}
		if !slices.Contains(s.Cfg.RegistrationHosts, u.Hostname()) || (u.Port() != "" && u.Port() != "443") {
			return "", fmt.Errorf("redirect_uri host %q is not on the list of allowed hosts (or its port is not 443)", u.Hostname())
		}
		return u.Hostname(), nil
	case "http":
		if appType == "web" {
			return "", fmt.Errorf("redirect_uri %q: a web client needs https", raw)
		}
		if h := u.Hostname(); h != "127.0.0.1" && h != "::1" && h != "localhost" {
			return "", fmt.Errorf("redirect_uri %q: http is accepted for loopback addresses only", raw)
		}
		return u.Hostname(), nil
	}
	return "", fmt.Errorf("redirect_uri %q: unsupported scheme", raw)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in registration
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&in); err != nil {
		mRegs.WithLabelValues("bad_json").Inc()
		invalidMeta(w, "the body must be a JSON object of at most 16 KiB")
		return
	}
	in.ClientName = strings.Map(func(r rune) rune {
		if clientNameBad(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(in.ClientName))
	if in.ClientName == "" {
		in.ClientName = "Unnamed client"
	}
	reject := func(reason, desc string) {
		mRegs.WithLabelValues(reason).Inc()
		s.log().Info("registration refused", "reason", reason)
		invalidMeta(w, desc)
	}
	switch {
	case len([]rune(in.ClientName)) > 100:
		reject("name", "client_name is longer than 100 characters")
		return
	case len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5:
		reject("redirect_count", "redirect_uris must list 1 to 5 addresses")
		return
	case in.ApplicationType != "" && in.ApplicationType != "web" && in.ApplicationType != "native":
		reject("application_type", "application_type must be web or native")
		return
	case in.AuthMethod != "" && in.AuthMethod != "none":
		reject("auth_method", "only token_endpoint_auth_method none (a public client with PKCE) is supported")
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			reject("grant_types", "grant_types may be authorization_code and refresh_token only")
			return
		}
	}
	for _, t := range in.ResponseTypes {
		if t != "code" {
			reject("response_types", "response_types may be code only")
			return
		}
	}
	scope := Scopes
	if in.Scope != "" {
		if scope = strings.Fields(in.Scope); !inScopes(scope) {
			reject("scope", "scope may be "+strings.Join(Scopes, " "))
			return
		}
	}
	appType := in.ApplicationType
	seen := map[string]bool{}
	hosts := map[string]bool{}
	for _, ru := range in.RedirectURIs {
		if seen[ru] {
			reject("redirect_duplicate", "redirect_uris lists "+ru+" twice")
			return
		}
		seen[ru] = true
		if appType == "" { // decided by the first address: https is a web client, loopback http a native one
			appType = "web"
			if strings.HasPrefix(ru, "http:") {
				appType = "native"
			}
		}
		h, err := s.checkRedirect(ru, appType)
		if err != nil {
			reject("redirect", err.Error())
			return
		}
		hosts[h] = true
	}
	if n, err := s.Clients.Count(ctx); err != nil || n >= s.Cfg.MaxClients {
		reason := "cap"
		if err != nil {
			reason = "registry"
		}
		mRegs.WithLabelValues(reason).Inc()
		s.log().Warn("registration refused: the client cap is reached or the count is unreadable", "reason", reason, "cap", s.Cfg.MaxClients)
		if err != nil {
			oauthError(w, 503, "temporarily_unavailable", "registration is not available now")
			return
		}
		invalidMeta(w, "the number of registered clients has reached its limit")
		return
	}
	now := s.now().UTC()
	c, err := s.Hydra.CreateClient(ctx, Client{Name: in.ClientName, RedirectURIs: in.RedirectURIs,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseType: []string{"code"}, AuthMethod: "none",
		Scope: strings.Join(scope, " "), Audience: []string{s.Cfg.Resource},
		Metadata: map[string]any{"registered_via": "dcr", "application_type": appType, "last_used": now.Format(time.RFC3339)}})
	if err != nil {
		s.log().Error("registration: Hydra refused", "err", err)
		mRegs.WithLabelValues("hydra").Inc()
		oauthError(w, 503, "temporarily_unavailable", "registration is not available now")
		return
	}
	// The cap is checked again after the add: two registrations that passed the check together cannot both stay.
	if n, err := s.Clients.Add(ctx, c.ID); err != nil || n > s.Cfg.MaxClients {
		_ = s.Hydra.DeleteClient(ctx, c.ID)
		_ = s.Clients.Remove(ctx, c.ID)
		mRegs.WithLabelValues("cap").Inc()
		invalidMeta(w, "the number of registered clients has reached its limit")
		return
	}
	mRegs.WithLabelValues("created").Inc()
	s.log().Info("client registered", "client_id", c.ID, "application_type", appType, "hosts", len(hosts))
	writeJSON(w, 201, map[string]any{"client_id": c.ID, "client_id_issued_at": now.Unix(), "client_name": in.ClientName,
		"redirect_uris": in.RedirectURIs, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none", "scope": strings.Join(scope, " "), "application_type": appType})
}
