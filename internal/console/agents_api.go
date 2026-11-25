package console

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
)

// Agent endpoints (docs/agents.md), EXPERIMENTAL in 0.6.x. Any signed-in person creates and manages person-mode agents
// for themselves; admins create and manage itself-mode agents and may delete anyone's. Every write runs under the key
// writer Lease. A key of an agent is revoked with the ordinary key endpoints (DELETE /api/keys/{id}).

type agentView struct {
	Name              string    `json:"name"`
	DisplayName       string    `json:"displayName"`
	Owner             string    `json:"owner"`
	Mode              string    `json:"mode"`
	Group             string    `json:"group,omitempty"`
	Identity          string    `json:"identity,omitempty"` // itself mode: the service account the agent calls as
	Platform          string    `json:"platform"`
	Scope             []string  `json:"scope"`
	ConnectionConsent []string  `json:"connectionConsent"`
	State             string    `json:"state"`
	Rev               int       `json:"rev"`
	Created           time.Time `json:"created"`
	CreatedBy         string    `json:"createdBy"`
	// Manageable: the caller may change it (the owner in person mode, an admin in itself mode).
	Manageable bool `json:"manageable"`
	// Stop (docs/agents.md): see the Agent record. Stopped only when every Gateway confirmed it.
	StopRequestedAt    *time.Time `json:"stopRequestedAt,omitempty"`
	StoppedAt          *time.Time `json:"stoppedAt,omitempty"`
	StopUnlisted       bool       `json:"stopUnlisted,omitempty"`
	StopNotConfirmedAt *time.Time `json:"stopNotConfirmedAt,omitempty"`
	OrphanedAt         *time.Time `json:"orphanedAt,omitempty"`
	Keys               *[]KeyView `json:"keys,omitempty"` // the detail only
}

func (c *Catalog) agentView(a Agent, s Session) agentView {
	actor, _ := identity.CanonicalEmail(s.Email)
	v := agentView{Name: a.Name, DisplayName: a.DisplayName, Owner: a.Owner, Mode: a.Mode, Group: a.Group, Platform: a.Platform,
		Scope: nonNil(a.Scope), ConnectionConsent: nonNil(a.ConnectionConsent), State: a.State, Rev: a.Rev, Created: a.Created, CreatedBy: a.CreatedBy,
		Manageable: canManage(s, a, actor) == nil, StopRequestedAt: a.StopRequestedAt, StoppedAt: a.StoppedAt, StopUnlisted: a.StopUnlisted,
		StopNotConfirmedAt: a.StopNotConfirmedAt, OrphanedAt: a.OrphanedAt}
	if a.Mode == ModeItself {
		v.Identity = AccountIdentity(a.Name)
	}
	return v
}

// decodeBody reads one JSON object of at most limit bytes into v, refusing unknown fields and anything after the
// object (a second value, or an oversized suffix).
func decodeBody(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func (c *Catalog) registerAgents(mux *http.ServeMux, read func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc,
	allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	mux.HandleFunc("GET /api/agents", read(c.listAgents))
	mux.HandleFunc("POST /api/agents", allow("", c.createAgent))
	mux.HandleFunc("GET /api/agents/{name}", read(c.getAgent))
	mux.HandleFunc("PATCH /api/agents/{name}", allow("", c.patchAgent))
	mux.HandleFunc("DELETE /api/agents/{name}", allow("", c.deleteAgent))
	mux.HandleFunc("POST /api/agents/{name}/keys", allow("", c.createAgentKey))
	mux.HandleFunc("POST /api/agents/{name}/stop", allow("", c.stopAgent))
	mux.HandleFunc("POST /api/agents/{name}/start", allow("", c.startAgent))
	mux.HandleFunc("PUT /api/agents/{name}/owner", allow("only admins can move an agent", c.moveAgent, RoleAdmin))
}

type agentSettingsView struct {
	Enabled      bool `json:"enabled"` // false also when apiKeys.enabled is false
	MaxTotal     int  `json:"maxTotal"`
	MaxPerPerson int  `json:"maxPerPerson"`
	KeyMaxDays   int  `json:"keyMaxDays"`
}

// listAgents answers GET /api/agents: the caller's agents; admins add ?all=true for every agent.
func (c *Catalog) listAgents(w http.ResponseWriter, r *http.Request, s Session) {
	var as []Agent
	var err error
	if r.URL.Query().Get("all") == "true" {
		if s.Role != RoleAdmin {
			writeErr(w, http.StatusForbidden, "only admins can list every agent")
			return
		}
		as, err = c.Keys.AllAgents(r.Context())
	} else {
		as, err = c.Keys.AgentsOf(r.Context(), s.Email)
	}
	if err != nil {
		keyErr(w, err)
		return
	}
	out := make([]agentView, 0, len(as))
	for _, a := range as {
		out = append(out, c.agentView(a, s))
	}
	ag := c.Keys.Agents
	writeJSON(w, http.StatusOK, map[string]any{"agents": out,
		"settings": agentSettingsView{Enabled: ag.Enabled && c.Keys.Settings.Enabled, MaxTotal: ag.MaxTotal, MaxPerPerson: ag.MaxPerPerson, KeyMaxDays: ag.KeyMaxDays}})
}

func (c *Catalog) getAgent(w http.ResponseWriter, r *http.Request, s Session) {
	a, err := c.Keys.GetAgent(r.Context(), r.PathValue("name"))
	if err == nil {
		actor, _ := identity.CanonicalEmail(s.Email)
		if s.Role != RoleAdmin && a.Owner != actor {
			err = ErrNoAgent // other people's agents are not confirmed
		}
	}
	if err != nil {
		keyErr(w, err)
		return
	}
	keys, err := c.Keys.AgentKeys(r.Context(), a.Name)
	if err == nil { // the agent may have moved while the keys were read: authorize again against the record as it is now
		var again Agent
		if again, err = c.Keys.GetAgent(r.Context(), a.Name); err == nil {
			if actor, _ := identity.CanonicalEmail(s.Email); s.Role != RoleAdmin && again.Owner != actor {
				err = ErrNoAgent
			}
			a = again
		}
	}
	if err != nil {
		keyErr(w, err)
		return
	}
	v := c.agentView(a, s)
	kv := c.views(r.Context(), keys, s.Role == RoleAdmin, true)
	v.Keys = &kv
	writeJSON(w, http.StatusOK, v)
}

func (c *Catalog) createAgent(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Mode              string   `json:"mode"`
		DisplayName       string   `json:"displayName"`
		Platform          string   `json:"platform"`
		Scope             []string `json:"scope"`
		ConnectionConsent []string `json:"connectionConsent"`
		Name              string   `json:"name"`
		Group             string   `json:"group"`
		Owner             string   `json:"owner"`
	}
	if err := decodeBody(w, r, &in, 64<<10); err != nil {
		writeErr(w, http.StatusBadRequest, "send {displayName, platform, scope, connectionConsent} as JSON; admins add mode, name, group, owner for an itself-mode agent")
		return
	}
	a, err := c.Keys.CreateAgent(r.Context(), s, AgentInput{Mode: in.Mode, DisplayName: in.DisplayName, Platform: in.Platform, Scope: in.Scope,
		Consent: in.ConnectionConsent, Name: in.Name, Group: in.Group, Owner: in.Owner})
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c.agentView(a, s))
}

