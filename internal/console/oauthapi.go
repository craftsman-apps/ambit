package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/identity"
	authnv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OAuthAPI is the console's internal API for ambit-auth (docs/oauth.md, "Internal API of the console"): it answers which MCP servers
// a person may offer to a platform, creates the person-mode agent that a consent makes, and says whether an agent and its grant are
// still active. It listens on its own port, and every call must carry the service account token of ambit-auth (a projected token
// whose audience is the console's), which the API server checks (TokenReview). Nothing else may call it.
type OAuthAPI struct {
	Keys      *KeyService
	Client    client.Reader
	Review    client.Writer // creates TokenReviews
	Namespace string
	// Audience is the audience of the projected token; Subject is the only service account allowed.
	Audience, Subject string
	Now               func() time.Time
}

// OAuthSubject is the user name Kubernetes gives ambit-auth's service account.
func OAuthSubject(namespace string) string {
	return "system:serviceaccount:" + namespace + ":ambit-auth"
}

func (a *OAuthAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *OAuthAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/oauth/v1/servers", a.guard(a.servers))
	mux.HandleFunc("POST /internal/oauth/v1/agents", a.guard(a.createAgent))
	mux.HandleFunc("POST /internal/oauth/v1/active", a.guard(a.active))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// guard lets a call through only for ambit-auth's own token. The API server decides (TokenReview); a token with another audience, another
// service account or no valid signature is refused with 401.
func (a *OAuthAPI) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok == r.Header.Get("Authorization") || len(tok) > 16<<10 {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		tr := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: tok, Audiences: []string{a.Audience}}}
		if err := a.Review.Create(ctx, tr); err != nil {
			slog.Error("oauth api: token review failed", "err", err)
			http.Error(w, "cannot check the token", http.StatusServiceUnavailable)
			return
		}
		if !tr.Status.Authenticated || tr.Status.User.Username != a.Subject || !containsStr(tr.Status.Audiences, a.Audience) {
			http.Error(w, "not allowed", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ServerChoice is one server a person may offer: allowed by the rules, and served by the front door.
type ServerChoice struct {
	Name       string `json:"name"`
	Display    string `json:"display"`
	Connection string `json:"connection,omitempty"`
}

// FrontDoorServersFor lists the servers that the person's AccessRules allow (at least one tool) and the front door serves.
func (a *OAuthAPI) frontDoorServersFor(ctx context.Context, email string, groups []string) ([]ServerChoice, error) {
	var servers v1.ServerList
	var conns v1.ConnectionList
	var rules v1.AccessRuleList
	if err := a.Client.List(ctx, &servers, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	if err := a.Client.List(ctx, &conns, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	if err := a.Client.List(ctx, &rules, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	out := []ServerChoice{}
	for _, s := range servers.Items {
		if !controller.OnFrontDoor(s) || !controller.PersonMayUse(servers.Items, conns.Items, rules.Items, s.Name, email, groups) {
			continue
		}
		out = append(out, ServerChoice{Name: s.Name, Display: s.Name})
	}
	return out, nil
}

func (a *OAuthAPI) servers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := readJSON(r, &in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email, ok := identity.CanonicalEmail(in.Email)
	if !ok || identity.IsService(email) || len(in.Groups) > 200 {
		reply(w, http.StatusOK, map[string]any{"servers": []ServerChoice{}})
		return
	}
	// An offboarded person is offered nothing.
	if blocked, err := a.Keys.blockedEmails(r.Context()); err != nil {
		http.Error(w, "cannot read the offboarding list", http.StatusServiceUnavailable)
		return
	} else if blocked[email] {
		reply(w, http.StatusOK, map[string]any{"servers": []ServerChoice{}})
		return
	}
	list, err := a.frontDoorServersFor(r.Context(), email, in.Groups)
	if err != nil {
		slog.Error("oauth api: servers", "err", err)
		http.Error(w, "cannot read the servers", http.StatusServiceUnavailable)
		return
	}
	reply(w, http.StatusOK, map[string]any{"servers": list})
}

func (a *OAuthAPI) createAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Owner, Platform, DisplayName string
		Scope, Connections           []string
		GrantID                      string   `json:"grantID"`
		Groups                       []string `json:"groups"` // the groups of the person's sign-in
	}
	if err := readJSON(r, &in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	owner, ok := identity.CanonicalEmail(in.Owner)
	if !ok || identity.IsService(owner) {
		http.Error(w, "bad owner", http.StatusBadRequest)
		return
	}
	// Whatever ambit-auth sends, a scope must name only servers that the person may offer: what their AccessRules allow and the front
	// door serves (the same list as the consent page was built from).
	if len(in.Groups) > 200 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	allowed, err := a.frontDoorServersFor(r.Context(), owner, in.Groups)
	if err != nil {
		http.Error(w, "cannot read the servers", http.StatusServiceUnavailable)
		return
	}
	for _, sc := range in.Scope {
		name, _, _ := strings.Cut(sc, "/")
		if !slices.ContainsFunc(allowed, func(c ServerChoice) bool { return c.Name == name }) {
			reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "the agent was refused"})
			return
		}
	}
	// The person acts for themselves: the console treats the call as that person's own sign-in (limits, offboarding and names as for an
	// agent made by hand).
	by := Session{Email: owner, Sub: owner, SignedIn: a.now().Unix(), Role: RoleViewer}
	ag, err := a.Keys.CreateAgent(r.Context(), by, AgentInput{Mode: ModePerson, DisplayName: in.DisplayName, Platform: in.Platform,
		Scope: in.Scope, Consent: in.Connections, Grant: in.GrantID})
	switch {
	case err == nil:
		reply(w, http.StatusOK, map[string]string{"name": ag.Name})
	case errors.Is(err, ErrAgentLimit):
		reply(w, http.StatusConflict, map[string]string{"error": "the person has too many agents"})
	case errors.Is(err, ErrOffboarded):
		reply(w, http.StatusConflict, map[string]string{"error": "the person is offboarded"})
	case errors.Is(err, ErrBadInput), errors.Is(err, ErrNotAllowed):
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "the agent was refused"})
	default:
		slog.Error("oauth api: create agent", "err", err)
		http.Error(w, "cannot create the agent", http.StatusServiceUnavailable)
	}
}

func (a *OAuthAPI) active(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent, Grant string
		Scope        []string // the scope of the token that is being refreshed
	}
	if err := readJSON(r, &in); err != nil || in.Agent == "" || in.Grant == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ag, err := a.Keys.GetAgent(r.Context(), in.Agent)
	switch {
	case errors.Is(err, ErrNoAgent):
		reply(w, http.StatusOK, map[string]bool{"active": false})
	case err != nil:
		slog.Error("oauth api: read agent", "err", err)
		http.Error(w, "cannot read the agent", http.StatusServiceUnavailable)
	default:
		on := ag.State == StateActive && ag.Mode == ModePerson && containsStr(ag.Grants, in.Grant)
		// The token's scope must still be within the agent's: a scope that was narrowed since the consent ends the refresh (the platform
		// connects again, and the person sees what is offered). A token that was issued before stays as it is until it expires.
		for _, sc := range in.Scope {
			on = on && containsStr(ag.Scope, sc)
		}
		if on {
			// The owner must not be offboarded either: the sign-in is refused, and so is every refresh.
			if blocked, err := a.Keys.blockedEmails(r.Context()); err != nil {
				http.Error(w, "cannot read the offboarding list", http.StatusServiceUnavailable)
				return
			} else if blocked[ag.Owner] {
				on = false
			}
		}
		reply(w, http.StatusOK, map[string]bool{"active": on})
	}
}
