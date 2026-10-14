package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// keysFixture serves the real console routes over a fake cluster. Sessions are made by cookie(email, role).
type keysFixture struct {
	h      http.Handler
	keys   *KeyService
	k8s    client.Client
	auth   *Auth
	cookie func(email, role string) *http.Cookie
}

func newKeysFixture(t *testing.T) *keysFixture {
	t.Helper()
	keys, _ := newKeySvc(t)
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	cat := &Catalog{Client: keys.Client, Namespace: keys.Namespace, Keys: keys,
		LLMURL: "http://llm.localtest.me:8080", MCPURL: "http://mcp.localtest.me:8080/mcp"}
	f := &keysFixture{h: NewServer(auth, testSPA, LoginOptions{}, cat), keys: keys, k8s: keys.Client, auth: auth}
	f.cookie = func(email, role string) *http.Cookie {
		s := Session{ID: randomString(), Email: email, Role: role, Exp: time.Now().Add(time.Hour).Unix(), SignedIn: t0.Unix()}
		auth.Sessions.Add(s.ID, s.Email, s.Exp)
		return &http.Cookie{Name: SessionCookie, Value: auth.codec.Encode(s)}
	}
	return f
}

func (f *keysFixture) call(t *testing.T, method, path string, ck *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.h, method, path, ck, body)
}

type createdKey struct {
	Key  string            `json:"key"`
	View map[string]any    `json:"view"`
	URLs map[string]string `json:"urls"`
}

func (f *keysFixture) create(t *testing.T, ck *http.Cookie, scope string, days int) createdKey {
	t.Helper()
	r := f.call(t, "POST", "/api/keys", ck, map[string]any{"name": "laptop", "scope": scope, "days": days})
	if r.Code != http.StatusCreated {
		t.Fatal(r.Code, r.Body.String())
	}
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("create response may be cached:", r.Header())
	}
	var c createdKey
	if err := json.Unmarshal(r.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeysAPI(t *testing.T) {
	f := newKeysFixture(t)
	ck := f.cookie("mira@acme.com", RoleViewer)
	created := f.create(t, ck, "both", 90)
	if !strings.HasPrefix(created.Key, "ambit_") || created.View["prefix"] != created.Key[:10] || created.View["scope"] != "both" {
		t.Fatalf("%+v", created)
	}
	// The console builds the per-tool setup text from these addresses, so the key never travels in a second place.
	if created.URLs["llm"] != "http://llm.localtest.me:8080" || created.URLs["mcp"] != "http://mcp.localtest.me:8080/mcp" {
		t.Errorf("urls: %v", created.URLs)
	}
	body := f.call(t, "GET", "/api/keys", ck, nil).Body.String()
	if strings.Contains(body, created.Key) || strings.Contains(body, "sha256:") || !strings.Contains(body, created.View["id"].(string)) {
		t.Fatal(body)
	}
	var list struct {
		Settings map[string]any
		Keys     []map[string]any
	}
	json.Unmarshal([]byte(body), &list)
	if list.Settings["enabled"] != true || len(list.Keys) != 1 || list.Keys[0]["lastUsed"] != nil || list.Keys[0]["owner"] != nil {
		t.Fatalf("%s", body)
	}
	if r := f.call(t, "POST", "/api/keys", ck, map[string]any{"name": "x", "scope": "llm", "days": 60}); r.Code != 400 {
		t.Fatal("days 60 accepted:", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/keys/"+created.View["id"].(string), ck, nil); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if r := f.call(t, "DELETE", "/api/keys/"+created.View["id"].(string), ck, nil); r.Code != 404 {
		t.Fatal("second revoke:", r.Code)
	}
}

func TestKeysAPINeedsSignIn(t *testing.T) {
	f := newKeysFixture(t)
	for _, p := range []string{"/api/keys", "/api/admin/keys", "/api/admin/key-events", "/api/settings/keys"} {
		if r := f.call(t, "GET", p, nil, nil); r.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, r.Code)
		}
	}
}

