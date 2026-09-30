package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}

func testServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "time", Version: "1"}, nil)
	h := func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "get_current_time", Description: "Current time in a timezone"}, h)
	mcp.AddTool(s, &mcp.Tool{Name: "convert_time", Description: "Convert between timezones"}, h)
	return s
}

func TestListToolsOverStreamableHTTP(t *testing.T) {
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return testServer() }, nil))
	defer srv.Close()
	got, err := listTools(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL, MaxRetries: -1, DisableStandaloneSSE: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "convert_time" || got[1].Name != "get_current_time" || got[1].Description != "Current time in a timezone" {
		t.Fatalf("tools (sorted by name) = %+v", got)
	}
}

func TestListToolsUnreachableFailsFast(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // nothing listens any more
	if _, err := listTools(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1}); err == nil {
		t.Fatal("want an error from a server that is gone")
	}
}