func (c *Catalog) patchAgent(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		DisplayName       *string   `json:"displayName"`
		Platform          *string   `json:"platform"`
		Scope             *[]string `json:"scope"`
		ConnectionConsent *[]string `json:"connectionConsent"`
	}
	if err := decodeBody(w, r, &in, 64<<10); err != nil {
		writeErr(w, http.StatusBadRequest, "send any of {displayName, platform, scope, connectionConsent} as JSON")
		return
	}
	a, err := c.Keys.UpdateAgent(r.Context(), s, r.PathValue("name"), AgentPatch{DisplayName: in.DisplayName, Platform: in.Platform, Scope: in.Scope, Consent: in.ConnectionConsent})
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.agentView(a, s))
}

func (c *Catalog) deleteAgent(w http.ResponseWriter, r *http.Request, s Session) {
	if err := c.Keys.DeleteAgent(r.Context(), s, r.PathValue("name")); err != nil {
		keyErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Catalog) createAgentKey(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Name         string   `json:"name"`
		Days         int      `json:"days"`
		SourceRanges []string `json:"sourceRanges"`
	}
	if err := decodeBody(w, r, &in, 8192); err != nil {
		writeErr(w, http.StatusBadRequest, "send {name, days, sourceRanges} as JSON")
		return
	}
	plain, k, err := c.Keys.CreateAgentKey(r.Context(), s, r.PathValue("name"), in.Name, in.Days, in.SourceRanges)
	if err != nil {
		keyErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the answer holds the key, shown once
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "view": c.views(r.Context(), []Key{k}, false, false)[0],
		"urls": map[string]string{"mcp": c.MCPURL}})
}

func (c *Catalog) moveAgent(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Owner string `json:"owner"`
	}
	if err := decodeBody(w, r, &in, 4096); err != nil {
		writeErr(w, http.StatusBadRequest, `send {"owner": "<email or group:name>"} as JSON`)
		return
	}
	a, err := c.Keys.MoveAgent(r.Context(), s, r.PathValue("name"), in.Owner)
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.agentView(a, s))
}

// stopAgent answers POST /api/agents/{name}/stop with 202 and the agent in state stopping (or stopped, when every Gateway
// had confirmed already). The state changes to stopped when every Gateway acknowledged the stop: poll the agent.
func (c *Catalog) stopAgent(w http.ResponseWriter, r *http.Request, s Session) {
	a, err := c.Keys.StopAgent(r.Context(), s, r.PathValue("name"))
	// 202 only when the stop is on record (state stopping or stopped): then every step is repeated until it works
	if err != nil && a.State != StateStopping && a.State != StateStopped {
		keyErr(w, err)
		return
	}
	if err != nil { // the stop is recorded and goes on; the caller learns that a step is still to be repeated
		w.Header().Set("Retry-After", "5")
		slog.Warn("stop not complete yet", "agent", a.Name, "err", err)
	}
	writeJSON(w, http.StatusAccepted, c.agentView(a, s))
}

func (c *Catalog) startAgent(w http.ResponseWriter, r *http.Request, s Session) {
	a, err := c.Keys.StartAgent(r.Context(), s, r.PathValue("name"))
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.agentView(a, s))
}
