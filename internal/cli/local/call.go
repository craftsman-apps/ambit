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

// NoAnswerError means the settling window ended without an HTTP 200. LastStatus is the last HTTP status (0: no answer
// at all). local up treats a last status of 404 as a failed model: a removed provider model and the gateway's own
// settling 404 look the same.
type NoAnswerError struct {
	Limit      time.Duration
	LastStatus int
	Last       string
}

func (e *NoAnswerError) Error() string {
	return fmt.Sprintf("no answer within %s; the last answer was %s", e.Limit, e.Last)
}

// settling are the answers of a gateway that does not have the new Model or AccessRule yet: 404 until it has the
// model, then 403 until it has the rule (docs/quick-start.md), and 502/503 while its pods start.
var settling = map[int]bool{404: true, 403: true, 502: true, 503: true}

// Prompt is the message of the test call.
const Prompt = "Say hello in five words."

// Format is the request a tool sends: OpenAI Chat Completions (opencode, the SDKs), Anthropic Messages (Claude
// Code) or OpenAI Responses (Codex).
type Format int

const (
	FormatChat Format = iota
	FormatMessages
	FormatResponses
)

// request is the path, the JSON body and the headers of the test call in this format.
func (f Format) request(model, token string) (path string, body []byte, header http.Header) {
	header = http.Header{"Content-Type": {"application/json"}}
	var v map[string]any
	switch f {
	case FormatMessages:
		path, v = "/v1/messages", map[string]any{"model": model, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": Prompt}}}
		header.Set("x-api-key", token)
		header.Set("anthropic-version", "2023-06-01")
	case FormatResponses:
		path, v = "/v1/responses", map[string]any{"model": model, "max_output_tokens": 16, "input": Prompt}
		header.Set("Authorization", "Bearer "+token)
	default:
		path, v = "/v1/chat/completions", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": Prompt}}}
		header.Set("Authorization", "Bearer "+token)
	}
	body, _ = json.Marshal(v)
	return path, body, header
}

// answer is the text of a 200 answer in this format.
func (f Format) answer(b []byte) (string, bool) {
	var text []string
	switch f {
	case FormatMessages:
		var r struct{ Content []struct{ Type, Text string } }
		if json.Unmarshal(b, &r) != nil {
			return "", false
		}
		for _, c := range r.Content {
			if c.Type == "text" {
				text = append(text, c.Text)
			}
		}
	case FormatResponses:
		var r struct {
			Output []struct{ Content []struct{ Type, Text string } }
		}
		if json.Unmarshal(b, &r) != nil {
			return "", false
		}
		for _, o := range r.Output {
			for _, c := range o.Content {
				if c.Type == "output_text" {
					text = append(text, c.Text)
				}
			}
		}
	default:
		var r struct {
			Choices []struct{ Message struct{ Content string } }
		}
		if json.Unmarshal(b, &r) != nil || len(r.Choices) == 0 {
			return "", false
		}
		text = append(text, r.Choices[0].Message.Content)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(strings.Join(text, "")), "\n")
	return first, len(text) > 0
}

// RefusedError is the gateway's own refusal of the key (HTTP 401).
type RefusedError struct {
	Status int
	Body   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("the test call was refused: HTTP %d — the token or access rule is wrong: %.200s", e.Status, e.Body)
}

// TestCall is CallWith in the Chat Completions format.
func TestCall(ctx context.Context, c *http.Client, llmURL, token, model string, limit, every time.Duration) (string, error) {
	return CallWith(ctx, c, llmURL, token, model, FormatChat, limit, every)
}

// CallWith calls the gateway in the format until it answers 200 or limit passes, and returns the first line of the
// answer.
func CallWith(ctx context.Context, c *http.Client, llmURL, token, model string, f Format, limit, every time.Duration) (string, error) {
	path, body, header := f.request(model, token)
	deadline := time.Now().Add(limit)
	parent := ctx
	ctx, cancel := context.WithDeadline(ctx, deadline) // a request that hangs must not outlast limit
	defer cancel()
	last, lastStatus := "", 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+path, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header = header.Clone()
		resp, err := c.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
				first, ok := f.answer(b)
				if !ok {
					return "", fmt.Errorf("HTTP 200 without an answer: %.200s", b)
				}
				return first, nil
			case settling[resp.StatusCode]:
				last, lastStatus = fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, strings.TrimSpace(string(b))), resp.StatusCode
			case resp.StatusCode == http.StatusUnauthorized:
				return "", &RefusedError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
			case resp.StatusCode == http.StatusBadRequest:
				return "", fmt.Errorf("the test call failed: HTTP %d — the request was rejected: %.200s", resp.StatusCode, strings.TrimSpace(string(b)))
			default:
				return "", &ProviderError{Status: resp.StatusCode, Body: fmt.Sprintf("%.300s", strings.TrimSpace(string(b)))}
			}
		} else {
			if ctx.Err() == nil { // a request cut off by the window's own end says nothing new
				last, lastStatus = err.Error(), 0
			}
		}
		if time.Now().After(deadline) {
			return "", &NoAnswerError{Limit: limit, LastStatus: lastStatus, Last: last}
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return "", parent.Err()
			}
			return "", &NoAnswerError{Limit: limit, LastStatus: lastStatus, Last: last}
		case <-time.After(every):
		}
	}
}
