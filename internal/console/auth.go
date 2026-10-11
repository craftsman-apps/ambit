package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type AuthConfig struct {
	Issuer string // Dex issuer, e.g. http://console.localtest.me:8080/dex
	// InternalIssuer is Dex's in-cluster URL, e.g. http://ambit-dex:5556/dex. When set, the console reaches
	// Dex there (discovery, code exchange, keys); the browser and the tokens still use Issuer. The public
	// URL may not resolve inside the cluster: on kind it is 127.0.0.1, the pod itself.
	InternalIssuer string
	ClientID       string
	ClientSecret   string
	PublicURL      string // console URL; the callback is PublicURL + /auth/callback
	SessionKey     []byte
	SessionTTL     time.Duration
	Sessions       *SessionStore                        // optional; a new one is created when nil
	OnSignIn       func(ctx context.Context, s Session) // called after a verified sign-in; errors are its own to log
}

type Auth struct {
	Sessions *SessionStore
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	codec    Codec
	ttl      time.Duration
	secure   bool
	onSignIn func(ctx context.Context, s Session)
}

// loginTx is kept in a short-lived signed cookie between /auth/login and /auth/callback.
type loginTx struct {
	State, Nonce, Verifier string
	Exp                    int64
}

func NewAuth(ctx context.Context, c AuthConfig) (*Auth, error) {
	p, err := provider(ctx, c.Issuer, c.InternalIssuer)
	if err != nil {
		return nil, err
	}
	store := c.Sessions
	if store == nil {
		store = NewSessionStore()
	}
	return &Auth{
		Sessions: store,
		oauth: oauth2.Config{
			ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: p.Endpoint(),
			RedirectURL: strings.TrimSuffix(c.PublicURL, "/") + "/auth/callback",
			Scopes:      []string{oidc.ScopeOpenID, "email", "profile", "groups", "federated:id"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: c.ClientID}),
		codec:    Codec{Key: c.SessionKey},
		ttl:      c.SessionTTL,
		secure:   strings.HasPrefix(c.PublicURL, "https://"),
		onSignIn: c.OnSignIn,
	}, nil
}

func provider(ctx context.Context, issuer, internal string) (*oidc.Provider, error) {
	if internal == "" {
		p, err := oidc.NewProvider(ctx, issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery at %s: %w", issuer, err)
		}
		return p, nil
	}
	// Discovery comes from the internal URL. InsecureIssuerURLContext turns off go-oidc's issuer check, so
	// the discovered issuer is compared with the public one here.
	d, err := oidc.NewProvider(oidc.InsecureIssuerURLContext(ctx, issuer), internal)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery at %s (issuer %s): %w", internal, issuer, err)
	}
	var m struct {
		Issuer   string   `json:"issuer"`
		Auth     string   `json:"authorization_endpoint"`
		Token    string   `json:"token_endpoint"`
		Keys     string   `json:"jwks_uri"`
		UserInfo string   `json:"userinfo_endpoint"`
		Algs     []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := d.Claims(&m); err != nil {
		return nil, fmt.Errorf("oidc discovery at %s: %w", internal, err)
	}
	if m.Issuer != issuer {
		return nil, fmt.Errorf("oidc discovery at %s names issuer %q, want %q", internal, m.Issuer, issuer)
	}
	// Server-to-server endpoints move to the internal URL; the sign-in page stays public for the browser.
	in := func(u string) string {
		if rest, ok := strings.CutPrefix(u, issuer); ok {
			return internal + rest
		}
		return u
	}
	return (&oidc.ProviderConfig{IssuerURL: issuer, AuthURL: m.Auth, TokenURL: in(m.Token),
		JWKSURL: in(m.Keys), UserInfoURL: in(m.UserInfo), Algorithms: m.Algs}).NewProvider(ctx), nil
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *Auth) cookie(name, value string, maxAge int, path string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode}
}

// Login starts the authorization code flow with PKCE. ?connector=local skips Dex's chooser.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	tx := loginTx{State: randomString(), Nonce: randomString(), Verifier: oauth2.GenerateVerifier(),
		Exp: time.Now().Add(10 * time.Minute).Unix()}
	http.SetCookie(w, a.cookie(txCookie, a.codec.Encode(tx), 600, "/auth"))
	opts := []oauth2.AuthCodeOption{oidc.Nonce(tx.Nonce), oauth2.S256ChallengeOption(tx.Verifier)}
	if c := r.URL.Query().Get("connector"); c != "" {
		opts = append(opts, oauth2.SetAuthURLParam("connector_id", c))
	}
	http.Redirect(w, r, a.oauth.AuthCodeURL(tx.State, opts...), http.StatusFound)
}

func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string, err error) {
		slog.Warn("sign-in failed", "reason", msg, "err", err)
		http.Error(w, "Sign-in failed: "+msg+". Start again from /login.", http.StatusBadRequest)
	}
	ck, err := r.Cookie(txCookie)
	if err != nil {
		fail("no sign-in in progress", err)
		return
	}
	var tx loginTx
	if err := a.codec.Decode(ck.Value, &tx); err != nil || time.Now().Unix() > tx.Exp {
		fail("sign-in expired", err)
		return
	}
	http.SetCookie(w, a.cookie(txCookie, "", -1, "/auth"))
	if r.URL.Query().Get("state") != tx.State {
		fail("state mismatch", nil)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail("identity provider returned "+e, nil)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(tx.Verifier))
	if err != nil {
		fail("code exchange", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		fail("ID token invalid", err)
		return
	}
	if idt.Nonce != tx.Nonce {
		fail("nonce mismatch", nil)
		return
	}
	var cl struct {
		Email     string   `json:"email"`
		Name      string   `json:"name"`
		Groups    []string `json:"groups"`
		Federated struct {
			ConnectorID string `json:"connector_id"`
		} `json:"federated_claims"`
	}
	if err := idt.Claims(&cl); err != nil || cl.Email == "" {
		fail("ID token has no email", err)
		return
	}
	boot := cl.Federated.ConnectorID == "local" && cl.Email == BootstrapEmail
	s := Session{ID: randomString(), Sub: idt.Subject, Email: cl.Email, Name: cl.Name, Groups: cl.Groups, Bootstrap: boot,
		Role: roleFor(cl.Federated.ConnectorID, cl.Email, cl.Groups), Exp: time.Now().Add(a.ttl).Unix(), SignedIn: time.Now().Unix()}
	a.Sessions.Add(s.ID, s.Email, s.Exp)
	http.SetCookie(w, a.cookie(SessionCookie, a.codec.Encode(s), int(a.ttl.Seconds()), "/"))
	slog.Info("signed in", "email", s.Email, "role", s.Role, "connector", cl.Federated.ConnectorID)
	if a.onSignIn != nil { // synchronous: the refresh holds the key lock for milliseconds
		a.onSignIn(context.WithoutCancel(r.Context()), s)
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use POST to sign out.", http.StatusMethodNotAllowed)
		return
	}
	if s, ok := a.Current(r); ok {
		a.Sessions.Remove(s.ID)
	}
	http.SetCookie(w, a.cookie(SessionCookie, "", -1, "/"))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Current returns the signed-in session, or false if there is none or it expired.
func (a *Auth) Current(r *http.Request) (Session, bool) {
	var s Session
	ck, err := r.Cookie(SessionCookie)
	if err != nil || a.codec.Decode(ck.Value, &s) != nil || time.Now().Unix() > s.Exp || !a.Sessions.Valid(s.ID) {
		return Session{}, false
	}
	return s, true
}
