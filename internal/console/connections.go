package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Connections lets users connect SaaS apps with their own accounts (journeys U1, U4) and gives the
// gateway each user's token through the broker. Grants never leave the console except as the
// Authorization header of that user's own requests.
type Connections struct {
	Client    client.Client // Connection objects in Namespace
	Namespace string
	Store     *GrantStore // grants and Connection client secrets, in Store.Namespace
	PublicURL string
	Codec     Codec // signs the short-lived OAuth state cookie
	HTTP      *http.Client
	Now       func() time.Time

	mu sync.Mutex // one refresh per grant at a time (single console replica)
}

const connCookie = "ambit_connect"

type ConnectionView struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	State       string    `json:"state"` // "not-connected", "connected" or "broken"
	Error       string    `json:"error,omitempty"`
	ConnectedAt time.Time `json:"connectedAt,omitzero"`
	CheckedAt   time.Time `json:"checkedAt,omitzero"`
	Revocable   bool      `json:"revocable"`
	ConnectURL  string    `json:"connectURL"`
	Servers     []string  `json:"servers"` // MCP servers that use this connection
}

func (c *Connections) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Connections) connectURL(name string) string {
	return strings.TrimSuffix(c.PublicURL, "/") + "/connections/" + url.PathEscape(name) + "/connect"
}

func (c *Connections) connection(ctx context.Context, name string) (*v1.Connection, error) {
	var conn v1.Connection
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, &conn); err != nil {
		return nil, err
	}
	return &conn, nil
}

// oauthConfig reads the app's client secret from the grants namespace, the only place the console
// may read Secrets freely.
func (c *Connections) oauthConfig(ctx context.Context, conn *v1.Connection) (*oauth2.Config, error) {
	var sec corev1.Secret
	if err := c.Store.Client.Get(ctx, client.ObjectKey{Namespace: c.Store.Namespace, Name: conn.Spec.ClientSecretRef.Name}, &sec); err != nil {
		return nil, fmt.Errorf("client secret %s/%s: %w", c.Store.Namespace, conn.Spec.ClientSecretRef.Name, err)
	}
	return &oauth2.Config{ClientID: conn.Spec.ClientID, ClientSecret: string(sec.Data["clientSecret"]),
		Endpoint:    oauth2.Endpoint{AuthURL: conn.Spec.AuthorizationURL, TokenURL: conn.Spec.TokenURL},
		RedirectURL: strings.TrimSuffix(c.PublicURL, "/") + "/connections/callback", Scopes: conn.Spec.Scopes}, nil
}

func (c *Connections) oauthCtx(ctx context.Context) context.Context {
	if c.HTTP != nil {
		return context.WithValue(ctx, oauth2.HTTPClient, c.HTTP)
	}
	return ctx
}

func (c *Connections) register(mux *http.ServeMux, auth *Auth) {
	signedIn := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s, ok := auth.Current(r)
			if !ok {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeErr(w, http.StatusUnauthorized, "not signed in")
				} else {
					http.Redirect(w, r, "/login", http.StatusFound)
				}
				return
			}
			h(w, r, s)
		}
	}
	mux.HandleFunc("GET /api/connections", signedIn(c.list))
	mux.HandleFunc("DELETE /api/connections/{name}", signedIn(c.disconnect))
	mux.HandleFunc("POST /api/connections/check", signedIn(func(w http.ResponseWriter, r *http.Request, s Session) {
		if s.Role != RoleAdmin {
			writeErr(w, http.StatusForbidden, "only admins can start a check")
			return
		}
		n, broken := c.CheckAll(r.Context(), true)
		writeJSON(w, http.StatusOK, map[string]int{"checked": n, "broken": broken})
	}))
	mux.HandleFunc("GET /connections/{name}/connect", signedIn(c.start))
	mux.HandleFunc("GET /connections/callback", signedIn(c.callback))
}

