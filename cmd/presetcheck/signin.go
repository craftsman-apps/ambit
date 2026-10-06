package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/console"
	"github.com/craftsman-apps/ambit/internal/presets"
	"golang.org/x/oauth2"
)

// signInFlow signs the maintainer in once per vendor in their own browser. Claude never runs it.
type signInFlow struct {
	Open   func(url string) error
	HTTP   *http.Client
	Listen func(network, addr string) (net.Listener, error)
	TLS    *tls.Config // https callback (Slack, Asana): a self-signed certificate for 127.0.0.1
}

func (f signInFlow) run(ctx context.Context, p presets.Preset, pc *PresetCreds) error {
	res, err := p.Resolve(pc.Values, pc.ClientID, callbackFor(p))
	if err != nil {
		return err
	}
	cb, _ := url.Parse(callbackFor(p))
	ln, err := f.Listen("tcp", cb.Host)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cb.Host, err)
	}
	if cb.Scheme == "https" {
		if f.TLS == nil {
			ln.Close()
			return errors.New("https callback needs a certificate")
		}
		ln = tls.NewListener(ln, f.TLS)
	}
	defer ln.Close()
	b := make([]byte, 16)
	rand.Read(b)
	state, verifier := hex.EncodeToString(b), oauth2.GenerateVerifier()
	q := url.Values{"response_type": {"code"}, "client_id": {pc.ClientID}, "redirect_uri": {cb.String()}, "state": {state},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}, "access_type": {"offline"}}
	if len(res.Connection.Scopes) > 0 {
		q.Set("scope", strings.Join(res.Connection.Scopes, " "))
	}
	if res.Connection.Resource != "" {
		q.Set("resource", res.Connection.Resource)
	}
	keys := make([]string, 0, len(res.Connection.AuthorizeParams))
	for k := range res.Connection.AuthorizeParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q.Set(k, res.Connection.AuthorizeParams[k])
	}
	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "this sign-in did not start here", http.StatusBadRequest)
			return
		}
		o := outcome{code: r.URL.Query().Get("code")}
		if e := r.URL.Query().Get("error"); e != "" {
			o = outcome{err: fmt.Errorf("the provider refused: %s %s", e, r.URL.Query().Get("error_description"))}
		}
		select {
		case done <- o:
		default: // a repeated callback: the first one counts
		}
		fmt.Fprintln(w, "Signed in. You can close this tab and return to the terminal.")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := f.Open(res.Connection.AuthorizationURL + "?" + q.Encode()); err != nil {
		return err
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(5 * time.Minute):
		return errors.New("no sign-in within 5 minutes")
	case <-ctx.Done():
		return ctx.Err()
	}
	if o.err != nil {
		return o.err
	}
	conn := &v1.Connection{Spec: res.Connection}
	conn.Spec.ClientID = pc.ClientID
	tok, err := console.TokenRequest(ctx, f.HTTP, conn, pc.ClientSecret, url.Values{"grant_type": {"authorization_code"}, "code": {o.code},
		"redirect_uri": {cb.String()}, "code_verifier": {verifier}}, time.Now())
	if err != nil {
		return err
	}
	// No refresh token: kept as reported, not a failure (spec: such presets are reported so their setup can
	// request offline access; users reconnect when access expires).
	pc.RefreshToken, pc.AccessToken, pc.Expiry = tok.RefreshToken, tok.AccessToken, tok.Expiry
	return nil
}

// loadOrCreateCert returns the self-signed certificate for the https callback on 127.0.0.1:8766, creating
// it (1 year) in dir on first use. The maintainer accepts it once in the browser.
func loadOrCreateCert(dir string) (*tls.Config, error) {
	certPath, keyPath := filepath.Join(dir, "presetcheck-cert.pem"), filepath.Join(dir, "presetcheck-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && time.Now().Before(c.Leaf.NotAfter) {
		return &tls.Config{Certificates: []tls.Certificate{c}}, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "presetcheck 127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(keyPath, 0o600); err != nil { // WriteFile keeps the mode of an existing file
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{c}}, nil
}
