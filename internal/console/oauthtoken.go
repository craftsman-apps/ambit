package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// Ambit makes its own token requests: golang.org/x/oauth2 cannot add RFC 8707 resource to a refresh,
// send JSON bodies, or tell a vendor's "try later" from "reconnect".

type TokenResult struct {
	AccessToken, RefreshToken string
	Expiry                    time.Time
}

// TokenError is a refused or failed token request. Transient: network error, 5xx, 429 or 408; the grant is kept.
type TokenError struct {
	Status            int
	Code, Description string
	Transient         bool
}

func (e *TokenError) Error() string {
	if e.Status == 0 {
		return "could not reach the token endpoint: " + e.Description
	}
	if e.Code == "" {
		return fmt.Sprintf("token endpoint answered %d %s", e.Status, e.Description)
	}
	return fmt.Sprintf("token endpoint answered %d: %s %s", e.Status, e.Code, e.Description)
}

type authStyle int

const (
	styleBasic authStyle = iota
	styleBody
)

func styleKey(conn *v1.Connection, secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return conn.Name + "/" + conn.Spec.ClientID + "/" + hex.EncodeToString(sum[:8])
}

// TokenRequest is the stateless form for tools (presetcheck): Basic first, then the secret in the body.
func TokenRequest(ctx context.Context, hc *http.Client, conn *v1.Connection, secret string, form url.Values, now time.Time) (TokenResult, error) {
	res, err := tokenOnce(ctx, hc, conn, secret, form, styleBasic, now)
	var te *TokenError
	if errors.As(err, &te) && te.Code == "invalid_client" {
		res, err = tokenOnce(ctx, hc, conn, secret, form, styleBody, now)
	}
	return res, err
}

func (c *Connections) tokenRequest(ctx context.Context, conn *v1.Connection, secret string, form url.Values) (TokenResult, error) {
	key := styleKey(conn, secret)
	c.stylesMu.Lock()
	style, known := c.styles[key]
	c.stylesMu.Unlock()
	res, err := tokenOnce(ctx, c.HTTP, conn, secret, form, style, c.now())
	var te *TokenError
	if !known && errors.As(err, &te) && te.Code == "invalid_client" {
		style = styleBody
		res, err = tokenOnce(ctx, c.HTTP, conn, secret, form, style, c.now())
	}
	if err == nil {
		c.stylesMu.Lock()
		if c.styles == nil {
			c.styles = map[string]authStyle{}
		}
		c.styles[key] = style
		c.stylesMu.Unlock()
	}
	return res, err
}

func tokenOnce(ctx context.Context, hc *http.Client, conn *v1.Connection, secret string, form url.Values, style authStyle, now time.Time) (TokenResult, error) {
	body := url.Values{}
	for k, v := range form {
		body[k] = v
	}
	if conn.Spec.Resource != "" {
		body.Set("resource", conn.Spec.Resource)
	}
	if style == styleBody {
		body.Set("client_id", conn.Spec.ClientID)
		body.Set("client_secret", secret)
	}
	payload, contentType := []byte(body.Encode()), "application/x-www-form-urlencoded"
	if conn.Spec.TokenRequestFormat == "JSON" {
		m := map[string]string{}
		for k := range body {
			m[k] = body.Get(k)
		}
		payload, _ = json.Marshal(m)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conn.Spec.TokenURL, bytes.NewReader(payload))
	if err != nil {
		return TokenResult{}, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if style == styleBasic {
		req.SetBasicAuth(url.QueryEscape(conn.Spec.ClientID), url.QueryEscape(secret))
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return TokenResult{}, &TokenError{Transient: true, Description: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		Error        string      `json:"error"`
		Description  string      `json:"error_description"`
		OK           *bool       `json:"ok"` // Slack answers 200 with ok:false
	}
	_ = json.Unmarshal(raw, &out)
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		return TokenResult{}, &TokenError{Status: resp.StatusCode, Transient: true}
	case resp.StatusCode >= 400 || out.Error != "" || (out.OK != nil && !*out.OK) || out.AccessToken == "":
		code := out.Error
		if code == "" {
			code = "invalid_response"
		}
		return TokenResult{}, &TokenError{Status: resp.StatusCode, Code: code, Description: out.Description}
	}
	exp := now.Add(time.Hour) // no expires_in: assume one hour (the broker gets no 401 feedback)
	if n, err := out.ExpiresIn.Int64(); err == nil && n > 0 {
		exp = now.Add(time.Duration(n) * time.Second)
	}
	return TokenResult{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Expiry: exp}, nil
}
