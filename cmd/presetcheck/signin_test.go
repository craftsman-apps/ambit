package main

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/presets"
)

func TestSignInChecksStateAndStoresRefreshToken(t *testing.T) {
	var authURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path == "/token" && r.PostForm.Get("code") == "good" && r.PostForm.Get("code_verifier") != "" &&
			r.PostForm.Get("resource") == "https://mcp.v.test/mcp" {
			w.Write([]byte(`{"access_token":"at","refresh_token":"rt-new","expires_in":60}`))
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	p := presets.Preset{ID: "v", Server: presets.ServerRef{URL: "https://mcp.v.test/mcp", Transport: "streamable-http"},
		AuthorizationURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", AuthorizeParams: map[string]string{"prompt": "consent"}}
	pc := &PresetCreds{ClientID: "id", ClientSecret: "s"}
	flow := signInFlow{HTTP: srv.Client(), Listen: net.Listen, Open: func(u string) error {
		authURL = u
		q, _ := url.Parse(u)
		cb := q.Query().Get("redirect_uri")
		go func() {
			http.Get(cb + "?state=wrong&code=good")                          // a forged callback is refused
			http.Get(cb + "?state=" + q.Query().Get("state") + "&code=good") // the real one
		}()
		return nil
	}}
	if err := flow.run(t.Context(), p, pc); err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(authURL)
	if pc.RefreshToken != "rt-new" || q.Query().Get("code_challenge_method") != "S256" || q.Query().Get("prompt") != "consent" ||
		q.Query().Get("resource") != "https://mcp.v.test/mcp" || !strings.HasPrefix(q.Query().Get("redirect_uri"), "http://127.0.0.1:8765/") {
		t.Fatalf("token %q url %s", pc.RefreshToken, authURL)
	}
}

func TestSignInHonoursProviderError(t *testing.T) {
	flow := signInFlow{Listen: net.Listen, Open: func(u string) error {
		q, _ := url.Parse(u)
		go http.Get(q.Query().Get("redirect_uri") + "?state=" + q.Query().Get("state") + "&error=access_denied")
		return nil
	}}
	p := presets.Preset{ID: "v", Server: presets.ServerRef{URL: "https://mcp.v.test/mcp"}, AuthorizationURL: "https://a.test/authorize", TokenURL: "https://a.test/token"}
	if err := flow.run(t.Context(), p, &PresetCreds{ClientID: "id"}); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err %v", err)
	}
}

func TestSelfSignedCertCovers127(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrCreateCert(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "presetcheck-cert.pem"))
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatal("no PEM certificate")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, ip := range c.IPAddresses {
		ok = ok || ip.Equal(net.ParseIP("127.0.0.1"))
	}
	if st, _ := os.Stat(filepath.Join(dir, "presetcheck-key.pem")); !ok || st.Mode().Perm() != 0o600 {
		t.Fatalf("ips %v key mode %v", c.IPAddresses, st.Mode())
	}
	if _, err := loadOrCreateCert(dir); err != nil { // the second run loads the same files
		t.Fatal(err)
	}
}
