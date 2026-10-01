package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// revocationStyle names the provider format a revocation URL asks for; "rfc7009" is the standard.
func revocationStyle(u *url.URL) string {
	path := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.Contains(path, "/applications/") && strings.HasSuffix(path, "/grant"):
		return "github"
	case strings.HasSuffix(path, "/api/auth.revoke"):
		return "slack"
	case u.Hostname() == "api.notion.com" && path == "/v1/oauth/revoke":
		return "notion"
	case strings.Contains(path, "/oauth/v1/refresh-tokens"):
		return "hubspot"
	}
	return "rfc7009"
}

// revokesAccessToken reports whether the format sends the access token, which must then be current.
func revokesAccessToken(revURL string) bool {
	u, err := url.Parse(revURL)
	if err != nil {
		return false
	}
	switch revocationStyle(u) {
	case "github", "slack", "notion":
		return true
	}
	return false
}

// revocationRequest builds the request that revokes a grant at its provider, and the check for its answer.
// The admin enters only the provider's revocation URL; its shape picks the format (docs/offboarding.md):
//   - .../applications/<client>/grant            GitHub and GitHub Enterprise Server
//   - .../api/auth.revoke                         Slack
//   - api.notion.com/v1/oauth/revoke              Notion
//   - .../oauth/v1/refresh-tokens[/<token>]       HubSpot
//   - anything else                               standard RFC 7009
func revocationRequest(ctx context.Context, revURL, clientID, clientSecret string, g *Grant) (*http.Request, func(status int, body []byte) error, error) {
	u, err := url.Parse(revURL)
	if err != nil {
		return nil, nil, fmt.Errorf("revocation URL %q: %w", revURL, err)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(clientID)+":"+url.QueryEscape(clientSecret)))
	needAccess := func() error {
		if g.AccessToken == "" {
			return errors.New("the grant has no access token to revoke")
		}
		return nil
	}
	newReq := func(method, target string, body []byte) (*http.Request, error) {
		var r *http.Request
		var err error
		if body == nil {
			r, err = http.NewRequestWithContext(ctx, method, target, nil)
		} else {
			r, err = http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		return r, err
	}
	status := func(want ...int) func(int, []byte) error {
		return func(got int, body []byte) error {
			for _, w := range want {
				if got == w {
					return nil
				}
			}
			return fmt.Errorf("HTTP %d %s", got, short(strings.TrimSpace(string(body))))
		}
	}
	path := strings.TrimSuffix(u.Path, "/")

	switch revocationStyle(u) {
	case "github":
		// GitHub: DELETE /applications/{client_id}/grant removes the whole grant (every token of it).
		if err := needAccess(); err != nil {
			return nil, nil, err
		}
		base := path[:strings.LastIndex(path, "/applications/")]
		u.Path = base + "/applications/" + clientID + "/grant"
		u.RawPath = ""
		body, _ := json.Marshal(map[string]string{"access_token": g.AccessToken})
		r, err := newReq(http.MethodDelete, u.String(), body)
		if err != nil {
			return nil, nil, err
		}
		r.Header.Set("Authorization", basic)
		r.Header.Set("Accept", "application/vnd.github+json")
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		return r, status(http.StatusNoContent), nil

	case "slack":
		// Slack: the token revokes itself; failures still answer 200, with "ok": false.
		if err := needAccess(); err != nil {
			return nil, nil, err
		}
		r, err := newReq(http.MethodPost, u.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		r.Header.Set("Authorization", "Bearer "+g.AccessToken)
		return r, func(got int, body []byte) error {
			var a struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if got != http.StatusOK || json.Unmarshal(body, &a) != nil || !a.OK {
				return fmt.Errorf("HTTP %d %s", got, short(strings.TrimSpace(string(body))))
			}
			return nil
		}, nil

	case "notion":
		if err := needAccess(); err != nil {
			return nil, nil, err
		}
		body, _ := json.Marshal(map[string]string{"token": g.AccessToken})
		r, err := newReq(http.MethodPost, u.String(), body)
		if err != nil {
			return nil, nil, err
		}
		r.Header.Set("Authorization", basic)
		r.Header.Set("Notion-Version", "2026-03-11")
		return r, status(http.StatusOK), nil

	case "hubspot":
		// HubSpot: only the refresh token can be revoked; its access token lives on for up to 30 minutes.
		if g.RefreshToken == "" {
			return nil, nil, errors.New("HubSpot revokes only a refresh token, and the grant has none")
		}
		u.Path = path[:strings.Index(path, "/oauth/v1/refresh-tokens")] + "/oauth/v1/refresh-tokens/" + url.PathEscape(g.RefreshToken)
		u.RawPath = ""
		r, err := newReq(http.MethodDelete, u.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		return r, status(http.StatusNoContent, http.StatusOK), nil
	}

	// RFC 7009: the refresh token where there is one (it ends the access tokens with it at most providers).
	tok, hint := g.RefreshToken, "refresh_token"
	if tok == "" {
		tok, hint = g.AccessToken, "access_token"
	}
	form := url.Values{"token": {tok}, "token_type_hint": {hint}}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, revURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", basic)
	return r, status(http.StatusOK), nil
}
