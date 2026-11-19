package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
)

// Service account endpoints (spec section 3.1): admins only, every write under the key writer Lease. Revoking a
// key uses the existing DELETE /api/admin/keys/{id} (and DELETE /api/keys/self for the key itself).

type serviceSettingsView struct {
	Enabled      bool `json:"enabled"` // false also when apiKeys.enabled is false
	MaxDays      int  `json:"maxDays"`
	ExposureDays int  `json:"exposureDays"`
	// LastUsedAvailable as for personal keys: without an audit trail no "not used" warning is possible.
	LastUsedAvailable bool `json:"lastUsedAvailable"`
}

type accountView struct {
	Name        string    `json:"name"`
	Identity    string    `json:"identity"`
	Group       string    `json:"group"`
	Description string    `json:"description"`
	Created     time.Time `json:"created"`
	CreatedBy   string    `json:"createdBy"`
	Keys        []KeyView `json:"keys"`
	Warnings    []string  `json:"warnings"`
}

// unusedAfter: a key not used for this long gets a warning on the account (decision SA-6 A: a warning only).
const unusedAfter = 30 * 24 * time.Hour

func (c *Catalog) registerServiceAccounts(mux *http.ServeMux, adminRead func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc,
	allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	const denied = "only admins can manage service accounts"
	mux.HandleFunc("GET /api/admin/service-accounts", adminRead(c.listAccounts))
	mux.HandleFunc("POST /api/admin/service-accounts", allow(denied, c.createAccount, RoleAdmin))
	mux.HandleFunc("DELETE /api/admin/service-accounts/{name}", allow(denied, c.deleteAccount, RoleAdmin))
	mux.HandleFunc("POST /api/admin/service-accounts/{name}/keys", allow(denied, c.createAccountKey, RoleAdmin))
}

func (c *Catalog) serviceSettings() serviceSettingsView {
	s := c.Keys.Services
	return serviceSettingsView{Enabled: s.Enabled && c.Keys.Settings.Enabled, MaxDays: s.MaxDays, ExposureDays: s.ExposureDays,
		LastUsedAvailable: c.lastUsedFunc() != nil}
}

func (c *Catalog) listAccounts(w http.ResponseWriter, r *http.Request, _ Session) {
	accts, err := c.Keys.Accounts(r.Context())
	if err != nil {
		keyErr(w, err)
		return
	}
	var all []Key
	for _, a := range accts {
		all = append(all, a.Keys...)
	}
	views := c.views(r.Context(), all, false, true)
	byID := map[string]KeyView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	settings, now := c.serviceSettings(), c.Keys.now()
	out := make([]accountView, 0, len(accts))
	for _, a := range accts {
		o := a.Owner
		v := accountView{Name: strings.TrimSuffix(o.Email, identity.ServiceDomain), Identity: o.Email, Description: o.Description,
			Created: o.Created, CreatedBy: o.CreatedBy, Keys: []KeyView{}, Warnings: []string{}}
		if len(o.Groups) > 0 {
			v.Group = o.Groups[0]
		}
		for _, k := range a.Keys {
			kv := byID[k.ID]
			v.Keys = append(v.Keys, kv)
			if kv.Status == "stopped" {
				continue
			}
			if !k.Exposed.IsZero() {
				v.Warnings = append(v.Warnings, fmt.Sprintf("Key %q was issued by a person who was offboarded. It stops on %s unless you rotate it.",
					k.Name, k.Until.UTC().Format("2006-01-02 15:04 UTC")))
			}
			if settings.LastUsedAvailable {
				last := k.Created
				if kv.LastUsed != nil {
					last = *kv.LastUsed
				}
				if d := now.Sub(last); d >= unusedAfter {
					v.Warnings = append(v.Warnings, fmt.Sprintf("Key %q not used for %d days.", k.Name, int(d.Hours()/24)))
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "accounts": out,
		"urls": map[string]string{"llm": c.LLMURL, "mcp": c.MCPURL}})
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	return d.Decode(v) == nil
}

func (c *Catalog) createAccount(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Name        string `json:"name"`
		Group       string `json:"group"`
		Description string `json:"description"`
	}
	if !decodeStrict(w, r, &in) {
		writeErr(w, http.StatusBadRequest, "send {name, group, description} as JSON")
		return
	}
	o, err := c.Keys.CreateAccount(r.Context(), s, in.Name, in.Group, in.Description)
	if err != nil {
		keyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": accountView{Name: in.Name, Identity: o.Email, Group: in.Group,
		Description: o.Description, Created: o.Created, CreatedBy: o.CreatedBy, Keys: []KeyView{}, Warnings: []string{}}})
}

func (c *Catalog) deleteAccount(w http.ResponseWriter, r *http.Request, s Session) {
	if err := c.Keys.DeleteAccount(r.Context(), s, r.PathValue("name")); err != nil {
		keyErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Catalog) createAccountKey(w http.ResponseWriter, r *http.Request, s Session) {
	var in struct {
		Name  string `json:"name"`
		Scope Scope  `json:"scope"`
		Days  int    `json:"days"`
	}
	if !decodeStrict(w, r, &in) {
		writeErr(w, http.StatusBadRequest, "send {name, scope, days} as JSON")
		return
	}
	plain, k, err := c.Keys.CreateAccountKey(r.Context(), s, r.PathValue("name"), in.Name, in.Scope, in.Days)
	if err != nil {
		keyErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the answer holds the key, shown once
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "view": c.views(r.Context(), []Key{k}, false, false)[0],
		"urls": map[string]string{"llm": c.LLMURL, "mcp": c.MCPURL}})
}
