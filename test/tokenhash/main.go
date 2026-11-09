// tokenhash is a test MCP server (step 5): an image that talks streamable HTTP and needs a person's token.
// Its tool reports a hash of the bearer token that reached it, and the user name inside the token, so a test
// can show that each person's own token arrives, through ToolHive's proxy. It also reports how it was
// configured: the variable TOKENHASH_LABEL, a hash of the variable TOKENHASH_SECRET (never the value), and the
// -suffix argument. It refuses a call with no bearer token. Test only.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}

// describe returns "sha256:<16 hex of the token's hash> user=<preferred_username, or - when the token is not a JWT>".
func describe(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	user := "-"
	if parts := strings.Split(bearer, "."); len(parts) == 3 {
		var m struct {
			User string `json:"preferred_username"`
		}
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil && json.Unmarshal(raw, &m) == nil && m.User != "" {
			user = m.User
		}
	}
	return "sha256:" + hex.EncodeToString(sum[:])[:16] + " user=" + user
}

// config says how the server was set up: a label, a hash of a secret value, and an argument.
func config(suffix string) string {
	sum := sha256.Sum256([]byte(os.Getenv("TOKENHASH_SECRET")))
	return " label=" + os.Getenv("TOKENHASH_LABEL") + " secret=" + hex.EncodeToString(sum[:])[:16] + " suffix=" + suffix
}

func main() {
	suffix := flag.String("suffix", "", "reported in the answer")
	flag.Parse()
	s := mcp.NewServer(&mcp.Implementation{Name: "tokenhash", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "token_hash", Description: "Reports a hash of the bearer token that reached this server"},
		func(_ context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			text := "no request headers"
			if req.Extra != nil {
				bearer, _ := strings.CutPrefix(req.Extra.Header.Get("Authorization"), "Bearer ")
				text = describe(bearer) + config(*suffix)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})))
}
