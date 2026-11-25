package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HydraAdmin is the part of Hydra's admin API that ambit-auth uses. The admin port can read Hydra's private keys: only
// this process and the console may reach it (NetworkPolicy ambit-hydra).
type HydraAdmin struct {
	Base string
	HTTP *http.Client
}

// ErrUnknown: Hydra does not know the challenge, or it was used or has expired.
var ErrUnknown = errors.New("unknown, used or expired request")

func (h *HydraAdmin) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// do sends one request and decodes a JSON answer into out. 404, 409 and 410 (Hydra's answers for an unknown, used or
// expired challenge) are ErrUnknown; any other status above 299 is an error that carries no body text.
func (h *HydraAdmin) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.Base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("hydra admin: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == 404 || resp.StatusCode == 409 || resp.StatusCode == 410:
		return resp.StatusCode, ErrUnknown
	case resp.StatusCode > 299:
		return resp.StatusCode, fmt.Errorf("hydra admin: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("hydra admin: %s %s: bad answer", method, path)
		}
	}
	return resp.StatusCode, nil
}

// Client is the part of a Hydra OAuth client that Ambit reads and writes.
type Client struct {
	ID           string         `json:"client_id,omitempty"`
	Name         string         `json:"client_name,omitempty"`
	RedirectURIs []string       `json:"redirect_uris,omitempty"`
	GrantTypes   []string       `json:"grant_types,omitempty"`
	ResponseType []string       `json:"response_types,omitempty"`
	AuthMethod   string         `json:"token_endpoint_auth_method,omitempty"`
	Scope        string         `json:"scope,omitempty"`
	Audience     []string       `json:"audience,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at,omitempty"`
}

// LoginRequest is what Ambit needs of Hydra's login request.
type LoginRequest struct {
	Challenge  string `json:"challenge"`
	RequestURL string `json:"request_url"`
	Client     Client `json:"client"`
}

func (h *HydraAdmin) GetLogin(ctx context.Context, challenge string) (LoginRequest, error) {
	var r LoginRequest
	_, err := h.do(ctx, "GET", "/admin/oauth2/auth/requests/login?login_challenge="+url.QueryEscape(challenge), nil, &r)
	return r, err
}

type redirectTo struct {
	RedirectTo string `json:"redirect_to"`
}

func (h *HydraAdmin) AcceptLogin(ctx context.Context, challenge, subject string, extra map[string]any) (string, error) {
	var o redirectTo
	_, err := h.do(ctx, "PUT", "/admin/oauth2/auth/requests/login/accept?login_challenge="+url.QueryEscape(challenge),
		map[string]any{"subject": subject, "remember": false, "context": extra}, &o)
	return o.RedirectTo, err
}

func (h *HydraAdmin) RejectLogin(ctx context.Context, challenge, code, description string) (string, error) {
	var o redirectTo
	_, err := h.do(ctx, "PUT", "/admin/oauth2/auth/requests/login/reject?login_challenge="+url.QueryEscape(challenge),
		map[string]any{"error": code, "error_description": description}, &o)
	return o.RedirectTo, err
}

// ConsentRequest is what Ambit needs of Hydra's consent request.
type ConsentRequest struct {
	Challenge      string         `json:"challenge"`
	Subject        string         `json:"subject"`
	RequestURL     string         `json:"request_url"`
	RequestedScope []string       `json:"requested_scope"`
	Client         Client         `json:"client"`
	Context        map[string]any `json:"context"`
}

func (h *HydraAdmin) GetConsent(ctx context.Context, challenge string) (ConsentRequest, error) {
	var r ConsentRequest
	_, err := h.do(ctx, "GET", "/admin/oauth2/auth/requests/consent?consent_challenge="+url.QueryEscape(challenge), nil, &r)
	return r, err
}

// AcceptConsentBody is Hydra's consent accept request with the claims Ambit puts into the access token.
type AcceptConsentBody struct {
	GrantScope    []string       `json:"grant_scope"`
	GrantAudience []string       `json:"grant_access_token_audience"`
	Remember      bool           `json:"remember"`
	AccessToken   map[string]any `json:"-"`
}

