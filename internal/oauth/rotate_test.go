package oauth

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
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeKeys is the key-set part of Hydra's admin API: GET, POST (a new set with one RS256 key), PUT (replace) and DELETE of /admin/keys/{set}.
type fakeKeys struct {
	mu    sync.Mutex
	sets  map[string][]json.RawMessage
	calls []string
	fail  string // "METHOD /path" that answers 500
}

func newKey(t *testing.T, kid string) json.RawMessage {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := jose.JSONWebKey{Key: k, KeyID: kid, Algorithm: "RS256", Use: "sig"}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fakeKeys) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.fail == r.Method+" "+r.URL.Path {
		w.WriteHeader(500)
		return
	}
	set := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	switch r.Method {
	case "GET":
		keys, ok := f.sets[set]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(keySet{Keys: keys})
	case "POST":
		f.sets[set] = []json.RawMessage{newKeyForServer(set)}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(keySet{Keys: f.sets[set]})
	case "PUT":
		var s keySet
		_ = json.NewDecoder(r.Body).Decode(&s)
		f.sets[set] = s.Keys
		_ = json.NewEncoder(w).Encode(s)
	case "DELETE":
		if _, ok := f.sets[set]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.sets, set)
		w.WriteHeader(204)
	}
}

var kidSeq int

func newKeyForServer(set string) json.RawMessage {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	kidSeq++
	b, _ := jose.JSONWebKey{Key: k, KeyID: "new-" + set + "-" + string(rune('a'+kidSeq)), Algorithm: "RS256", Use: "sig"}.MarshalJSON()
	return b
}

// tempLeft is a staging set that a rotation left behind ("" for none).
func tempLeft(f *fakeKeys) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name := range f.sets {
		if strings.HasPrefix(name, tempKeySet) {
			return name
		}
	}
	return ""
}

func TestRotateSigningKeys(t *testing.T) {
	f := &fakeKeys{sets: map[string][]json.RawMessage{
		"hydra.jwt.access-token": {newKey(t, "old-access")}, "hydra.openid.id-token": {newKey(t, "old-id")}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	h := &HydraAdmin{Base: srv.URL}
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	pc := ProbeClaims{Issuer: "https://auth.example.com", Audience: "https://mcp.example.com/mcp"}
	oldAccess := f.sets["hydra.jwt.access-token"][0]
	rot, err := RotateSigningKeys(context.Background(), h, pc, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rot.Sets) != 2 || rot.Sets[0].OldKID != "old-access" || rot.Sets[1].OldKID != "old-id" || rot.Sets[0].NewKID == "" || rot.Sets[0].NewKID == "old-access" {
		t.Fatalf("%+v", rot.Sets)
	}
	// every set holds exactly one key, the new one, and the temporary set is gone
	for _, s := range rot.Sets {
		if ks := f.sets[s.Set]; len(ks) != 1 || KeyID(ks[0]) != s.NewKID {
			t.Errorf("set %s: %d keys, kid %s, want one: %s", s.Set, len(ks), KeyID(ks[0]), s.NewKID)
		}
	}
	if left := tempLeft(f); left != "" {
		t.Errorf("the temporary key set %s stays", left)
	}
	// the signing set is replaced, never grown: no POST to it
	for _, c := range f.calls {
		if c == "POST /admin/keys/hydra.jwt.access-token" {
			t.Error("a key was added to the signing set (two keys for a moment)")
		}
	}
	// probe tokens: the old one verifies with the old key only, the new one with the new key only; both carry no ambit claim
	verify := func(tok string, key json.RawMessage) error {
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON(key); err != nil {
			t.Fatal(err)
		}
		parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			return err
		}
		var c map[string]any
		if err := parsed.Claims(jwk.Public(), &c); err != nil {
			return err
		}
		if c["iss"] != pc.Issuer || c["sub"] != "ambit-rotation-probe" || c["ambit_agent"] != nil || c["ambit_grant"] != nil || c["ambit_scope"] != nil {
			t.Errorf("probe claims %v", c)
		}
		return nil
	}
	newAccess := f.sets["hydra.jwt.access-token"][0]
	if verify(rot.Probe.Old, oldAccess) != nil || verify(rot.Probe.New, newAccess) != nil {
		t.Error("a probe token does not verify with its own key")
	}
	if verify(rot.Probe.Old, newAccess) == nil || verify(rot.Probe.New, oldAccess) == nil {
		t.Error("a probe token verifies with the other key")
	}
}

func TestRotateRefusesASetThatIsNotOneKey(t *testing.T) {
	f := &fakeKeys{sets: map[string][]json.RawMessage{
		"hydra.jwt.access-token": {newKey(t, "a"), newKey(t, "b")}, "hydra.openid.id-token": {newKey(t, "i")}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := RotateSigningKeys(context.Background(), &HydraAdmin{Base: srv.URL}, ProbeClaims{}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "holds 2 keys") {
		t.Fatalf("%v", err)
	}
	if len(f.sets["hydra.jwt.access-token"]) != 2 {
		t.Error("a set that is not one key was changed")
	}
}

func TestRotateErrorsAreNamedAndKeepTheOldKey(t *testing.T) {
	f := &fakeKeys{sets: map[string][]json.RawMessage{
		"hydra.jwt.access-token": {newKey(t, "old-access")}, "hydra.openid.id-token": {newKey(t, "old-id")}}, fail: "PUT /admin/keys/hydra.jwt.access-token"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, err := RotateSigningKeys(context.Background(), &HydraAdmin{Base: srv.URL}, ProbeClaims{}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "replace key set hydra.jwt.access-token") {
		t.Fatalf("%v", err)
	}
	if KeyID(f.sets["hydra.jwt.access-token"][0]) != "old-access" {
		t.Error("the old key is gone although the replacement failed")
	}
	if left := tempLeft(f); left != "" {
		t.Errorf("the temporary key set %s stays after a failed replacement", left)
	}
	if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), `"d"`) {
		t.Error("the error holds key material")
	}
}

func TestGrantTime(t *testing.T) {
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	got, ok := GrantTime(NewGrantID(at))
	if !ok || !got.Equal(at) {
		t.Fatalf("%v %v", got, ok)
	}
	if _, ok := GrantTime("nonsense"); ok {
		t.Error("a name that was not made by NewGrantID has a time")
	}
}