func TestKeysAPINotLeader(t *testing.T) {
	f := newKeysFixture(t)
	f.keys.EndTerm()
	r := f.call(t, "POST", "/api/keys", f.cookie("mira@acme.com", RoleViewer), map[string]any{"name": "x", "scope": "llm", "days": 30})
	if r.Code != 503 || r.Header().Get("Retry-After") == "" {
		t.Fatal(r.Code, r.Header())
	}
}

func TestKeyErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{{ErrBadInput, 400}, {ErrKeysOff, 409}, {ErrOffboarded, 403}, {ErrTooMany, 429}, {ErrTooFast, 429}, {ErrNotLeader, 503}, {ErrNotFound, 404}, {io.ErrUnexpectedEOF, 500}} {
		w := httptest.NewRecorder()
		keyErr(w, c.err)
		if w.Code != c.code {
			t.Errorf("%v: %d, want %d", c.err, w.Code, c.code)
		}
	}
}

func TestAdminKeysNeedAdmin(t *testing.T) {
	f := newKeysFixture(t)
	v := f.cookie("mira@acme.com", RoleViewer)
	for _, p := range []string{"/api/admin/keys", "/api/admin/key-events"} {
		if r := f.call(t, "GET", p, v, nil); r.Code != http.StatusForbidden {
			t.Errorf("%s: %d", p, r.Code)
		}
	}
	if r := f.call(t, "DELETE", "/api/admin/keys/aaaaaaaaaaaa", v, nil); r.Code != http.StatusForbidden {
		t.Fatal(r.Code)
	}
}

