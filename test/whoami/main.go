// whoami is a test MCP server (step 5): its one tool reports whose token reached it, so a test can
// prove that each user's call carried that user's own SaaS grant. Test only; it does not verify tokens.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}

// subject reads preferred_username and iss from the bearer JWT without verifying it.
func subject(bearer string) string {
	parts := strings.Split(strings.TrimPrefix(bearer, "Bearer "), ".")
	if len(parts) != 3 {
		return "no token"
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "unreadable token"
	}
	var c struct {
		User string `json:"preferred_username"`
		Iss  string `json:"iss"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.User + " @ " + c.Iss
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "whoami", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "whoami", Description: "Reports which account's token reached this server"},
		func(_ context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			who := "no request headers"
			if req.Extra != nil {
				who = subject(req.Extra.Header.Get("Authorization"))
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: who}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})))
}
