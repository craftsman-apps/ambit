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

// tryOtherStyle: while the client auth style is unknown, any refusal that is not transient may mean the
// vendor wants the other style (GitHub answers 404 "Not Found" to a Basic header), as golang.org/x/oauth2 does.
func tryOtherStyle(err error) bool {
	var te *TokenError
	return errors.As(err, &te) && !te.Transient
}

// TokenRequest is the stateless form for tools (presetcheck): Basic first, then the secret in the body.
func TokenRequest(ctx context.Context, hc *http.Client, conn *v1.Connection, secret string, form url.Values, now time.Time) (TokenResult, error) {
	res, err := tokenOnce(ctx, hc, conn, secret, form, styleBasic, now)
	if tryOtherStyle(err) {
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
	if !known && tryOtherStyle(err) {
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
	// No expires_in: with a refresh token (new or the one being used), assume one hour, since the broker
	// gets no 401 feedback. With none at all (GitHub OAuth Apps, Slack without rotation) the token does
	// not expire: a zero Expiry means "valid until the provider refuses it".
	var exp time.Time
	if out.RefreshToken != "" || form.Get("refresh_token") != "" {
		exp = now.Add(time.Hour)
	}
	if n, err := out.ExpiresIn.Int64(); err == nil && n > 0 {
		exp = now.Add(time.Duration(n) * time.Second)
	}
	return TokenResult{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Expiry: exp}, nil
}
