package console

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Broker gives the gateway the calling user's own token for a Connection (the gateway's extAuth).
//
// Trust boundary: the gateway validates the user's JWT, removes it, and tells the broker who the user
// is in the X-Ambit-User header, built from the validated jwt.email claim (not from anything the client
// sends). The broker believes that header because only the gateway can reach it (NetworkPolicy
// ambit-console-broker). Keeping the JWT removed means no MCP server ever receives the company token.
//
// With no grant, or a broken one, the broker adds no token: the server refuses and the gateway leaves
// only that server out of the user's session. A refusal from the broker would instead fail the user's
// whole MCP session. The connect link travels in X-Ambit-Connect for the gateway's logs; users see it
// on "My connections".
type Broker struct {
	Conns *Connections
}

const userHeader = "X-Ambit-User"

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/connection/{name}", b.token)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (b *Broker) token(w http.ResponseWriter, r *http.Request) {
	connName := r.PathValue("name")
	email := strings.TrimSpace(r.Header.Get(userHeader))
	link := b.Conns.connectURL(connName)
	if email == "" {
		w.Header().Set("X-Ambit-Connect", "the gateway token has no email claim, which Ambit uses to find your connections")
		w.WriteHeader(http.StatusOK)
		return
	}
	token, g, err := b.Conns.Token(r.Context(), email, connName)
	switch {
	case err != nil && token != "": // a usable token with a failed save or renewal: use it
		slog.Error("broker", "connection", connName, "user", email, "err", err)
		w.Header().Set("Authorization", "Bearer "+token)
	case err != nil:
		slog.Error("broker", "connection", connName, "user", email, "err", err)
		w.Header().Set("X-Ambit-Connect", "the console could not read the connection")
	case g == nil:
		w.Header().Set("X-Ambit-Connect", fmt.Sprintf("connect %s: %s", connName, link))
	case g.State == grantBrokenState:
		w.Header().Set("X-Ambit-Connect", fmt.Sprintf("reconnect %s (%s): %s", connName, g.Error, link))
	default:
		w.Header().Set("Authorization", "Bearer "+token)
	}
	w.WriteHeader(http.StatusOK)
}
