package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AdminEmail is the bootstrap admin of the chart; ambit-cli is Dex's public client for the password grant.
const (
	AdminEmail = "admin@ambit.local"
	DexClient  = "ambit-cli"
)

// URLs of a local install on host port port. *.localtest.me resolves to 127.0.0.1 in public DNS.
func ConsoleURL(port int) string { return fmt.Sprintf("http://console.localtest.me:%d", port) }
func LLMURL(port int) string     { return fmt.Sprintf("http://llm.localtest.me:%d", port) }

// HTTPClient connects every *.localtest.me address to 127.0.0.1:port itself, so the test call does not depend on
// DNS (some company resolvers drop answers that point at 127.0.0.1).
func HTTPClient(port int) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the gateway is on this machine; never send these requests to HTTPS_PROXY
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(host, ".localtest.me") {
			addr = fmt.Sprintf("127.0.0.1:%d", port)
		}
		return d.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

// DexToken gets an access token for the bootstrap admin from Ambit's Dex (password grant, as the quick start does).
func DexToken(ctx context.Context, c *http.Client, consoleURL, password string) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {DexClient}, "scope": {"openid email profile"},
		"username": {AdminEmail}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleURL+"/dex/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Dex answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("Dex answered without an access token")
	}
	return t.AccessToken, nil
}

// ProviderError is an answer that is not 200, not part of the gateway's settling window, and not a refusal of our own
// call (400, 401): the model's provider failed or refused it (429, 500, 504 and similar). local up then tries the next
// free model.
type ProviderError struct {
	Status int
	Body   string
}

func (e *ProviderError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// settling are the answers of a gateway that does not have the new Model or AccessRule yet: 404 until it has the
// model, then 403 until it has the rule (docs/quick-start.md), and 502/503 while its pods start.
var settling = map[int]bool{404: true, 403: true, 502: true, 503: true}

// Prompt is the message of the test call.
const Prompt = "Say hello in five words."

// TestCall calls /v1/chat/completions until it answers 200 or limit passes, and returns the first line of the answer.
func TestCall(ctx context.Context, c *http.Client, llmURL, token, model string, limit, every time.Duration) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": Prompt}}})
	deadline := time.Now().Add(limit)
	parent := ctx
	ctx, cancel := context.WithDeadline(ctx, deadline) // a request that hangs must not outlast limit
	defer cancel()
	last := ""
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
				var r struct {
					Choices []struct{ Message struct{ Content string } }
				}
				if err := json.Unmarshal(b, &r); err != nil || len(r.Choices) == 0 {
					return "", fmt.Errorf("HTTP 200 without an answer: %.200s", b)
				}
				first, _, _ := strings.Cut(strings.TrimSpace(r.Choices[0].Message.Content), "\n")
				return first, nil
			case settling[resp.StatusCode]:
				last = fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, strings.TrimSpace(string(b)))
			case resp.StatusCode == http.StatusUnauthorized:
				return "", fmt.Errorf("the test call was refused: HTTP %d — the token or access rule is wrong: %.200s", resp.StatusCode, strings.TrimSpace(string(b)))
			case resp.StatusCode == http.StatusBadRequest:
				return "", fmt.Errorf("the test call failed: HTTP %d — the request was rejected: %.200s", resp.StatusCode, strings.TrimSpace(string(b)))
			default:
				return "", &ProviderError{Status: resp.StatusCode, Body: fmt.Sprintf("%.300s", strings.TrimSpace(string(b)))}
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no answer within %s; the last answer was %s", limit, last)
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return "", parent.Err()
			}
			return "", fmt.Errorf("no answer within %s; the last answer was %s", limit, last)
		case <-time.After(every):
		}
	}
}