func (h *HydraAdmin) AcceptConsent(ctx context.Context, challenge string, b AcceptConsentBody) (string, error) {
	var o redirectTo
	_, err := h.do(ctx, "PUT", "/admin/oauth2/auth/requests/consent/accept?consent_challenge="+url.QueryEscape(challenge), map[string]any{
		"grant_scope": b.GrantScope, "grant_access_token_audience": b.GrantAudience, "remember": false,
		"session": map[string]any{"access_token": b.AccessToken},
	}, &o)
	return o.RedirectTo, err
}

func (h *HydraAdmin) RejectConsent(ctx context.Context, challenge, code, description string) (string, error) {
	var o redirectTo
	_, err := h.do(ctx, "PUT", "/admin/oauth2/auth/requests/consent/reject?consent_challenge="+url.QueryEscape(challenge),
		map[string]any{"error": code, "error_description": description}, &o)
	return o.RedirectTo, err
}

func (h *HydraAdmin) CreateClient(ctx context.Context, c Client) (Client, error) {
	var o Client
	_, err := h.do(ctx, "POST", "/admin/clients", c, &o)
	return o, err
}

func (h *HydraAdmin) GetClient(ctx context.Context, id string) (Client, error) {
	var o Client
	_, err := h.do(ctx, "GET", "/admin/clients/"+url.PathEscape(id), nil, &o)
	return o, err
}

func (h *HydraAdmin) DeleteClient(ctx context.Context, id string) error {
	_, err := h.do(ctx, "DELETE", "/admin/clients/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrUnknown) {
		return nil
	}
	return err
}

// SetClientMetadata replaces the metadata of a client (Hydra's PUT replaces the whole client: the other fields are sent back).
func (h *HydraAdmin) SetClientMetadata(ctx context.Context, id string, md map[string]any) error {
	var c map[string]any
	if _, err := h.do(ctx, "GET", "/admin/clients/"+url.PathEscape(id), nil, &c); err != nil {
		return err
	}
	c["metadata"] = md
	_, err := h.do(ctx, "PUT", "/admin/clients/"+url.PathEscape(id), c, nil)
	return err
}

// ListClients pages through every client (200 per page).
func (h *HydraAdmin) ListClients(ctx context.Context, each func(Client) error) error {
	token := ""
	for {
		var page []Client
		u := "/admin/clients?page_size=200"
		if token != "" {
			u += "&page_token=" + url.QueryEscape(token)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", h.Base+u, nil)
		resp, err := h.client().Do(req)
		if err != nil {
			return fmt.Errorf("hydra admin: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 || json.Unmarshal(data, &page) != nil {
			return fmt.Errorf("hydra admin: list clients: HTTP %d", resp.StatusCode)
		}
		for _, c := range page {
			if err := each(c); err != nil {
				return err
			}
		}
		token = nextPageToken(resp.Header.Values("Link"))
		if token == "" {
			return nil
		}
	}
}

// nextPageToken reads rel="next" of Hydra's Link header.
func nextPageToken(links []string) string {
	for _, l := range links {
		for _, part := range splitLinks(l) {
			if !bytes.Contains([]byte(part), []byte(`rel="next"`)) {
				continue
			}
			s, e := bytes.IndexByte([]byte(part), '<'), bytes.IndexByte([]byte(part), '>')
			if s < 0 || e < s {
				continue
			}
			if u, err := url.Parse(part[s+1 : e]); err == nil {
				return u.Query().Get("page_token")
			}
		}
	}
	return ""
}

func splitLinks(l string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range l {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, l[start:i])
				start = i + 1
			}
		}
	}
	return append(out, l[start:])
}

// RevokeConsents ends every grant of a person (refresh tokens stop; issued access tokens live to their expiry and are
// refused at the gateway by the stop list and the offboarding list). Used by offboarding and rotation (G2-3, G2-4).
func (h *HydraAdmin) RevokeConsents(ctx context.Context, subject string) error {
	_, err := h.do(ctx, "DELETE", "/admin/oauth2/auth/sessions/consent?all=true&subject="+url.QueryEscape(subject), nil, nil)
	if errors.Is(err, ErrUnknown) {
		return nil
	}
	return err
}
