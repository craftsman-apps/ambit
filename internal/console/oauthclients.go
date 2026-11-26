package console

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/craftsman-apps/ambit/internal/oauth"
)

// The OAuth clients of platforms (docs/oauth.md, "Platforms that are connected"), EXPERIMENTAL in 0.6.x. An admin lists them and deletes
// one to keep a platform out: its codes and tokens go with it, and a new sign-in of that client is refused. Only with OAuth turned on.

type oauthClientView struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"` // as the platform declared it
	RedirectHosts []string  `json:"redirectHosts"`
	Via           string    `json:"registeredVia"` // dcr (the platform registered itself) or another
	LastUsed      time.Time `json:"lastUsed,omitempty"`
	Created       time.Time `json:"created,omitempty"`
}

func (c *Catalog) registerOAuth(mux *http.ServeMux, allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/oauth/clients", allow("only admins can see OAuth clients", c.listOAuthClients, RoleAdmin))
	mux.HandleFunc("DELETE /api/admin/oauth/clients/{id}", allow("only admins can delete OAuth clients", c.deleteOAuthClient, RoleAdmin))
}

func (c *Catalog) listOAuthClients(w http.ResponseWriter, r *http.Request, _ Session) {
	if c.Hydra == nil {
		writeErr(w, http.StatusNotFound, "OAuth sign-in is not turned on")
		return
	}
	out := []oauthClientView{}
	err := c.Hydra.ListClients(r.Context(), func(cl oauth.Client) error {
		v := oauthClientView{ID: cl.ID, Name: cl.Name, RedirectHosts: []string{}, Created: cl.CreatedAt}
		seen := map[string]bool{}
		for _, u := range cl.RedirectURIs {
			if h := hostOf(u); h != "" && !seen[h] {
				seen[h] = true
				v.RedirectHosts = append(v.RedirectHosts, h)
			}
		}
		v.Via, _ = cl.Metadata["registered_via"].(string)
		if s, ok := cl.Metadata["last_used"].(string); ok {
			v.LastUsed, _ = time.Parse(time.RFC3339, s)
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the OAuth server did not answer")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

func (c *Catalog) deleteOAuthClient(w http.ResponseWriter, r *http.Request, s Session) {
	if c.Hydra == nil {
		writeErr(w, http.StatusNotFound, "OAuth sign-in is not turned on")
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 {
		writeErr(w, http.StatusBadRequest, "no client")
		return
	}
	if err := c.Hydra.DeleteClient(r.Context(), id); err != nil && !errors.Is(err, oauth.ErrUnknown) {
		writeErr(w, http.StatusBadGateway, "the OAuth server did not answer")
		return
	}
	c.Keys.event(r.Context(), s.Email, "oauth-client-delete", "", "", "client="+id)
	w.WriteHeader(http.StatusNoContent)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
