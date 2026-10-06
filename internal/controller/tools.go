package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolLister reads the tools a running Server offers.
type ToolLister func(ctx context.Context, s v1.Server) ([]v1.Tool, error)

// ListTools asks the Server's ToolHive proxy for its tools. The NetworkPolicy lets only the gateway
// and this controller reach the proxy.
func ListTools(ctx context.Context, s v1.Server) ([]v1.Tool, error) {
	endpoint := fmt.Sprintf("http://mcp-%s-proxy.%s.svc:%d/mcp", s.Name, s.Namespace, proxyPort)
	if s.Spec.Connection != "" {
		// Per-user servers need a user's token; without --console-tools-url their tools stay unlisted
		// and rules grant "all tools". With it, WithConsoleTools sends them to the console instead.
		return nil, nil
	}
	switch {
	case s.Spec.URL != "":
		endpoint = s.Spec.URL
	case s.Spec.Transport == "sse":
		endpoint = strings.TrimSuffix(endpoint, "/mcp") + "/sse"
	}
	if s.Spec.Transport == "sse" {
		return listTools(ctx, &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: noRedirects})
	}
	return listTools(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true, HTTPClient: noRedirects})
}

// noRedirects keeps tool discovery at the approved URL: a redirect could otherwise send the
// controller to an address nobody approved, such as a cloud metadata service.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return errors.New("redirects are not followed: approve the final URL instead")
}}

type bearerTransport struct {
	token, scheme, host string
	next                http.RoundTripper
}

// RoundTrip adds the token only for the server's own scheme and host.
func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == b.scheme && r.URL.Host == b.host {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

// BearerClient sends token to endpoint's host only and follows no redirects.
func BearerClient(base *http.Client, endpoint, token string) *http.Client {
	next := http.DefaultTransport
	if base != nil && base.Transport != nil {
		next = base.Transport
	}
	u, _ := url.Parse(endpoint)
	if u == nil {
		u = &url.URL{} // unparsable endpoint matches no request, so no token is sent
	}
	hc := &http.Client{Transport: bearerTransport{token: token, scheme: u.Scheme, host: u.Host, next: next},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if base != nil {
		hc.Timeout, hc.Jar = base.Timeout, base.Jar
	}
	return hc
}

// ListToolsAt lists the tools of an MCP server at a URL with one user's token (per-user servers). It
// follows no redirects, like ListTools.
func ListToolsAt(ctx context.Context, base *http.Client, endpoint, transport, bearer string) ([]v1.Tool, error) {
	hc := BearerClient(base, endpoint, bearer)
	if transport == "sse" {
		return listTools(ctx, &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc})
	}
	return listTools(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true, HTTPClient: hc})
}

// Fingerprint identifies a tool definition: what a model reads (name, description) and what it may
// send (input schema). The schema is decoded and re-encoded, so key order does not change it.
// ambit-guard computes the same value from the gateway's tools/list response.
func Fingerprint(name, description string, inputSchema []byte) string {
	var v any
	canon := inputSchema
	if json.Unmarshal(inputSchema, &v) == nil {
		canon, _ = json.Marshal(v) // Go sorts map keys
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s", name, description, canon)
	return hex.EncodeToString(h.Sum(nil))
}

func listTools(ctx context.Context, t mcp.Transport) ([]v1.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "ambit-controller", Version: v1.GroupVersion.Version}, nil)
	cs, err := c.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer cs.Close()
	var out []v1.Tool
	for tool, err := range cs.Tools(ctx, nil) { // follows pagination
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		vt := v1.Tool{Name: tool.Name, Description: tool.Description, Fingerprint: Fingerprint(tool.Name, tool.Description, schema)}
		if a := tool.Annotations; a != nil {
			ro := a.ReadOnlyHint
			vt.ReadOnlyHint = &ro
			vt.DestructiveHint = a.DestructiveHint
		}
		out = append(out, vt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrNoReader: a per-user server has no admin who agreed to have its tools read with their account.
var ErrNoReader = errors.New("no tool reader")

// ConsoleTools asks the console (port 8083) for a per-user server's tools; the console reads them with
// the consenting admin's grant and returns metadata only.
type ConsoleTools struct {
	URL  string // e.g. http://ambit-console.ambit-system.svc:8083
	HTTP *http.Client
}

// List returns ErrNoReader when the console has no reader for the server: 404 (no reader, or the
// console has not seen the Server yet) and 409 (not bound to its connection). Other failures are
// ordinary errors, so the controller retries.
func (c ConsoleTools) List(ctx context.Context, s v1.Server) ([]v1.Tool, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.URL, "/")+"/v1/tools/"+url.PathEscape(s.Name), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return nil, ErrNoReader
	default:
		return nil, fmt.Errorf("console tools: status %d", resp.StatusCode)
	}
	var out struct {
		Tools []v1.Tool `json:"tools"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// WithConsoleTools sends per-user servers to the console and every other server to base.
func WithConsoleTools(base ToolLister, c ConsoleTools) ToolLister {
	return func(ctx context.Context, s v1.Server) ([]v1.Tool, error) {
		if s.Spec.Connection != "" {
			return c.List(ctx, s)
		}
		return base(ctx, s)
	}
}
