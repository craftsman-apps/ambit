// whoami is a test MCP server (step 5). It verifies the bearer token like a real SaaS app would
// (RS256 signature against the realm's JWKS, issuer, expiry, audience), and its tools report whose
// token reached it. Test only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}

// verify checks a compact JWT and returns its claims JSON.
func verify(token string, keys *jose.JSONWebKeySet, iss, aud string, now time.Time) (json.RawMessage, error) {
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, err
	}
	if len(tok.Headers) != 1 {
		return nil, errors.New("one signature expected")
	}
	cands := keys.Keys
	if kid := tok.Headers[0].KeyID; kid != "" {
		cands = keys.Key(kid)
	}
	for _, k := range cands {
		var c jwt.Claims
		var raw json.RawMessage
		if tok.Claims(k, &c, &raw) != nil {
			continue
		}
		if err := c.ValidateWithLeeway(jwt.Expected{Issuer: iss, AnyAudience: jwt.Audience{aud}, Time: now}, 0); err != nil {
			return nil, err
		}
		if c.Expiry == nil {
			return nil, errors.New("no exp")
		}
		return raw, nil
	}
	return nil, errors.New("no key verifies the signature")
}

// jwks fetches the realm's keys once and keeps them; a failed fetch is retried on the next request.
type jwks struct {
	url  string
	mu   sync.Mutex
	keys *jose.JSONWebKeySet
}

func (j *jwks) get() (*jose.JSONWebKeySet, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.keys != nil {
		return j.keys, nil
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(j.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ks jose.JSONWebKeySet
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("JWKS: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&ks); err != nil {
		return nil, err
	}
	j.keys = &ks
	return j.keys, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	iss := env("ISSUER", "http://keycloak.localtest.me:8080/realms/saas")
	aud := env("AUDIENCE", "http://whoami.ambit-system.svc:8080/mcp")
	keys := &jwks{url: env("JWKS_URL", "http://keycloak.ambit-system.svc:8080/realms/saas/protocol/openid-connect/certs")}

	s := mcp.NewServer(&mcp.Implementation{Name: "whoami", Version: "1"}, nil)
	who := func(_ context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		text := "no request headers"
		if req.Extra != nil {
			// The middleware verified the token; this only reads the claims.
			var m struct {
				User string `json:"preferred_username"`
				Iss  string `json:"iss"`
			}
			if t, err := jwt.ParseSigned(strings.TrimPrefix(req.Extra.Header.Get("Authorization"), "Bearer "), []jose.SignatureAlgorithm{jose.RS256}); err == nil {
				_ = t.UnsafeClaimsWithoutVerification(&m)
			}
			text = m.User + " @ " + m.Iss
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "whoami", Description: "Reports which account's token reached this server"}, who)
	mcp.AddTool(s, &mcp.Tool{Name: "whoami_admin", Description: "Same as whoami; a second tool, to test granting one tool of two"}, who)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		ks, err := keys.get()
		if err != nil {
			log.Printf("keys: %v", err)
			http.Error(w, "cannot load signing keys", http.StatusUnauthorized)
			return
		}
		if _, err := verify(bearer, ks, iss, aud, time.Now()); err != nil {
			log.Printf("rejected token: %v", err)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})))
}
