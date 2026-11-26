package console

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/craftsman-apps/ambit/internal/oauth"
	"github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeKeyAPI is the key-set part of Hydra's admin API (GET, POST, PUT, DELETE of /admin/keys/{set}).
func fakeKeyAPI(t *testing.T, fail string) (*httptest.Server, map[string][]json.RawMessage) {
	t.Helper()
	var mu sync.Mutex
	sets := map[string][]json.RawMessage{}
	mk := func(kid string) json.RawMessage {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		b, _ := jose.JSONWebKey{Key: k, KeyID: kid, Algorithm: "RS256", Use: "sig"}.MarshalJSON()
		return b
	}
	sets["hydra.jwt.access-token"] = []json.RawMessage{mk("old-access")}
	sets["hydra.openid.id-token"] = []json.RawMessage{mk("old-id")}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail != "" && fail == r.Method+" "+r.URL.Path {
			w.WriteHeader(500)
			return
		}
		set := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
		switch r.Method {
		case "GET":
			ks, ok := sets[set]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": ks})
		case "POST":
			n++
			sets[set] = []json.RawMessage{mk("new-" + string(rune('a'+n)))}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case "PUT":
			var in struct{ Keys []json.RawMessage }
			_ = json.NewDecoder(r.Body).Decode(&in)
			sets[set] = in.Keys
			_, _ = w.Write([]byte(`{}`))
		case "DELETE":
			if _, ok := sets[set]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(sets, set)
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sets
}

func rotateReq(confirm string) *http.Request {
	return httptest.NewRequest("POST", "/api/admin/oauth/signing-key/emergency-rotation", strings.NewReader(`{"confirm":"`+confirm+`"}`))
}

func TestEmergencyRotation(t *testing.T) {
	srv, sets := fakeKeyAPI(t, "")
	keys, _ := newAgentSvc(t)
	c := &Catalog{Keys: keys, Client: keys.Client, Namespace: keys.Namespace, OAuthIssuer: "https://auth.example.com", OAuthResource: "https://mcp.example.com/mcp"}
	admin := Session{Role: RoleAdmin, Email: "admin@acme.com"}
	// OAuth off: 404
	w := httptest.NewRecorder()
	c.rotateSigningKey(w, rotateReq(RotateConfirm), admin)
	if w.Code != 404 {
		t.Fatalf("OAuth off: %d", w.Code)
	}
	c.Hydra = &oauth.HydraAdmin{Base: srv.URL}
	// no confirmation: nothing changes
	w = httptest.NewRecorder()
	c.rotateSigningKey(w, rotateReq("yes"), admin)
	if w.Code != 400 || oauth.KeyID(sets["hydra.jwt.access-token"][0]) != "old-access" {
		t.Fatalf("no confirmation: %d", w.Code)
	}
	var cm corev1.ConfigMap
	if err := c.Client.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: oauth.RotationConfigMap}, &cm); err == nil {
		t.Fatal("the time was written without a confirmation")
	}
	// the status shows key IDs and no key material
	w = httptest.NewRecorder()
	c.getSigningKey(w, httptest.NewRequest("GET", "/api/admin/oauth/signing-key", nil), admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "old-access") || strings.Contains(w.Body.String(), `"d"`) || strings.Contains(w.Body.String(), `"n"`) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	// the rotation
	w = httptest.NewRecorder()
	c.rotateSigningKey(w, rotateReq(RotateConfirm), admin)
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	var rot oauth.Rotation
	_ = json.Unmarshal(w.Body.Bytes(), &rot)
	if len(rot.Sets) != 2 || rot.Sets[0].OldKID != "old-access" || rot.Sets[0].NewKID == "old-access" || rot.Probe.Old == "" || rot.Probe.New == "" {
		t.Fatalf("%+v", rot)
	}
	if strings.Contains(w.Body.String(), `"d":`) {
		t.Fatal("the answer holds a private key")
	}
	if ks := sets["hydra.jwt.access-token"]; len(ks) != 1 || oauth.KeyID(ks[0]) != rot.Sets[0].NewKID {
		t.Fatalf("the signing set: %d keys", len(ks))
	}
	// the time that the hook reads was written
	if err := c.Client.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: oauth.RotationConfigMap}, &cm); err != nil || cm.Data["since"] == "" || cm.Labels[rotationLabel] != "true" {
		t.Fatalf("the rotation time: %v %v %v", err, cm.Data, cm.Labels)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("the answer with the probe tokens may be cached")
	}
	// a second rotation works (the ConfigMap is updated)
	w = httptest.NewRecorder()
	c.rotateSigningKey(w, rotateReq(RotateConfirm), admin)
	if w.Code != 200 {
		t.Fatalf("second rotation: %d %s", w.Code, w.Body.String())
	}
}

// A rotation that fails at Hydra has already written the time (every older grant is refused), keeps the old key, and says to run it again.
func TestEmergencyRotationFailureKeepsTheOldKey(t *testing.T) {
	srv, sets := fakeKeyAPI(t, "PUT /admin/keys/hydra.jwt.access-token")
	keys, _ := newAgentSvc(t)
	c := &Catalog{Keys: keys, Client: keys.Client, Namespace: keys.Namespace, OAuthIssuer: "https://auth.example.com", OAuthResource: "https://mcp.example.com/mcp", Hydra: &oauth.HydraAdmin{Base: srv.URL}}
	w := httptest.NewRecorder()
	c.rotateSigningKey(w, rotateReq(RotateConfirm), Session{Role: RoleAdmin, Email: "admin@acme.com"})
	if w.Code != 502 || !strings.Contains(w.Body.String(), "run it again") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if oauth.KeyID(sets["hydra.jwt.access-token"][0]) != "old-access" {
		t.Fatal("the old key is gone")
	}
	var cm corev1.ConfigMap
	if err := c.Client.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: oauth.RotationConfigMap}, &cm); err != nil || cm.Data["since"] == "" {
		t.Fatalf("the time must be written first: %v", err)
	}
}