func TestAdminSeesAndRevokesEveryKey(t *testing.T) {
	f := newKeysFixture(t)
	c := f.create(t, f.cookie("mira@acme.com", RoleViewer), "llm", 30)
	id := c.View["id"].(string)
	admin := f.cookie("root@acme.com", RoleAdmin)
	r := f.call(t, "GET", "/api/admin/keys", admin, nil)
	var all struct{ Keys []map[string]any }
	json.Unmarshal(r.Body.Bytes(), &all)
	if r.Code != 200 || len(all.Keys) != 1 || all.Keys[0]["owner"] != "mira@acme.com" || strings.Contains(r.Body.String(), c.Key) {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := f.call(t, "DELETE", "/api/admin/keys/"+id, admin, nil); r.Code != 204 {
		t.Fatal(r.Code)
	}
	r = f.call(t, "GET", "/api/admin/key-events", admin, nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"revoke"`) || !strings.Contains(r.Body.String(), "root@acme.com") {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestPeopleCannotRevokeOthersKeys(t *testing.T) {
	f := newKeysFixture(t)
	c := f.create(t, f.cookie("mira@acme.com", RoleViewer), "llm", 30)
	if r := f.call(t, "DELETE", "/api/keys/"+c.View["id"].(string), f.cookie("bob@acme.com", RoleViewer), nil); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestKeySettingsEndpoint(t *testing.T) {
	f := newKeysFixture(t)
	r := f.call(t, "GET", "/api/settings/keys", f.cookie("mira@acme.com", RoleViewer), nil)
	if r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"enabled":true,"inactivityDays":30,"maxDays":365}` {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestCreateKeyRefusesFormPosts(t *testing.T) {
	f := newKeysFixture(t)
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookie("mira@acme.com", RoleViewer))
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatal(w.Code)
	}
}

func TestCreateKeyRejectsUnknownFieldsAndBigBodies(t *testing.T) {
	f := newKeysFixture(t)
	ck := f.cookie("mira@acme.com", RoleViewer)
	if r := f.call(t, "POST", "/api/keys", ck, map[string]any{"name": "x", "scope": "llm", "days": 30, "admin": true}); r.Code != 400 {
		t.Fatal("unknown field:", r.Code)
	}
	if r := f.call(t, "POST", "/api/keys", ck, map[string]any{"name": strings.Repeat("a", 5000), "scope": "llm", "days": 30}); r.Code != 400 {
		t.Fatal("big body:", r.Code)
	}
}

func TestKeyViewFillsLastUsed(t *testing.T) {
	f := newKeysFixture(t)
	f.keys.Client = f.k8s
	cat := &Catalog{Client: f.k8s, Namespace: f.keys.Namespace, Keys: f.keys,
		LastUsed: func(_ context.Context, ids []string, from time.Time) (map[string]time.Time, error) {
			if !from.Equal(t0) {
				t.Errorf("last use searched from %v, want the oldest key's creation %v", from, t0)
			}
			m := map[string]time.Time{}
			for _, id := range ids {
				m[id] = t0.Add(time.Hour)
			}
			return m, nil
		}}
	h := NewServer(f.auth, testSPA, LoginOptions{}, cat)
	ck := f.cookie("mira@acme.com", RoleViewer)
	f.create(t, ck, "llm", 30)
	body := do(t, h, "GET", "/api/keys", ck, nil).Body.String()
	if !strings.Contains(body, `"lastUsed":"2026-10-11T10:00:00Z"`) {
		t.Fatal(body)
	}
}

func TestExpireHookOnlyWithTestHooks(t *testing.T) {
	t.Setenv("AMBIT_TEST_HOOKS", "")
	f := newKeysFixture(t)
	r := f.call(t, "POST", "/api/test/keys/aaaaaaaaaaaa/expire", f.cookie("root@acme.com", RoleAdmin), map[string]any{"until": t0.Format(time.RFC3339)})
	if r.Code != http.StatusNotFound {
		t.Fatal("test hook registered in a normal console:", r.Code)
	}
}

func TestExpireHook(t *testing.T) {
	t.Setenv("AMBIT_TEST_HOOKS", "true")
	f := newKeysFixture(t)
	c := f.create(t, f.cookie("mira@acme.com", RoleViewer), "llm", 30)
	id := c.View["id"].(string)
	until := t0.Add(72 * time.Hour)
	body := map[string]any{"until": until.Format(time.RFC3339)}
	if r := f.call(t, "POST", "/api/test/keys/"+id+"/expire", f.cookie("mira@acme.com", RoleViewer), body); r.Code != 403 {
		t.Fatal("viewer:", r.Code)
	}
	admin := f.cookie("root@acme.com", RoleAdmin)
	if r := f.call(t, "POST", "/api/test/keys/"+id+"/expire", admin, map[string]any{"until": "soon"}); r.Code != 400 {
		t.Fatal("bad time:", r.Code)
	}
	if r := f.call(t, "POST", "/api/test/keys/"+id+"/expire", admin, body); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	ks, _ := f.keys.List(context.Background(), "mira@acme.com")
	if !ks[0].Until.Equal(until) {
		t.Fatalf("%+v", ks[0])
	}
	if r := f.call(t, "POST", "/api/test/keys/aaaaaaaaaaaa/expire", admin, body); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestResetLimitsHook(t *testing.T) {
	t.Setenv("AMBIT_TEST_HOOKS", "")
	f := newKeysFixture(t)
	admin := f.cookie("root@acme.com", RoleAdmin)
	if r := f.call(t, "POST", "/api/test/keys/reset-limits", admin, map[string]any{}); r.Code != http.StatusNotFound {
		t.Fatal("test hook registered in a normal console:", r.Code)
	}
	t.Setenv("AMBIT_TEST_HOOKS", "true")
	f = newKeysFixture(t)
	admin = f.cookie("root@acme.com", RoleAdmin)
	mira := f.cookie("mira@acme.com", RoleViewer)
	body := map[string]any{"name": "x", "scope": "llm", "days": 30}
	n := 0
	for ; n < 30; n++ {
		r := f.call(t, "POST", "/api/keys", mira, body)
		if r.Code == http.StatusTooManyRequests {
			break
		}
		if r.Code != http.StatusCreated {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if n != 10 {
		t.Fatal("hourly limit hit after", n)
	}
	if r := f.call(t, "POST", "/api/test/keys/reset-limits", mira, map[string]any{}); r.Code != 403 {
		t.Fatal("viewer:", r.Code)
	}
	if r := f.call(t, "POST", "/api/test/keys/reset-limits", admin, map[string]any{}); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if r := f.call(t, "POST", "/api/keys", mira, body); r.Code != http.StatusCreated {
		t.Fatal("after reset:", r.Code, r.Body.String())
	}
}

func TestRevokeOfUnreadableKeyIsNotFound(t *testing.T) {
	f := newKeysFixture(t)
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-aaaaaaaaaaaa", Namespace: f.keys.Namespace,
		Labels: map[string]string{keyLabel: "true"}}, Data: map[string][]byte{"key": []byte("not json")}}
	if err := f.k8s.Create(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if r := f.call(t, "DELETE", "/api/admin/keys/aaaaaaaaaaaa", f.cookie("root@acme.com", RoleAdmin), nil); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	if err := f.keys.Revoke(context.Background(), Session{Email: "root@acme.com"}, "aaaaaaaaaaaa", true); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestKeyListWithoutLastUse(t *testing.T) {
	f := newKeysFixture(t)
	queries := 0
	cat := &Catalog{Client: f.k8s, Namespace: f.keys.Namespace, Keys: f.keys,
		LastUsed: func(context.Context, []string, time.Time) (map[string]time.Time, error) { queries++; return nil, nil }}
	h := NewServer(f.auth, testSPA, LoginOptions{}, cat)
	ck := f.cookie("mira@acme.com", RoleViewer)
	f.create(t, ck, "llm", 30)
	r := do(t, h, "GET", "/api/keys?lastUsed=false", ck, nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"lastUsed":null`) || queries != 0 {
		t.Fatalf("lastUsed=false queried the audit %d times: %d %s", queries, r.Code, r.Body.String())
	}
	if do(t, h, "GET", "/api/keys", ck, nil); queries != 1 {
		t.Fatalf("the Keys page lost its last use (%d queries)", queries)
	}
}

// Sessions created before the upgrade carry no sign-in time: the person is told to sign in again.
func TestCreateWithASessionFromBeforeTheUpgrade(t *testing.T) {
	f := newKeysFixture(t)
	s := Session{ID: randomString(), Email: "mira@acme.com", Role: RoleViewer, Exp: time.Now().Add(time.Hour).Unix()}
	f.auth.Sessions.Add(s.ID, s.Email, s.Exp)
	ck := &http.Cookie{Name: SessionCookie, Value: f.auth.codec.Encode(s)}
	r := f.call(t, "POST", "/api/keys", ck, map[string]any{"name": "laptop", "scope": "llm", "days": 30})
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.Body.String(), "Sign in again to create keys") {
		t.Fatal(r.Code, r.Body.String())
	}
}

// Each key shows why and when it stops: inactivity of its owner (until before its end date) or its end date.
func TestKeyStatus(t *testing.T) {
	f := newKeysFixture(t)
	ck := f.cookie("mira@acme.com", RoleViewer)
	f.create(t, ck, "llm", 30)  // until = end date = t0+30d
	f.create(t, ck, "llm", 365) // until = sign-in + 30d, before the end date
	status := func(now time.Time) []string {
		f.keys.Now = func() time.Time { return now }
		var list struct{ Keys []map[string]any }
		json.Unmarshal(f.call(t, "GET", "/api/keys?lastUsed=false", ck, nil).Body.Bytes(), &list)
		var out []string
		for _, k := range list.Keys {
			out = append(out, fmt.Sprint(k["status"], "/", k["stopsBecause"]))
		}
		return out
	}
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{t0, "active/end-date active/inactivity"},
		{t0.AddDate(0, 0, 24), "stops-soon/end-date stops-soon/inactivity"},
		{t0.AddDate(0, 0, 30), "stopped/end-date stopped/inactivity"},
	} {
		if got := strings.Join(slices.Sorted(slices.Values(status(c.now))), " "); got != c.want {
			t.Errorf("at %v: %s, want %s", c.now, got, c.want)
		}
	}
}
