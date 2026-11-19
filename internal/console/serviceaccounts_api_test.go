package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// saFixture is newKeysFixture with a last-use lookup (keyID → time) and the clock.
func saFixture(t *testing.T, lastUsed map[string]time.Time) (*keysFixture, *syncClock) {
	t.Helper()
	keys, clk := newKeySvc(t)
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	cat := &Catalog{Client: keys.Client, Namespace: keys.Namespace, Keys: keys,
		LLMURL: "http://llm.localtest.me:8080", MCPURL: "http://mcp.localtest.me:8080/mcp",
		LastUsed: func(_ context.Context, ids []string, _ time.Time) (map[string]time.Time, error) {
			out := map[string]time.Time{}
			for _, id := range ids {
				if t, ok := lastUsed[id]; ok {
					out[id] = t
				}
			}
			return out, nil
		}}
	f := &keysFixture{h: NewServer(auth, testSPA, LoginOptions{}, cat), keys: keys, k8s: keys.Client, auth: auth}
	f.cookie = func(email, role string) *http.Cookie {
		s := Session{ID: randomString(), Email: email, Role: role, Exp: time.Now().Add(time.Hour).Unix(), SignedIn: t0.Unix()}
		auth.Sessions.Add(s.ID, s.Email, s.Exp)
		return &http.Cookie{Name: SessionCookie, Value: auth.codec.Encode(s)}
	}
	return f, clk
}

type accountsAnswer struct {
	Settings serviceSettingsView `json:"settings"`
	Accounts []accountView       `json:"accounts"`
}

