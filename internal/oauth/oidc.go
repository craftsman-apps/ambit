package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC is the company's identity provider: a confidential client with the code flow, PKCE and a nonce.
type OIDC struct {
	conf     oauth2.Config
	verifier *oidc.IDTokenVerifier
	groups   string
	client   *http.Client
	issReq   bool
	clientID string
}

// httpsClient never follows a redirect from https to another scheme.
func httpsClient(base *http.Client) *http.Client {
	c := &http.Client{Timeout: 15 * time.Second}
	if base != nil {
		cp := *base
		c = &cp
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("redirect to a URL that is not https")
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return c
}

// NewOIDC reads the provider's discovery document. The issuer in it must be the configured one (the library checks),
// and every endpoint it names must be https: a document that sends the code, the client secret or the key request over
// plain HTTP is refused.
func NewOIDC(ctx context.Context, c Config) (*OIDC, error) { return NewOIDCClient(ctx, c, nil) }

// NewOIDCClient is NewOIDC with the HTTP client to use (its TLS settings), for tests.
func NewOIDCClient(ctx context.Context, c Config, base *http.Client) (*OIDC, error) {
	hc := httpsClient(base)
	ctx = oidc.ClientContext(ctx, hc)
	p, err := oidc.NewProvider(ctx, c.IdPIssuer)
	if err != nil {
		return nil, fmt.Errorf("identity provider discovery: %w", stripURL(err))
	}
	var extra struct {
		JWKSURI string `json:"jwks_uri"`
		IssSupp bool   `json:"authorization_response_iss_parameter_supported"`
	}
	if err := p.Claims(&extra); err != nil {
		return nil, errors.New("identity provider discovery: the document is not readable")
	}
	ep := p.Endpoint()
	for name, raw := range map[string]string{"authorization_endpoint": ep.AuthURL, "token_endpoint": ep.TokenURL, "jwks_uri": extra.JWKSURI} {
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("identity provider discovery: %s is not an https address", name)
		}
	}
	return &OIDC{
		conf: oauth2.Config{ClientID: c.IdPClientID, ClientSecret: c.IdPClientSecret, Endpoint: ep,
			RedirectURL: c.PublicURL + "/callback", Scopes: []string{oidc.ScopeOpenID, "email", "profile"}},
		verifier: p.Verifier(&oidc.Config{ClientID: c.IdPClientID}), // signature, issuer, audience, expiry
		groups:   c.GroupsClaim, client: hc, issReq: extra.IssSupp, clientID: c.IdPClientID,
	}, nil
}

func (o *OIDC) IssRequired() bool { return o.issReq }

func (o *OIDC) AuthURL(state, nonce, verifier string) string {
	return o.conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (o *OIDC) Exchange(ctx context.Context, code, verifier string) (IDClaims, error) {
	if code == "" {
		return IDClaims{}, errors.New("no code")
	}
	ctx = context.WithValue(oidc.ClientContext(ctx, o.client), oauth2.HTTPClient, o.client)
	tok, err := o.conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return IDClaims{}, errors.New("code exchange failed") // the provider's answer may hold detail: never repeated
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return IDClaims{}, errors.New("the ID token was not accepted")
	}
	var cl map[string]any
	if err := idt.Claims(&cl); err != nil {
		return IDClaims{}, errors.New("ID token claims unreadable")
	}
	// OIDC Core 3.1.3.7: with several audiences, azp must be there and be this client; if azp is there, it must be this client.
	azp, hasAzp := cl["azp"].(string)
	if (len(idt.Audience) > 1 && !hasAzp) || (hasAzp && azp != o.clientID) || (len(idt.Audience) > 1 && !slices.Contains(idt.Audience, o.clientID)) {
		return IDClaims{}, errors.New("the ID token's audience or authorized party is not this client")
	}
	out := IDClaims{Issuer: idt.Issuer, Subject: idt.Subject, Nonce: idt.Nonce}
	out.Email, _ = cl["email"].(string)
	out.EmailVerified, _ = cl["email_verified"].(bool)
	if gs, ok := cl[o.groups].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				out.Groups = append(out.Groups, s)
			}
		}
	}
	return out, nil
}
