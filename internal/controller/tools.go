package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	switch {
	case s.Spec.Connection != "":
		// ponytail: tools of a per-user server need a user's token to read; they stay unlisted, so
		// rules grant "all tools" for such servers. Read them with a service grant if needed.
		return nil, nil
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