func (c *Connections) list(w http.ResponseWriter, r *http.Request, s Session) {
	var conns v1.ConnectionList
	if err := c.Client.List(r.Context(), &conns, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	var servers v1.ServerList
	if err := c.Client.List(r.Context(), &servers, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	grants, err := c.Store.List(r.Context(), s.Email)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := []ConnectionView{}
	for _, conn := range conns.Items {
		v := ConnectionView{Name: conn.Name, DisplayName: conn.Spec.DisplayName, State: "not-connected",
			Revocable: conn.Spec.RevocationURL != "", ConnectURL: c.connectURL(conn.Name), Servers: []string{}}
		for _, g := range grants {
			if g.Connection == conn.Name {
				v.State, v.Error, v.ConnectedAt, v.CheckedAt = g.State, g.Error, g.ConnectedAt, g.CheckedAt
			}
		}
		for _, srv := range servers.Items {
			if srv.Spec.Connection == conn.Name {
				v.Servers = append(v.Servers, srv.Name)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

var errAppChanged = errors.New("this app changed while you were connecting; connect again")

type connectState struct {
	State, Verifier, Connection string
	Exp                         int64
	// The configuration that started the flow; the callback refuses if any of it changed.
	ConnUID  string
	ConnGen  int64
	SecretRV string
}

func (c *Connections) secretRV(ctx context.Context, conn *v1.Connection) (string, error) {
	var sec corev1.Secret
	if err := c.Store.Client.Get(ctx, client.ObjectKey{Namespace: c.Store.Namespace, Name: conn.Spec.ClientSecretRef.Name}, &sec); err != nil {
		return "", err
	}
	return sec.ResourceVersion, nil
}

// snapshotMatches returns the Connection only if it is the one that started the flow, unchanged.
func (c *Connections) snapshotMatches(ctx context.Context, st connectState) (*v1.Connection, error) {
	conn, err := c.connection(ctx, st.Connection)
	if err != nil {
		return nil, err
	}
	rv, err := c.secretRV(ctx, conn)
	if err != nil {
		return nil, err
	}
	if string(conn.UID) != st.ConnUID || conn.Generation != st.ConnGen || rv != st.SecretRV {
		return nil, errAppChanged
	}
	return conn, nil
}

// start sends the user to the provider to approve access (authorization code with PKCE).
func (c *Connections) start(w http.ResponseWriter, r *http.Request, s Session) {
	conn, err := c.connection(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, "no connection named "+r.PathValue("name"), http.StatusNotFound)
		return
	}
	cfg, err := c.oauthConfig(r.Context(), conn)
	if err != nil {
		slog.Error("connection config", "connection", conn.Name, "err", err)
		http.Error(w, "this connection is not set up completely; ask an admin", http.StatusInternalServerError)
		return
	}
	rv, err := c.secretRV(r.Context(), conn)
	if err != nil {
		slog.Error("connection secret", "connection", conn.Name, "err", err)
		http.Error(w, "this connection is not set up completely; ask an admin", http.StatusInternalServerError)
		return
	}
	st := connectState{State: randomString(), Verifier: oauth2.GenerateVerifier(), Connection: conn.Name, Exp: c.now().Add(10 * time.Minute).Unix(),
		ConnUID: string(conn.UID), ConnGen: conn.Generation, SecretRV: rv}
	http.SetCookie(w, &http.Cookie{Name: connCookie, Value: c.Codec.Encode(st), Path: "/connections/", HttpOnly: true,
		Secure: strings.HasPrefix(c.PublicURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(st.Verifier)}
	if conn.Spec.Resource != "" {
		opts = append(opts, oauth2.SetAuthURLParam("resource", conn.Spec.Resource))
	}
	keys := make([]string, 0, len(conn.Spec.AuthorizeParams))
	for k := range conn.Spec.AuthorizeParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		opts = append(opts, oauth2.SetAuthURLParam(k, conn.Spec.AuthorizeParams[k]))
	}
	http.Redirect(w, r, cfg.AuthCodeURL(st.State, opts...), http.StatusFound)
}

func (c *Connections) callback(w http.ResponseWriter, r *http.Request, s Session) {
	ck, err := r.Cookie(connCookie)
	var st connectState
	if err == nil {
		err = c.Codec.Decode(ck.Value, &st)
	}
	if err != nil || st.State == "" || st.State != r.URL.Query().Get("state") || c.now().Unix() > st.Exp {
		http.Error(w, "this connect link expired or did not start here; start again from My connections", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: connCookie, Value: "", Path: "/connections/", MaxAge: -1})
	if e := r.URL.Query().Get("error"); e != "" {
		http.Redirect(w, r, "/connections?error="+url.QueryEscape(e), http.StatusFound)
		return
	}
	conn, err := c.snapshotMatches(r.Context(), st)
	if errors.Is(err, errAppChanged) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "the connection was removed", http.StatusNotFound)
		return
	}
	cfg, err := c.oauthConfig(r.Context(), conn)
	if err != nil {
		http.Error(w, "this connection is not set up completely; ask an admin", http.StatusInternalServerError)
		return
	}
	tok, err := cfg.Exchange(c.oauthCtx(r.Context()), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		slog.Warn("connect: code exchange failed", "connection", conn.Name, "user", s.Email, "err", err)
		http.Redirect(w, r, "/connections?error="+url.QueryEscape("the provider refused the approval; try again"), http.StatusFound)
		return
	}
	now := c.now()
	g := &Grant{Email: s.Email, Connection: conn.Name, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		Expiry: tok.Expiry, State: grantConnectedState, ConnectedAt: now, CheckedAt: now,
		Resource: conn.Spec.Resource, ServerURL: conn.Spec.ServerURL}
	if err := c.Store.Put(r.Context(), g); err != nil {
		serverErr(w, err)
		return
	}
	slog.Info("connection added", "connection", conn.Name, "user", s.Email)
	http.Redirect(w, r, "/connections?connected="+url.QueryEscape(conn.Name), http.StatusFound)
}

// Revoke asks the provider to revoke the grant (RFC 7009), then forgets it. It reports whether the
// provider confirmed; offboarding (step 6) logs that answer as proof.
// Revoke ends a grant at its provider (in the provider's format, see revoke.go) and deletes Ambit's copy.
// It holds the connections lock throughout and re-reads the grant, so a renewal running at the same time
// cannot be overwritten or leave a newer token unrevoked. Errors never contain the grant's tokens.
func (c *Connections) Revoke(ctx context.Context, g *Grant) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, err := c.Store.Get(ctx, g.Email, g.Connection); err == nil && cur != nil {
		g = cur
	}
	conn, err := c.connection(ctx, g.Connection)
	revoked := false
	if err == nil && conn.Spec.RevocationURL != "" {
		cfg, err := c.oauthConfig(ctx, conn)
		if err != nil {
			return false, err
		}
		// Formats that send the access token need a current one: renew an expired token first. If renewal
		// fails, the old token is sent and the provider's refusal is reported.
		if revokesAccessToken(conn.Spec.RevocationURL) && g.RefreshToken != "" && !g.Expiry.IsZero() && !c.now().Before(g.Expiry) {
			if _, renewed, rerr := c.refresh(ctx, g); rerr == nil && renewed != nil && renewed.State == grantConnectedState {
				g = renewed
			}
		}
		fail := func(err error) (bool, error) {
			return false, fmt.Errorf("revoke at %s: %s", conn.Spec.RevocationURL, redactTokens(err.Error(), g))
		}
		req, check, err := revocationRequest(ctx, conn.Spec.RevocationURL, cfg.ClientID, cfg.ClientSecret, g)
		if err != nil {
			return fail(err)
		}
		hc := c.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 10 * time.Second}
		}
		resp, err := hc.Do(req)
		if err != nil {
			return fail(err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err := check(resp.StatusCode, body); err != nil {
			return fail(err)
		}
		revoked = true
	}
	return revoked, c.Store.Delete(ctx, g.Email, g.Connection)
}

func (c *Connections) disconnect(w http.ResponseWriter, r *http.Request, s Session) {
	g, err := c.Store.Get(r.Context(), s.Email, r.PathValue("name"))
	if err != nil {
		serverErr(w, err)
		return
	}
	if g == nil {
		writeErr(w, http.StatusNotFound, "you have not connected "+r.PathValue("name"))
		return
	}
	revoked, err := c.Revoke(r.Context(), g)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the provider did not confirm the revocation, so the grant was kept: "+err.Error())
		return
	}
	slog.Info("connection removed", "connection", g.Connection, "user", s.Email, "revokedAtProvider", revoked)
	writeJSON(w, http.StatusOK, map[string]bool{"revokedAtProvider": revoked})
}

// Token returns a valid access token for the user, refreshing it when it is close to expiry. A
// failed refresh marks the grant broken, so the user sees "reconnect" before their agent fails again.
func (c *Connections) Token(ctx context.Context, email, connName string) (string, *Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A removed app hands out nothing, even while a stored token has not expired.
	conn, err := c.connection(ctx, connName)
	if apierrors.IsNotFound(err) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	g, err := c.Store.Get(ctx, email, connName)
	if err != nil || g == nil || g.State == grantBrokenState {
		return "", g, err
	}
	if g.Resource != conn.Spec.Resource || g.ServerURL != conn.Spec.ServerURL {
		g.State, g.Error = grantBrokenState, "the app's server changed; reconnect"
		return "", g, c.Store.Put(ctx, g)
	}
	if g.Expiry.IsZero() || c.now().Add(time.Minute).Before(g.Expiry) {
		return g.AccessToken, g, nil
	}
	return c.refresh(ctx, g)
}

func (c *Connections) refresh(ctx context.Context, g *Grant) (string, *Grant, error) {
	conn, err := c.connection(ctx, g.Connection)
	if err != nil {
		return "", g, err
	}
	cfg, err := c.oauthConfig(ctx, conn)
	if err != nil {
		return "", g, err
	}
	g.CheckedAt = c.now()
	tok, err := cfg.TokenSource(c.oauthCtx(ctx), &oauth2.Token{RefreshToken: g.RefreshToken, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		g.State, g.Error = grantBrokenState, "the provider refused to renew access: "+short(err.Error())
		slog.Warn("grant broken", "connection", g.Connection, "user", g.Email, "err", err)
		return "", g, c.Store.Put(ctx, g)
	}
	g.AccessToken, g.Expiry, g.State, g.Error = tok.AccessToken, tok.Expiry, grantConnectedState, ""
	if tok.RefreshToken != "" {
		g.RefreshToken = tok.RefreshToken
	}
	return g.AccessToken, g, c.Store.Put(ctx, g)
}

func short(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// CheckAll renews every grant that would expire before the next check (all of them when force is
// set), so a broken grant is found overnight rather than in the middle of a task.
func (c *Connections) CheckAll(ctx context.Context, force bool) (checked, broken int) {
	grants, err := c.Store.List(ctx, "")
	if err != nil {
		slog.Error("grant check", "err", err)
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range grants {
		if g.State == grantBrokenState {
			broken++
			continue
		}
		if !force && c.now().Add(CheckInterval+5*time.Minute).Before(g.Expiry) {
			continue
		}
		checked++
		if _, g2, _ := c.refresh(ctx, g); g2 != nil && g2.State == grantBrokenState {
			broken++
		}
	}
	return checked, broken
}

// CheckInterval is how often RunChecks renews grants.
var CheckInterval = time.Hour

func (c *Connections) RunChecks(ctx context.Context) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, b := c.CheckAll(ctx, false)
			slog.Info("grant check", "renewed", n, "broken", b)
		}
	}
}
