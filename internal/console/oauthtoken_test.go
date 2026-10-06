package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

func tokenServer(t *testing.T, h http.HandlerFunc) *v1.Connection {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &v1.Connection{Spec: v1.ConnectionSpec{TokenURL: srv.URL + "/token", ClientID: "ambit", Resource: "https://mcp.vendor.test/mcp"}}
}

func TestRefreshSendsResourceAndKeepsOldRefreshToken(t *testing.T) {
	var got url.Values
	conn := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "token_type": "Bearer"}) // no refresh_token, no expires_in
	})
	c := &Connections{Now: func() time.Time { return time.Unix(1000, 0) }}
	res, err := c.tokenRequest(t.Context(), conn, "s3cret", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"rt1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("resource") != "https://mcp.vendor.test/mcp" || res.AccessToken != "at2" || res.RefreshToken != "" {
		t.Fatalf("form %v result %+v", got, res)
	}
	if !res.Expiry.Equal(time.Unix(1000, 0).Add(time.Hour)) {
		t.Fatalf("missing expires_in must mean one hour, got %v", res.Expiry)
	}
}

func TestRefreshSlackStyleErrorIsBroken(t *testing.T) {
	conn := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_refresh_token"}) // HTTP 200
	})
	_, err := (&Connections{}).tokenRequest(t.Context(), conn, "s", url.Values{"grant_type": {"refresh_token"}})
	var te *TokenError
	if !errors.As(err, &te) || te.Transient || te.Code != "invalid_refresh_token" {
		t.Fatalf("err %v", err)
	}
}

func TestTokenServerErrorIsTransient(t *testing.T) {
	conn := tokenServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", http.StatusBadGateway) })
	_, err := (&Connections{}).tokenRequest(t.Context(), conn, "s", url.Values{"grant_type": {"refresh_token"}})
	var te *TokenError
	if !errors.As(err, &te) || !te.Transient {
		t.Fatalf("err %v", err)
	}
}

func TestClientAuthIsRememberedAndForgottenWithTheSecret(t *testing.T) {
	calls := 0
	conn := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if _, _, basic := r.BasicAuth(); basic || in["client_secret"] == "" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 60})
	})
	conn.Spec.TokenRequestFormat = "JSON"
	c := &Connections{}
	form := func() url.Values { return url.Values{"grant_type": {"authorization_code"}, "code": {"x"}} }
	for i := 0; i < 2; i++ {
		if _, err := c.tokenRequest(t.Context(), conn, "s", form()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 { // first: Basic refused, then body; second: body straight away
		t.Fatalf("calls %d", calls)
	}
	if _, err := c.tokenRequest(t.Context(), conn, "rotated-secret", form()); err != nil || calls != 5 {
		t.Fatalf("a new secret must be probed again: calls %d err %v", calls, err)
	}
}
