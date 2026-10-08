package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ToolReader reads the tools of per-user servers with one admin's own grant, given with their consent.
// The token never leaves the console: the controller receives the tool list only (port 8083).
type ToolReader struct {
	Conns     *Connections
	Namespace string
	List      func(ctx context.Context, hc *http.Client, endpoint, transport, bearer string) ([]v1.Tool, error)
	TTL       time.Duration

	consent sync.RWMutex // write: SetReader/ClearReader; read: a whole Tools call, so consent cannot change mid-read
	mu      sync.Mutex
	gen     uint64                 // bumped by every consent change
	cache   map[string]ToolsResult // key: server UID + "\n" + URL + "\n" + reader
}

type ToolsResult struct {
	Tools  []v1.Tool `json:"tools"`
	Reader string    `json:"reader"`
	ReadAt time.Time `json:"readAt"`
}

var (
	ErrNoReader = errors.New("no reader")
	errNotBound = errors.New("server is not bound to its connection; set the app's server URL")
	errNoServer = errors.New("no such server")
)

// consentErr is a reason the signed-in admin cannot give consent (not connected, connected for another URL).
type consentErr string

func (e consentErr) Error() string { return string(e) }

func (t *ToolReader) server(ctx context.Context, name string) (*v1.Server, error) {
	var s v1.Server
	err := t.Conns.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return nil, errNoServer
	}
	return &s, err
}

// bound: the reader's token may only go to the URL its Connection is bound to.
func (t *ToolReader) bound(ctx context.Context, s *v1.Server) error {
	conn, err := t.Conns.connection(ctx, s.Spec.Connection)
	if err != nil {
		return err
	}
	if conn.Spec.ServerURL == "" || conn.Spec.ServerURL != s.Spec.URL {
		return errNotBound
	}
	return nil
}

func (t *ToolReader) changed() {
	t.mu.Lock()
	t.gen++
	t.cache = nil
	t.mu.Unlock()
}

// usableFor: a connected grant issued for this server's URL.
func usableFor(g *Grant, s *v1.Server) error {
	if g == nil || g.State != grantConnectedState {
		return consentErr("connect " + s.Spec.Connection + " with your own account first")
	}
	if g.ServerURL != s.Spec.URL {
		return consentErr("your connection to " + s.Spec.Connection + " was made for another server; reconnect it")
	}
	return nil
}

// editGrant changes one grant under its own lock and re-reads it there, so a renewal or revocation running
// at the same time is neither overwritten nor undone. A grant that is gone is left alone.
func (t *ToolReader) editGrant(ctx context.Context, email, conn string, edit func(*Grant) error) error {
	defer t.Conns.lockGrant(email, conn)()
	g, err := t.Conns.Store.Get(ctx, email, conn)
	if err != nil {
		return err
	}
	if err := edit(g); err != nil || g == nil {
		return err
	}
	return t.Conns.Store.Put(ctx, g)
}

// SetReader records email's consent for server, replacing any other reader. The API passes only the
// signed-in admin's own email.
func (t *ToolReader) SetReader(ctx context.Context, email, server string) error {
	t.consent.Lock()
	defer t.consent.Unlock()
	defer t.changed()
	s, err := t.server(ctx, server)
	if err != nil {
		return err
	}
	if err := t.bound(ctx, s); err != nil {
		return err
	}
	g, err := t.Conns.Store.Get(ctx, email, s.Spec.Connection)
	if err != nil {
		return err
	}
	if err := usableFor(g, s); err != nil { // checked first, so a failed set does not clear the current reader
		return err
	}
	if err := t.clear(ctx, s, email); err != nil {
		return err
	}
	return t.editGrant(ctx, email, s.Spec.Connection, func(g *Grant) error {
		if err := usableFor(g, s); err != nil { // re-checked under the grant's lock
			return err
		}
		if !slices.Contains(g.ToolReaderFor, string(s.UID)) {
			g.ToolReaderFor = append(g.ToolReaderFor, string(s.UID))
		}
		return nil
	})
}

// ClearReader removes the reader of server, whoever it is (any admin may do this).
func (t *ToolReader) ClearReader(ctx context.Context, server string) error {
	t.consent.Lock()
	defer t.consent.Unlock()
	defer t.changed()
	s, err := t.server(ctx, server)
	if err != nil {
		return err
	}
	return t.clear(ctx, s, "")
}

