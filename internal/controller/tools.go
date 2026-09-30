package controller

import (
	"context"
	"fmt"
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
		return listTools(ctx, &mcp.SSEClientTransport{Endpoint: endpoint})
	}
	return listTools(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true})
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
		out = append(out, v1.Tool{Name: tool.Name, Description: tool.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
