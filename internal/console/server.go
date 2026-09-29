package console

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// Connector is a sign-in option shown on the login page. It mirrors a Dex connector.
type Connector struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LoginOptions tells the login page which buttons to show. It holds no secrets.
type LoginOptions struct {
	Bootstrap bool        `json:"bootstrap"`
	SSO       []Connector `json:"sso"`
}

// ParseConnectors reads AMBIT_SSO_CONNECTORS, a JSON list rendered by the chart.
func ParseConnectors(s string) ([]Connector, error) {
	out := []Connector{}
	if s == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("AMBIT_SSO_CONNECTORS is not valid JSON: %w", err)
	}
	for _, c := range out {
		if c.ID == "" {
			return nil, fmt.Errorf("AMBIT_SSO_CONNECTORS: a connector has no id")
		}
	}
	return out, nil
}

// NewServer routes the console. spa holds the built React app (index.html, assets/).
func NewServer(a *Auth, spa fs.FS, opts LoginOptions, catalog *Catalog) http.Handler {
	mux := http.NewServeMux()
	static := http.FileServerFS(spa)
	index := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, spa, "index.html")
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /auth/login", a.Login)
	mux.HandleFunc("GET /auth/callback", a.Callback)
	mux.HandleFunc("/auth/logout", a.Logout)
	mux.Handle("GET /assets/", static)
	mux.HandleFunc("GET /login", index)

	mux.HandleFunc("GET /api/login-options", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(opts)
	})
	if catalog != nil {
		catalog.register(mux, a)
	}
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.Current(r)
		if !ok {
			http.Error(w, `{"error":"not signed in"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.Current(r); !ok {
			http.Error(w, `{"error":"not signed in"}`, http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	})

	// Every other page belongs to the React app and needs a session.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		if strings.Contains(r.URL.Path, ".") { // favicon.svg and other public build files
			static.ServeHTTP(w, r)
			return
		}
		if _, ok := a.Current(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		index(w, r)
	})
	return mux
}
