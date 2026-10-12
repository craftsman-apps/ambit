package console

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	mux.HandleFunc("/v1/connection/{name}", b.legacy)
	mux.HandleFunc("/v1/connection/{name}/server/{server}/{hash}", b.bound)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

const staleConfig = "this gateway configuration is out of date; it will refresh within a minute"

// legacy serves Connections created before destination binding (no serverURL).
func (b *Broker) legacy(w http.ResponseWriter, r *http.Request) {
	conn, err := b.Conns.connection(r.Context(), r.PathValue("name"))
	if err != nil || conn.Spec.ServerURL != "" { // only a readable, unbound Connection may use this path
		w.Header().Set("X-Ambit-Connect", staleConfig)
		w.WriteHeader(http.StatusOK)
		return
	}
	b.token(w, r, r.PathValue("name"))
}

// bound gives a token only when the gateway's destination is the server this Connection is bound to.
func (b *Broker) bound(w http.ResponseWriter, r *http.Request) {
	connName, server, hash := r.PathValue("name"), r.PathValue("server"), r.PathValue("hash")
	var s v1.Server
	err := b.Conns.Client.Get(r.Context(), client.ObjectKey{Namespace: b.Conns.Namespace, Name: server}, &s)
	conn, cerr := b.Conns.connection(r.Context(), connName)
	ok := err == nil && cerr == nil && s.Spec.Connection == connName && controller.DestinationHash(s.Spec.URL) == hash &&
		(conn.Spec.ServerURL == "" || conn.Spec.ServerURL == s.Spec.URL)
	if !ok {
		slog.Warn("broker: destination does not match", "connection", connName, "server", server)
		w.Header().Set("X-Ambit-Connect", staleConfig)
		w.WriteHeader(http.StatusOK)
		return
	}
	b.token(w, r, connName)
}

func (b *Broker) token(w http.ResponseWriter, r *http.Request, connName string) {
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
		slog.Warn("broker", "connection", connName, "user", email, "err", err)
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
