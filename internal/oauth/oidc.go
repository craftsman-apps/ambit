package oauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC is the company's identity provider: a confidential client with the code flow, PKCE and a nonce.
type OIDC struct {
	conf     oauth2.Config
	verifier *oidc.IDTokenVerifier
	groups   string
}

// NewOIDC reads the provider's discovery document. The issuer in it must be the configured one (the library checks).
func NewOIDC(ctx context.Context, c Config) (*OIDC, error) {
	p, err := oidc.NewProvider(ctx, c.IdPIssuer)
	if err != nil {
		return nil, fmt.Errorf("identity provider discovery: %w", err)
	}
	return &OIDC{
		conf: oauth2.Config{ClientID: c.IdPClientID, ClientSecret: c.IdPClientSecret, Endpoint: p.Endpoint(),
			RedirectURL: c.PublicURL + "/callback", Scopes: []string{oidc.ScopeOpenID, "email", "profile"}},
		verifier: p.Verifier(&oidc.Config{ClientID: c.IdPClientID}), // signature, issuer, audience, expiry
		groups:   c.GroupsClaim,
	}, nil
}

func (o *OIDC) AuthURL(state, nonce, verifier string) string {
	return o.conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (o *OIDC) Exchange(ctx context.Context, code, verifier string) (IDClaims, error) {
	if code == "" {
		return IDClaims{}, errors.New("no code")
	}
	tok, err := o.conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return IDClaims{}, errors.New("code exchange failed")
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return IDClaims{}, fmt.Errorf("ID token: %w", err)
	}
	var cl map[string]any
	if err := idt.Claims(&cl); err != nil {
		return IDClaims{}, errors.New("ID token claims unreadable")
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