// clear removes the consent for s from every grant except keep's.
func (t *ToolReader) clear(ctx context.Context, s *v1.Server, keep string) error {
	grants, err := t.Conns.Store.List(ctx, "")
	if err != nil {
		return err
	}
	uid := string(s.UID)
	for _, listed := range grants {
		if listed.Connection != s.Spec.Connection || listed.Email == keep || !slices.Contains(listed.ToolReaderFor, uid) {
			continue
		}
		if err := t.editGrant(ctx, listed.Email, listed.Connection, func(g *Grant) error {
			if g != nil {
				g.ToolReaderFor = slices.DeleteFunc(g.ToolReaderFor, func(u string) bool { return u == uid })
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Reader returns the consenting reader if their grant is connected and they are not offboarded.
func (t *ToolReader) Reader(ctx context.Context, server string) (string, bool, error) {
	s, err := t.server(ctx, server)
	if err != nil {
		return "", false, err
	}
	return t.readerOf(ctx, s)
}

func (t *ToolReader) readerOf(ctx context.Context, s *v1.Server) (string, bool, error) {
	grants, err := t.Conns.Store.List(ctx, "")
	if err != nil {
		return "", false, err
	}
	off, err := t.offboarded(ctx)
	if err != nil {
		return "", false, err
	}
	for _, g := range grants {
		if g.Connection == s.Spec.Connection && slices.Contains(g.ToolReaderFor, string(s.UID)) &&
			g.State == grantConnectedState && !off[strings.ToLower(g.Email)] {
			return g.Email, true, nil
		}
	}
	return "", false, nil
}

func (t *ToolReader) offboarded(ctx context.Context) (map[string]bool, error) {
	var cm corev1.ConfigMap
	err := t.Conns.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: OffboardedConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return map[string]bool{}, nil
	} else if err != nil {
		return nil, err
	}
	emails, _ := controller.OffboardedEmails(&cm)
	out := map[string]bool{}
	for _, e := range emails {
		out[e] = true
	}
	return out, nil
}

// token returns who's access token, taken from the grant only while that same grant (read under its lock)
// still carries the consent and was issued for the server's URL. A reconnected grant carries none.
func (t *ToolReader) token(ctx context.Context, who string, s *v1.Server) (string, *Grant, error) {
	defer t.Conns.lockGrant(who, s.Spec.Connection)()
	g, err := t.Conns.Store.Get(ctx, who, s.Spec.Connection)
	if err != nil {
		return "", nil, err
	}
	if g == nil || !slices.Contains(g.ToolReaderFor, string(s.UID)) {
		return "", nil, ErrNoReader
	}
	tok, g2, err := t.Conns.renewLocked(ctx, g, false)
	if err != nil { // a token may still come with the error (save failed, or a transient failure before expiry)
		slog.Warn("tool reader: grant renewal", "connection", g.Connection, "user", who, "err", redactTokens(err.Error(), g))
	}
	if tok == "" || g2 == nil || g2.ServerURL != s.Spec.URL {
		return "", nil, ErrNoReader
	}
	return tok, g2, nil
}

// Tools returns the server's tools. Consent and binding are checked on every call, cached or not; the list
// itself is read with the reader's grant at most every TTL.
func (t *ToolReader) Tools(ctx context.Context, server string) (ToolsResult, error) {
	// ponytail: one read lock per call, held across the vendor request (at most 15 s); a consent change waits.
	t.consent.RLock()
	defer t.consent.RUnlock()
	s, err := t.server(ctx, server)
	if err != nil {
		return ToolsResult{}, err
	}
	if err := t.bound(ctx, s); err != nil {
		return ToolsResult{}, err
	}
	who, ok, err := t.readerOf(ctx, s)
	if err != nil {
		return ToolsResult{}, err
	}
	if !ok {
		return ToolsResult{}, ErrNoReader
	}
	key := string(s.UID) + "\n" + s.Spec.URL + "\n" + who
	t.mu.Lock()
	gen := t.gen
	if r, ok := t.cache[key]; ok && time.Since(r.ReadAt) < t.TTL {
		t.mu.Unlock()
		return r, nil
	}
	t.mu.Unlock()
	tok, g, err := t.token(ctx, who, s)
	if err != nil {
		return ToolsResult{}, err
	}
	tools, err := t.List(ctx, nil, s.Spec.URL, s.Spec.Transport, tok)
	if err != nil {
		return ToolsResult{}, fmt.Errorf("list tools of %s: %s", s.Spec.URL, redactTokens(err.Error(), g))
	}
	// Consent may have ended while reading (disconnect, offboarding): then the result is neither cached nor returned.
	t.mu.Lock()
	unchanged := t.gen == gen
	t.mu.Unlock()
	if still, ok, err := t.readerOf(ctx, s); err != nil || !ok || still != who || !unchanged {
		return ToolsResult{}, ErrNoReader
	}
	r := ToolsResult{Tools: tools, Reader: who, ReadAt: time.Now()}
	t.mu.Lock()
	if t.gen == gen {
		if t.cache == nil {
			t.cache = map[string]ToolsResult{}
		}
		t.cache[key] = r
	}
	t.mu.Unlock()
	return r, nil
}

// Handler serves tool metadata to the controller on its own port; it never returns a token.
func (t *ToolReader) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tools/{server}", func(w http.ResponseWriter, r *http.Request) {
		res, err := t.Tools(r.Context(), r.PathValue("server"))
		switch {
		case errors.Is(err, ErrNoReader):
			writeJSON(w, http.StatusNotFound, map[string]string{"reason": "no reader"})
		case errors.Is(err, errNoServer):
			writeJSON(w, http.StatusNotFound, map[string]string{"reason": "no such server"})
		case errors.Is(err, errNotBound):
			writeJSON(w, http.StatusConflict, map[string]string{"reason": "server is not bound to its connection"})
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"reason": err.Error()})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		}
	})
	return mux
}

// writeConsentErr answers a failed SetReader/ClearReader: the admin's own mistakes as 404/409, the rest as 500.
func writeConsentErr(w http.ResponseWriter, err error) {
	var ce consentErr
	switch {
	case errors.Is(err, errNoServer):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &ce), errors.Is(err, errNotBound), errors.Is(err, ErrNoReader):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		serverErr(w, err)
	}
}
