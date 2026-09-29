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
	Issuer       string // Dex issuer, e.g. http://console.localtest.me:8080/dex
	ClientID     string
	ClientSecret string
	PublicURL    string // console URL; the callback is PublicURL + /auth/callback
	SessionKey   []byte
	SessionTTL   time.Duration
}

type Auth struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	codec    Codec
	ttl      time.Duration
	secure   bool
}

// loginTx is kept in a short-lived signed cookie between /auth/login and /auth/callback.
type loginTx struct {
	State, Nonce, Verifier string
	Exp                    int64
}

func NewAuth(ctx context.Context, c AuthConfig) (*Auth, error) {
	p, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery at %s: %w", c.Issuer, err)
	}
	return &Auth{
		oauth: oauth2.Config{
			ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: p.Endpoint(),
			RedirectURL: strings.TrimSuffix(c.PublicURL, "/") + "/auth/callback",
			Scopes:      []string{oidc.ScopeOpenID, "email", "profile", "groups", "federated:id"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: c.ClientID}),
		codec:    Codec{Key: c.SessionKey},
		ttl:      c.SessionTTL,
		secure:   strings.HasPrefix(c.PublicURL, "https://"),
	}, nil
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
	s := Session{Sub: idt.Subject, Email: cl.Email, Name: cl.Name, Groups: cl.Groups, Bootstrap: boot,
		Role: roleFor(cl.Federated.ConnectorID, cl.Email, cl.Groups), Exp: time.Now().Add(a.ttl).Unix()}
	http.SetCookie(w, a.cookie(SessionCookie, a.codec.Encode(s), int(a.ttl.Seconds()), "/"))
	slog.Info("signed in", "email", s.Email, "role", s.Role, "connector", cl.Federated.ConnectorID)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use POST to sign out.", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, a.cookie(SessionCookie, "", -1, "/"))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Current returns the signed-in session, or false if there is none or it expired.
func (a *Auth) Current(r *http.Request) (Session, bool) {
	var s Session
	ck, err := r.Cookie(SessionCookie)
	if err != nil || a.codec.Decode(ck.Value, &s) != nil || time.Now().Unix() > s.Exp {
		return Session{}, false
	}
	return s, true
}
