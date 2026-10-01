package console

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
)

// Each provider's revocation request, built from the revocation URL an admin entered (docs/offboarding.md).
func TestRevocationRequestPerProvider(t *testing.T) {
	g := &Grant{AccessToken: "at-alice", RefreshToken: "rt-alice"}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-1:secret-1"))
	for _, tc := range []struct {
		name, url             string
		method, wantURL, auth string
		body                  map[string]string // JSON body; nil for none
		form                  string            // form body
		header, headerValue   string
	}{
		{name: "standard RFC 7009", url: "https://oauth2.googleapis.com/revoke",
			method: "POST", wantURL: "https://oauth2.googleapis.com/revoke", auth: basic, form: "token=rt-alice&token_type_hint=refresh_token"},
		{name: "GitHub, client ID placeholder", url: "https://api.github.com/applications/{client_id}/grant",
			method: "DELETE", wantURL: "https://api.github.com/applications/client-1/grant", auth: basic, body: map[string]string{"access_token": "at-alice"},
			header: "Accept", headerValue: "application/vnd.github+json"},
		{name: "GitHub Enterprise Server", url: "https://ghe.acme.test/api/v3/applications/old-id/grant",
			method: "DELETE", wantURL: "https://ghe.acme.test/api/v3/applications/client-1/grant", auth: basic, body: map[string]string{"access_token": "at-alice"}},
		{name: "Slack", url: "https://slack.com/api/auth.revoke",
			method: "POST", wantURL: "https://slack.com/api/auth.revoke", auth: "Bearer at-alice"},
		{name: "Notion", url: "https://api.notion.com/v1/oauth/revoke",
			method: "POST", wantURL: "https://api.notion.com/v1/oauth/revoke", auth: basic, body: map[string]string{"token": "at-alice"},
			header: "Notion-Version", headerValue: "2026-03-11"},
		{name: "HubSpot", url: "https://api.hubapi.com/oauth/v1/refresh-tokens/{token}",
			method: "DELETE", wantURL: "https://api.hubapi.com/oauth/v1/refresh-tokens/rt-alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, err := revocationRequest(t.Context(), tc.url, "client-1", "secret-1", g)
			if err != nil {
				t.Fatal(err)
			}
			if req.Method != tc.method || req.URL.String() != tc.wantURL || req.Header.Get("Authorization") != tc.auth {
				t.Fatalf("%s %s auth %q", req.Method, req.URL, req.Header.Get("Authorization"))
			}
			if tc.header != "" && req.Header.Get(tc.header) != tc.headerValue {
				t.Errorf("%s: %q", tc.header, req.Header.Get(tc.header))
			}
			var raw []byte
			if req.Body != nil {
				raw, _ = io.ReadAll(req.Body)
			}
			switch {
			case tc.body != nil:
				var got map[string]string
				if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(tc.body) {
					t.Fatalf("body %s", raw)
				}
				for k, v := range tc.body {
					if got[k] != v {
						t.Fatalf("body %s", raw)
					}
				}
			case tc.form != "":
				if string(raw) != tc.form {
					t.Fatalf("form %s", raw)
				}
			default:
				if len(raw) != 0 {
					t.Fatalf("unexpected body %s", raw)
				}
			}
		})
	}
}

// What counts as success differs: Slack answers 200 with "ok": false on failure.
func TestRevocationResponsePerProvider(t *testing.T) {
	g := &Grant{AccessToken: "at", RefreshToken: "rt"}
	for _, tc := range []struct {
		url    string
		status int
		body   string
		ok     bool
	}{
		{"https://oauth2.googleapis.com/revoke", 200, "", true},
		{"https://oauth2.googleapis.com/revoke", 400, `{"error":"invalid_token"}`, false},
		{"https://api.github.com/applications/x/grant", 204, "", true},
		{"https://api.github.com/applications/x/grant", 404, `{"message":"Not Found"}`, false},
		{"https://slack.com/api/auth.revoke", 200, `{"ok":true,"revoked":true}`, true},
		{"https://slack.com/api/auth.revoke", 200, `{"ok":false,"error":"invalid_auth"}`, false},
		{"https://api.notion.com/v1/oauth/revoke", 200, `{"request_id":"r"}`, true},
		{"https://api.hubapi.com/oauth/v1/refresh-tokens/{token}", 204, "", true},
		{"https://api.hubapi.com/oauth/v1/refresh-tokens/{token}", 404, "", false},
	} {
		_, check, err := revocationRequest(t.Context(), tc.url, "c", "s", g)
		if err != nil {
			t.Fatal(err)
		}
		if err := check(tc.status, []byte(tc.body)); (err == nil) != tc.ok {
			t.Errorf("%s %d %s: %v", tc.url, tc.status, tc.body, err)
		}
	}
}

// A provider that needs a token the grant does not hold is an error, not a silent success.
func TestRevocationMissingToken(t *testing.T) {
	if _, _, err := revocationRequest(t.Context(), "https://api.hubapi.com/oauth/v1/refresh-tokens", "c", "s", &Grant{AccessToken: "at"}); err == nil ||
		!strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("HubSpot without a refresh token: %v", err)
	}
	if _, _, err := revocationRequest(t.Context(), "https://slack.com/api/auth.revoke", "c", "s", &Grant{RefreshToken: "rt"}); err == nil {
		t.Fatal("Slack without an access token")
	}
}

// Only the exact GitHub path shape and the other providers' own hosts pick a special format.
func TestRevocationStyleIsNotGuessedFromLookalikes(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.github.com/applications/{client_id}/grant":     "github",
		"https://ghe.acme.test/api/v3/applications/abc/grant":       "github",
		"https://idp.acme.test/oauth/applications/abc/grant/revoke": "rfc7009",
		"https://idp.acme.test/applications/abc/grants":             "rfc7009",
		"https://slack.com/api/auth.revoke":                         "slack",
		"https://chat.acme.test/api/auth.revoke":                    "rfc7009",
		"https://api.notion.com/v1/oauth/revoke":                    "notion",
		"https://api.hubapi.com/oauth/v1/refresh-tokens":            "hubspot",
		"https://idp.acme.test/oauth/v1/refresh-tokens/revoke":      "rfc7009",
		"https://oauth2.googleapis.com/revoke":                      "rfc7009",
	} {
		u, _ := url.Parse(raw)
		if got := revocationStyle(u); got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
}

func TestRedactTokens(t *testing.T) {
	g := &Grant{AccessToken: "at-secret", RefreshToken: "rt/secret"}
	got := redactTokens(`Delete "https://api.hubapi.com/oauth/v1/refresh-tokens/rt%2Fsecret": timeout; echoed at-secret and rt/secret`, g)
	if strings.Contains(got, "secret") || strings.Count(got, "[token]") != 3 {
		t.Fatal(got)
	}
}
