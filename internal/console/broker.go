package console

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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
	// Agents reads agent records. nil: no agent gets a token.
	Agents interface {
		GetAgent(ctx context.Context, name string) (Agent, error)
	}
}

const (
	userHeader  = "X-Ambit-User"
	agentHeader = "X-Ambit-Agent" // the agent's name from the verified credential; "-" for a person's own key or token
)

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
	ok := err == nil && cerr == nil && s.Spec.Connection == connName && controller.DestinationHash(controller.Endpoint(s)) == hash &&
		(conn.Spec.ServerURL == "" || conn.Spec.ServerURL == controller.Endpoint(s))
	if !ok {
		slog.Warn("broker: destination does not match", "connection", connName, "server", server)
		w.Header().Set("X-Ambit-Connect", staleConfig)
		w.WriteHeader(http.StatusOK)
		return
	}
	b.token(w, r, connName)
}

// agentAllowed decides, on every request, whether the caller may use email's connection connName. The gateway names the
// calling agent in X-Ambit-Agent, from the verified key or token (a header of the client is never copied); "-" is a
// person's own key or token. A request without the header is refused (a gateway of an older version, or something
// that is not the gateway). An agent gets a token only when its record is readable and active, is a person-mode agent
// of this very person, and lists the connection (the person's consent). Every other case gives no token (fail closed).
func (b *Broker) agentAllowed(r *http.Request, email, connName string) (bool, string) {
	vals, present := r.Header[http.CanonicalHeaderKey(agentHeader)]
	if !present || len(vals) != 1 {
		return false, "the gateway did not say which agent is calling, so no token is given"
	}
	name := vals[0] // exactly as the gateway sent it: no trimming, so that a padded marker is not the marker
	if name == "-" {
		return true, ""
	}
	if !accountNamePattern.MatchString(name) {
		return false, "the gateway sent an agent name that is not valid, so no token is given"
	}
	if b.Agents == nil {
		return false, "agents cannot use connections here"
	}
	a, err := b.Agents.GetAgent(r.Context(), name)
	switch {
	case err != nil:
		slog.Warn("broker: agent record not readable; no token", "agent", name, "err", err)
		return false, "the agent's record could not be read, so no token is given"
	case a.State != StateActive || a.Mode != ModePerson || a.Owner != email:
		return false, "this agent may not use a person's connection"
	case !slices.Contains(a.ConnectionConsent, connName):
		return false, fmt.Sprintf("the owner has not allowed the agent %s to use %s: allow it in the agent's settings", name, connName)
	}
	return true, ""
}

func (b *Broker) token(w http.ResponseWriter, r *http.Request, connName string) {
	email := strings.TrimSpace(r.Header.Get(userHeader))
	link := b.Conns.connectURL(connName)
	if email == "" {
		w.Header().Set("X-Ambit-Connect", "the gateway token has no email claim, which Ambit uses to find your connections")
		w.WriteHeader(http.StatusOK)
		return
	}
	if ok, why := b.agentAllowed(r, email, connName); !ok {
		w.Header().Set("X-Ambit-Connect", why)
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
