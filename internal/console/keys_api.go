package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type keySettingsView struct {
	Enabled        bool `json:"enabled"`
	InactivityDays int  `json:"inactivityDays"`
	MaxDays        int  `json:"maxDays"`
}

// KeyView is what the console shows of a key: never the key itself or its hash.
type KeyView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Prefix   string     `json:"prefix"`
	Scope    Scope      `json:"scope"`
	Created  time.Time  `json:"created"`
	Expires  time.Time  `json:"expires"`
	Until    time.Time  `json:"until"`
	Owner    string     `json:"owner,omitempty"` // admin list only
	LastUsed *time.Time `json:"lastUsed"`        // null when unknown
}

func (c *Catalog) registerKeys(mux *http.ServeMux, read func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc,
	allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	if c.Keys == nil {
		return
	}
	adminRead := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return read(func(w http.ResponseWriter, r *http.Request, s Session) {
			if s.Role != RoleAdmin {
				writeErr(w, http.StatusForbidden, "only admins can see this")
				return
			}
			h(w, r, s)
		})
	}
	mux.HandleFunc("GET /api/settings/keys", read(func(w http.ResponseWriter, r *http.Request, _ Session) {
		writeJSON(w, http.StatusOK, c.keySettings())
	}))
	mux.HandleFunc("GET /api/keys", read(c.myKeys))
	mux.HandleFunc("POST /api/keys", allow("", c.createKey))
	mux.HandleFunc("DELETE /api/keys/{id}", allow("", func(w http.ResponseWriter, r *http.Request, s Session) { c.revokeKey(w, r, s, false) }))
	mux.HandleFunc("GET /api/admin/keys", adminRead(c.allKeys))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", allow("only admins can revoke other people's keys",
		func(w http.ResponseWriter, r *http.Request, s Session) { c.revokeKey(w, r, s, true) }, RoleAdmin))
	mux.HandleFunc("GET /api/admin/key-events", adminRead(func(w http.ResponseWriter, r *http.Request, _ Session) {
		ev, err := c.Keys.Events(r.Context())
		if err != nil {
			keyErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": ev})
	}))
	if os.Getenv("AMBIT_TEST_HOOKS") == "true" { // test clusters only (chart: console.testHooks)
		mux.HandleFunc("POST /api/test/keys/{id}/expire", allow("only admins can use test hooks", c.expireKey, RoleAdmin))
	}
}

func (c *Catalog) keySettings() keySettingsView {
	s := c.Keys.Settings
	return keySettingsView{Enabled: s.Enabled, InactivityDays: s.InactivityDays, MaxDays: s.MaxDays}
}

// keyErr maps the key service's errors to HTTP answers.
func keyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBadInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrKeysOff):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrOffboarded):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrTooMany), errors.Is(err, ErrTooFast):
		writeErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrNotLeader):
		w.Header().Set("Retry-After", "15")
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		serverErr(w, err)
	}
}

// views turns keys into KeyViews, adding each key's last use when the audit can tell. A failed lookup is
// logged and leaves lastUsed null: the list is still useful.
func (c *Catalog) views(ctx context.Context, keys []Key, withOwner bool) []KeyView {
	last := map[string]time.Time{}
	if lookup := c.lastUsedFunc(); lookup != nil && len(keys) > 0 {
		ids := make([]string, len(keys))
		for i, k := range keys {
			ids[i] = k.ID
		}
		var err error
		if last, err = lookup(ctx, ids); err != nil {
			slog.Warn("last use of keys unknown", "err", err)
		}
	}
	out := make([]KeyView, 0, len(keys))
	for _, k := range keys {
		v := KeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scope: k.Scope, Created: k.Created, Expires: k.Expires, Until: k.Until}
		if withOwner {
			v.Owner = k.Email
		}
		if t, ok := last[k.ID]; ok {
			v.LastUsed = &t
		}
		out = append(out, v)
	}
	return out
}

func (c *Catalog) lastUsedFunc() func(context.Context, []string) (map[string]time.Time, error) {
	if c.LastUsed != nil {
		return c.LastUsed
	}
	if c.Audit != nil {
		return c.Audit.KeysLastUsed
	}
	return nil
}

func (c *Catalog) myKeys(w http.ResponseWriter, r *http.Request, s Session) {
	keys, err := c.Keys.List(r.Context(), s.Email)
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": c.keySettings(), "keys": c.views(r.Context(), keys, false)})
}

func (c *Catalog) allKeys(w http.ResponseWriter, r *http.Request, _ Session) {
	keys, err := c.Keys.ListAll(r.Context())
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": c.keySettings(), "keys": c.views(r.Context(), keys, true)})
}

func (c *Catalog) createKey(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Name  string `json:"name"`
		Scope Scope  `json:"scope"`
		Days  int    `json:"days"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "send {name, scope, days} as JSON")
		return
	}
	plain, k, err := c.Keys.Create(r.Context(), s, in.Name, in.Scope, in.Days)
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "view": c.views(r.Context(), []Key{k}, false)[0],
		"snippets": snippets(c.LLMURL, c.MCPURL, plain, k.Scope)})
}

func (c *Catalog) revokeKey(w http.ResponseWriter, r *http.Request, s Session, admin bool) {
	if err := c.Keys.Revoke(r.Context(), s, r.PathValue("id"), admin); err != nil {
		keyErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// expireKey is the acceptance-test hook: it moves a key's until to the given time.
func (c *Catalog) expireKey(w http.ResponseWriter, r *http.Request, _ Session) {
	var in struct {
		Until string `json:"until"`
	}
	until, err := time.Parse(time.RFC3339, "")
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) == nil {
		until, err = time.Parse(time.RFC3339, in.Until)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, `send {"until":"<RFC3339 time>"}`)
		return
	}
	if err := c.Keys.ExpireForTest(r.Context(), r.PathValue("id"), until); err != nil {
		keyErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// snippets are copy-ready settings for each client, holding the new key. The LLM ones need llm scope, MCP needs mcp.
func snippets(llm, mcp, key string, scope Scope) map[string]string {
	m := map[string]string{}
	if scope != ScopeMCP {
		m["claudeCode"] = fmt.Sprintf(`{"env":{"ANTHROPIC_BASE_URL":%q,"ANTHROPIC_AUTH_TOKEN":%q}}`, llm, key)
		m["cursor"] = "OpenAI API Key: " + key + "\nOverride OpenAI Base URL: " + llm + "/v1"
		m["openai"] = "export OPENAI_BASE_URL=" + llm + "/v1\nexport OPENAI_API_KEY=" + key
		m["anthropic"] = "export ANTHROPIC_BASE_URL=" + llm + "\nexport ANTHROPIC_API_KEY=" + key
	}
	if scope != ScopeLLM {
		m["mcp"] = "claude mcp add --transport http ambit " + mcp + ` --header "Authorization: Bearer ` + key + `"`
	}
	return m
}