// Section 3.1: every endpoint is for admins only; the answers and refusals follow keyErr.
func TestServiceAccountsAPI(t *testing.T) {
	f, _ := saFixture(t, nil)
	admin, viewer, approver := f.cookie("admin@example.com", RoleAdmin), f.cookie("user01@acme.com", RoleViewer), f.cookie("user02@acme.com", RoleApprover)
	newAcct := map[string]any{"name": "ci-deploy", "group": "eng-ci", "description": "GitHub Actions, repo web"}
	newKey := map[string]any{"name": "gha-2026-12", "scope": "llm", "days": 90}
	for _, ck := range []*http.Cookie{viewer, approver} {
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"GET", "/api/admin/service-accounts", nil},
			{"POST", "/api/admin/service-accounts", newAcct},
			{"DELETE", "/api/admin/service-accounts/ci-deploy", nil},
			{"POST", "/api/admin/service-accounts/ci-deploy/keys", newKey},
		} {
			if r := f.call(t, c.method, c.path, ck, c.body); r.Code != http.StatusForbidden {
				t.Errorf("non-admin %s %s: %d", c.method, c.path, r.Code)
			}
		}
	}
	if r := f.call(t, "GET", "/api/admin/service-accounts", nil, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", r.Code)
	}
	r := f.call(t, "POST", "/api/admin/service-accounts", admin, newAcct)
	if r.Code != http.StatusCreated || !strings.Contains(r.Body.String(), `"identity":"ci-deploy@svc.ambit.internal"`) {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	r = f.call(t, "POST", "/api/admin/service-accounts/ci-deploy/keys", admin, newKey)
	if r.Code != http.StatusCreated || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key: %d %v %s", r.Code, r.Header(), r.Body)
	}
	var created createdKey
	_ = json.Unmarshal(r.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "ambit_") || created.View["issuedBy"] != "admin@example.com" || created.URLs["llm"] == "" {
		t.Fatalf("key answer %+v", created)
	}
	// list: the account with its key, never the key itself
	r = f.call(t, "GET", "/api/admin/service-accounts", admin, nil)
	var got accountsAnswer
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if r.Code != 200 || len(got.Accounts) != 1 || got.Accounts[0].Group != "eng-ci" || got.Accounts[0].Name != "ci-deploy" ||
		len(got.Accounts[0].Keys) != 1 || got.Accounts[0].Keys[0].IssuedBy != "admin@example.com" || strings.Contains(r.Body.String(), created.Key) ||
		got.Settings != (serviceSettingsView{Enabled: true, MaxDays: 90, ExposureDays: 7, LastUsedAvailable: true}) {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	// Settings → Keys lists the service account key with the identity as owner
	if r := f.call(t, "GET", "/api/admin/keys", admin, nil); !strings.Contains(r.Body.String(), `"owner":"ci-deploy@svc.ambit.internal"`) {
		t.Fatalf("all keys: %s", r.Body)
	}
	// refusals
	for _, c := range []struct {
		path string
		body any
		want int
	}{
		{"/api/admin/service-accounts", map[string]any{"name": "ci-deploy", "group": "eng", "description": ""}, http.StatusConflict},
		{"/api/admin/service-accounts", map[string]any{"name": "Bad", "group": "eng", "description": ""}, http.StatusBadRequest},
		{"/api/admin/service-accounts", map[string]any{"name": "x", "group": "eng", "extra": 1}, http.StatusBadRequest},
		{"/api/admin/service-accounts/nope/keys", newKey, http.StatusNotFound},
		{"/api/admin/service-accounts/ci-deploy/keys", map[string]any{"name": "k", "scope": "llm", "days": 365}, http.StatusBadRequest},
	} {
		if r := f.call(t, "POST", c.path, admin, c.body); r.Code != c.want {
			t.Errorf("POST %s %v: %d %s, want %d", c.path, c.body, r.Code, r.Body, c.want)
		}
	}
	if r := f.call(t, "POST", "/api/admin/service-accounts/ci-deploy/keys", admin, newKey); r.Code != http.StatusCreated {
		t.Fatal(r.Code)
	}
	if r := f.call(t, "POST", "/api/admin/service-accounts/ci-deploy/keys", admin, newKey); r.Code != http.StatusTooManyRequests {
		t.Errorf("third key: %d", r.Code)
	}
	// switched off: create 409, revoke still 204, delete still 204
	f.keys.Services.Enabled = false
	if r := f.call(t, "POST", "/api/admin/service-accounts", admin, map[string]any{"name": "other", "group": "eng", "description": ""}); r.Code != http.StatusConflict {
		t.Errorf("off: create %d", r.Code)
	}
	if r := f.call(t, "POST", "/api/admin/service-accounts/ci-deploy/keys", admin, newKey); r.Code != http.StatusConflict {
		t.Errorf("off: key %d", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/admin/keys/"+created.View["id"].(string), admin, nil); r.Code != http.StatusNoContent {
		t.Errorf("off: revoke %d", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/admin/service-accounts/ci-deploy", admin, nil); r.Code != http.StatusNoContent {
		t.Errorf("off: delete %d", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/admin/service-accounts/ci-deploy", admin, nil); r.Code != http.StatusNoContent {
		t.Errorf("second delete: %d", r.Code)
	}
	if r := f.call(t, "DELETE", "/api/admin/service-accounts/never", admin, nil); r.Code != http.StatusNotFound {
		t.Errorf("unknown delete: %d", r.Code)
	}
	f.keys.Services.Enabled = true
	if r := f.call(t, "POST", "/api/admin/service-accounts", admin, newAcct); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "never reused") {
		t.Errorf("name reuse: %d %s", r.Code, r.Body)
	}
	// not the key writer: 503 with Retry-After
	f.keys.EndTerm()
	if r := f.call(t, "POST", "/api/admin/service-accounts", admin, map[string]any{"name": "late", "group": "eng", "description": ""}); r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "15" {
		t.Errorf("no Lease: %d %v", r.Code, r.Header())
	}
}

// Section 6 (SA-6 A) and 7.3: the account shows a warning for a key not used for 30 days or more and for a key whose
// issuer was offboarded; nothing is revoked.
func TestServiceAccountWarnings(t *testing.T) {
	last := map[string]time.Time{}
	f, clk := saFixture(t, last)
	admin := f.cookie("admin@example.com", RoleAdmin)
	ctx := context.Background()
	if _, err := f.keys.CreateAccount(ctx, adminA, "nightly-eval", "ml", ""); err != nil {
		t.Fatal(err)
	}
	_, used, _ := f.keys.CreateAccountKey(ctx, adminA, "nightly-eval", "eval-1", ScopeLLM, 90)
	_, fresh, _ := f.keys.CreateAccountKey(ctx, adminB, "nightly-eval", "eval-2", ScopeLLM, 90)
	clk.Set(t0.AddDate(0, 0, 40))
	last[used.ID] = t0.AddDate(0, 0, 6)   // 34 days ago
	last[fresh.ID] = t0.AddDate(0, 0, 39) // yesterday
	if _, err := f.keys.ExposeIssuedBy(ctx, "admin-b@example.com", "scim"); err != nil {
		t.Fatal(err)
	}
	var got accountsAnswer
	_ = json.Unmarshal(f.call(t, "GET", "/api/admin/service-accounts", admin, nil).Body.Bytes(), &got)
	w := got.Accounts[0].Warnings
	if len(w) != 2 || w[0] != `Key "eval-1" not used for 34 days.` ||
		w[1] != `Key "eval-2" was issued by a person who was offboarded. It stops on 2026-11-27 09:00 UTC unless you rotate it.` {
		t.Fatalf("warnings %q", w)
	}
	if k, err := f.keys.getKey(ctx, used.ID); err != nil || !k.Until.Equal(used.Until) {
		t.Fatal("an unused key was changed")
	}
}
