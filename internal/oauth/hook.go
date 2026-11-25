package oauth

import (
	"encoding/json"
	"io"
	"net/http"
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
	if email == "" {
		email = in.Session.Subject
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
	if !refresh || email == "" {
		mHook.WithLabelValues("allowed").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A refresh: the groups are read again, so a change at the identity provider reaches the token within one refresh.
	groups, err := s.Backend.Groups(r.Context(), email)
	if err != nil {
		mHook.WithLabelValues("groups_unavailable").Inc()
		s.log().Error("token hook: groups not available", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	extra := map[string]any{}
	for k, v := range in.Session.Extra {
		extra[k] = v
	}
	extra["groups"] = groups
	s.touchClient(r, in.Request.ClientID)
	mHook.WithLabelValues("refreshed").Inc()
	writeJSON(w, http.StatusOK, map[string]any{"session": map[string]any{"access_token": extra}})
}
