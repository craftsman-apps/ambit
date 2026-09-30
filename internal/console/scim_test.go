package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const scimToken = "scim-test-token-0123456789"

func scimFixture(t *testing.T) (*http.ServeMux, *SCIM, *fakeSaaS, client.Client) {
	t.Helper()
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	a := testAuthOnly()
	connect(t, conns, a, "bob@acme.test", "bob")
	connect(t, conns, a, "alice@acme.test", "alice")
	sessions := NewSessionStore()
	sessions.Add("s1", "bob@acme.test", time.Now().Add(time.Hour).Unix())
	sessions.Add("s2", "bob@acme.test", time.Now().Add(time.Hour).Unix())
	s := &SCIM{Client: c, Namespace: testNS, Token: scimToken, Conns: conns, Sessions: sessions}
	mux := http.NewServeMux()
	s.register(mux)
	return mux, s, saas, c
}

func scimDo(t *testing.T, mux *http.ServeMux, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/scim+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func provision(t *testing.T, mux *http.ServeMux, email string) string {
	t.Helper()
	r := scimDo(t, mux, "POST", "/scim/v2/Users", scimToken, map[string]any{"schemas": []string{scimUserSchema}, "userName": email,
		"active": true, "emails": []any{map[string]any{"value": email, "primary": true}}})
	if r.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", email, r.Code, r.Body)
	}
	var u struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &u)
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(u.ID) { // it becomes part of a Secret name
		t.Fatalf("id %q is not valid in a Kubernetes name", u.ID)
	}
	return u.ID
}

func blocked(t *testing.T, c client.Client) string {
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: OffboardedConfigMap}, &cm); err != nil {
		return ""
	}
	return cm.Data["emails"]
}

func TestSCIMNeedsTheToken(t *testing.T) {
	mux, _, _, _ := scimFixture(t)
	for _, tok := range []string{"", "wrong"} {
		if r := scimDo(t, mux, "GET", "/scim/v2/Users", tok, nil); r.Code != 401 {
			t.Fatalf("token %q: %d", tok, r.Code)
		}
	}
	if r := scimDo(t, mux, "GET", "/scim/v2/ServiceProviderConfig", scimToken, nil); r.Code != 200 || !strings.Contains(r.Body.String(), `"patch":{"supported":true}`) {
		t.Fatalf("config: %d %s", r.Code, r.Body)
	}
}

func TestSCIMProvisionAndFilter(t *testing.T) {
	mux, _, _, _ := scimFixture(t)
	id := provision(t, mux, "bob@acme.test")
	r := scimDo(t, mux, "GET", `/scim/v2/Users?filter=userName+eq+%22BOB@acme.test%22`, scimToken, nil)
	if !strings.Contains(r.Body.String(), `"totalResults":1`) || !strings.Contains(r.Body.String(), id) {
		t.Fatalf("filter: %s", r.Body)
	}
	if r := scimDo(t, mux, "POST", "/scim/v2/Users", scimToken, map[string]any{"userName": "bob@acme.test"}); r.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", r.Code)
	}
}

// Entra ID sends {"op":"Replace","path":"active","value":"False"}.
func TestDeactivationOffboardsEverything(t *testing.T) {
	mux, s, saas, c := scimFixture(t)
	id := provision(t, mux, "bob@acme.test")
	r := scimDo(t, mux, "PATCH", "/scim/v2/Users/"+id, scimToken, map[string]any{"schemas": []string{scimPatchOp},
		"Operations": []any{map[string]any{"op": "Replace", "path": "active", "value": "False"}}})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"active":false`) {
		t.Fatalf("patch: %d %s", r.Code, r.Body)
	}
	if len(saas.revoked) != 1 || saas.revoked[0] != "rt-bob" {
		t.Fatalf("revoked at provider: %v", saas.revoked)
	}
	if g, _ := s.Conns.Store.Get(t.Context(), "bob@acme.test", "jira"); g != nil {
		t.Fatal("bob's grant kept")
	}
	if g, _ := s.Conns.Store.Get(t.Context(), "alice@acme.test", "jira"); g == nil {
		t.Fatal("alice's grant was removed too")
	}
	if s.Sessions.Valid("s1") || s.Sessions.Valid("s2") {
		t.Fatal("bob's console sessions still valid")
	}
	if b := blocked(t, c); b != `["bob@acme.test"]` {
		t.Fatalf("gateway block list %s", b)
	}
	recs, err := s.Records(t.Context())
	if err != nil || len(recs) != 1 || !recs[0].CompleteSuccess || recs[0].SessionsEnded != 2 || !recs[0].GatewayBlocked ||
		len(recs[0].Grants) != 1 || !recs[0].Grants[0].RevokedAtProvider || recs[0].Trigger != "scim PATCH active=false" {
		t.Fatalf("record %+v %v", recs, err)
	}
	// Okta reactivates with PUT: the gateway block is lifted.
	r = scimDo(t, mux, "PUT", "/scim/v2/Users/"+id, scimToken, map[string]any{"userName": "bob@acme.test", "active": true,
		"emails": []any{map[string]any{"value": "bob@acme.test", "primary": true}}})
	if r.Code != 200 || blocked(t, c) != `[]` {
		t.Fatalf("reactivate: %d, block list %s", r.Code, blocked(t, c))
	}
}

func TestDeleteOffboardsAnActiveUser(t *testing.T) {
	mux, _, saas, c := scimFixture(t)
	id := provision(t, mux, "alice@acme.test")
	if r := scimDo(t, mux, "DELETE", "/scim/v2/Users/"+id, scimToken, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
	if len(saas.revoked) != 1 || saas.revoked[0] != "rt-alice" || blocked(t, c) != `["alice@acme.test"]` {
		t.Fatalf("revoked %v block %s", saas.revoked, blocked(t, c))
	}
	if r := scimDo(t, mux, "GET", "/scim/v2/Users/"+id, scimToken, nil); r.Code != 404 {
		t.Fatalf("after delete: %d", r.Code)
	}
}
