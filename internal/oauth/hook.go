package oauth

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// tokenHook is Hydra's oauth2.token_hook, called for every grant. It is the only thing that stops a *refresh*: the
// gateway refuses issued tokens by itself (docs/oauth.md, "Stopping a connection"). It answers 403 (Hydra then answers
// access_denied and keeps the refresh token) when the person is offboarded or the agent or grant is stopped, 503 when it
// cannot be sure (fail closed), and otherwise 200 with the person's current groups.
func (s *Server) tokenHook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session struct {
			Subject string         `json:"subject"`
			Extra   map[string]any `json:"extra"`
		} `json:"session"`
		Request struct {
			ClientID   string   `json:"client_id"`
			GrantTypes []string `json:"grant_types"`
		} `json:"request"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if json.Unmarshal(b, &in) != nil {
		mHook.WithLabelValues("bad_request").Inc()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	agent, _ := in.Session.Extra["ambit_agent"].(string)
	grant, _ := in.Session.Extra["ambit_grant"].(string)
	email, _ := in.Session.Extra["email"].(string)
	// Every grant of this server is made at the consent page and carries an agent, a grant and an email. A request
	// without them is not ours, or is damaged: it is refused, never allowed because nothing matched.
	if agent == "" || grant == "" || email == "" || len(in.Request.GrantTypes) == 0 {
		mHook.WithLabelValues("incomplete").Inc()
		s.log().Warn("token hook: incomplete request refused", "client_id", in.Request.ClientID)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	stopped, err := s.Deny.Stopped(agent, grant)
	if err != nil {
		mHook.WithLabelValues("unreadable").Inc()
		s.log().Error("token hook: cannot read the stop list: the request is refused", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	off, err := s.Deny.Offboarded(email)
	if err != nil {
		mHook.WithLabelValues("unreadable").Inc()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if stopped || off {
		res := "stopped"
		if off {
			res = "offboarded"
		}
		mHook.WithLabelValues(res).Inc()
		s.log().Info("token hook: refused", "reason", res, "agent", agent, "grant", grant, "client_id", in.Request.ClientID)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	refresh := false
	for _, g := range in.Request.GrantTypes {
		refresh = refresh || g == "refresh_token"
	}
	if !refresh {
		mHook.WithLabelValues("allowed").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A refresh. A grant lives at most MaxGrantDays: the groups in the token are those of the sign-in, so this is how a group
	// that the person lost at the identity provider stops counting.
	if age, ok := GrantAge(grant, s.now()); !ok || (s.Cfg.MaxGrantDays > 0 && age > time.Duration(s.Cfg.MaxGrantDays)*24*time.Hour) {
		mHook.WithLabelValues("grant_too_old").Inc()
		s.log().Info("token hook: the grant is too old to be refreshed", "grant", grant)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// The agent must exist, be active and own this grant: deleting an agent (or ending its connection) ends its refresh for
	// good, without Hydra's help and after the stop list entry is gone.
	var scope []string
	if l, ok := in.Session.Extra["ambit_scope"].([]any); ok {
		for _, x := range l {
			if v, ok := x.(string); ok {
				scope = append(scope, v)
			}
		}
	}
	active, err := s.Backend.Active(r.Context(), agent, grant, scope)
	if err != nil {
		mHook.WithLabelValues("agent_unreadable").Inc()
		s.log().Error("token hook: the agent could not be checked: the request is refused", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !active {
		mHook.WithLabelValues("agent_inactive").Inc()
		s.log().Info("token hook: the agent is not active", "agent", agent, "grant", grant)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.touchClient(r, in.Request.ClientID)
	mHook.WithLabelValues("refreshed").Inc()
	w.WriteHeader(http.StatusNoContent)
}
