package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNS = "ambit-system"

func catalogFixture(t *testing.T) (http.Handler, client.Client, *Auth) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	m := func(name, id string, groups []string, all bool) *v1.Model {
		return &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec:   v1.ModelSpec{Provider: "Openrouter", Model: id, DisplayName: name + " display", CredentialsRef: v1.SecretRef{Name: "k"}},
			Status: v1.ModelStatus{AllowedGroups: groups, AllowAllSignedIn: all, Conditions: []metav1.Condition{{Type: "Ready", Status: "True", Reason: "Accepted", Message: "served by the gateway"}}}}
	}
	git := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "ml-research-nemotron", Namespace: testNS,
		Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "ambit-config:ambitproject.dev/AccessRule:ambit-system/ml-research-nemotron"}},
		Spec: v1.AccessRuleSpec{Groups: []string{"ml-research"}, Models: []string{"nemotron-3-super"}}}
	console := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "everyone-gemma", Namespace: testNS},
		Spec:   v1.AccessRuleSpec{AllSignedIn: true, Models: []string{"gemma-4-31b"}},
		Status: v1.AccessRuleStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: "True", Reason: "Compiled", Message: "applied"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(m("gemma-4-31b", "google/gemma-4-31b-it:free", nil, true), m("nemotron-3-super", "nvidia/nemotron:free", []string{"ml-research"}, false), git, console).
		WithStatusSubresource(&v1.Model{}, &v1.AccessRule{}).Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS}), c, auth
}

func as(a *Auth, role string) *http.Cookie {
	s := Session{ID: randomString(), Email: role + "@acme.test", Role: role, Exp: time.Now().Add(time.Hour).Unix()}
	a.Sessions.Add(s.ID, s.Email, s.Exp)
	return &http.Cookie{Name: SessionCookie, Value: a.codec.Encode(s)}
}

func do(t *testing.T, h http.Handler, method, path string, ck *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCatalogRequiresSession(t *testing.T) {
	h, _, _ := catalogFixture(t)
	for _, p := range []string{"/api/models", "/api/access-rules", "/api/access-rules/everyone-gemma"} {
		if r := do(t, h, "GET", p, nil, nil); r.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", p, r.Code)
		}
	}
}

func TestListModelsShowsWhoMayUseThem(t *testing.T) {
	h, _, auth := catalogFixture(t)
	r := do(t, h, "GET", "/api/models", as(auth, RoleViewer), nil)
	var got []ModelView
	json.NewDecoder(r.Body).Decode(&got)
	if r.Code != 200 || len(got) != 2 || got[0].Name != "gemma-4-31b" || !got[0].AllSignedIn || got[1].AllowedGroups[0] != "ml-research" || got[0].Ready != "True" {
		t.Fatalf("%d %+v", r.Code, got)
	}
}

func TestListRulesMarksGitManaged(t *testing.T) {
	h, _, auth := catalogFixture(t)
	r := do(t, h, "GET", "/api/access-rules", as(auth, RoleApprover), nil)
	var got []RuleView
	json.NewDecoder(r.Body).Decode(&got)
	if r.Code != 200 || len(got) != 2 {
		t.Fatalf("%d %+v", r.Code, got)
	}
	byName := map[string]RuleView{got[0].Name: got[0], got[1].Name: got[1]}
	if !byName["ml-research-nemotron"].GitManaged || byName["everyone-gemma"].GitManaged {
		t.Fatalf("git flags wrong: %+v", got)
	}
}

func TestRuleDetailShowsEffectivePolicy(t *testing.T) {
	h, _, auth := catalogFixture(t)
	r := do(t, h, "GET", "/api/access-rules/ml-research-nemotron", as(auth, RoleViewer), nil)
	var got RuleDetail
	json.NewDecoder(r.Body).Decode(&got)
	if r.Code != 200 || len(got.Effective) != 1 || got.Effective[0].Model != "nemotron-3-super" ||
		got.Effective[0].CEL != `jwt.groups.exists(g, g in ["ml-research"])` || got.Effective[0].ModelName != "nvidia/nemotron:free" {
		t.Fatalf("%d %+v", r.Code, got)
	}
	if r := do(t, h, "GET", "/api/access-rules/nope", as(auth, RoleViewer), nil); r.Code != 404 {
		t.Fatalf("missing rule: %d", r.Code)
	}
}

func TestOnlyAdminsWrite(t *testing.T) {
	h, _, auth := catalogFixture(t)
	body := RuleInput{Name: "eng-nemotron", Groups: []string{"eng"}, Models: []string{"nemotron-3-super"}}
	for _, role := range []string{RoleViewer, RoleApprover} {
		if r := do(t, h, "POST", "/api/access-rules", as(auth, role), body); r.Code != http.StatusForbidden {
			t.Errorf("%s create: got %d, want 403", role, r.Code)
		}
		if r := do(t, h, "DELETE", "/api/access-rules/everyone-gemma", as(auth, role), nil); r.Code != http.StatusForbidden {
			t.Errorf("%s delete: got %d, want 403", role, r.Code)
		}
	}
}

func TestAdminCreatesUpdatesDeletesRule(t *testing.T) {
	h, c, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "eng-nemotron", Groups: []string{"eng"}, Models: []string{"nemotron-3-super"}})
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	var got v1.AccessRule
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng-nemotron"}, &got); err != nil || got.Spec.Groups[0] != "eng" {
		t.Fatalf("not created: %v %+v", err, got.Spec)
	}
	if got.Annotations["ambitproject.dev/created-by"] != "admin@acme.test" {
		t.Fatalf("creator not recorded: %v", got.Annotations)
	}
	if r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "eng-nemotron", AllSignedIn: true, Models: []string{"gemma-4-31b"}}); r.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", r.Code)
	}
	if r := do(t, h, "PUT", "/api/access-rules/eng-nemotron", admin, RuleInput{Groups: []string{"eng", "finance"}, Models: []string{"nemotron-3-super"}}); r.Code != 200 {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng-nemotron"}, &got)
	if len(got.Spec.Groups) != 2 {
		t.Fatalf("update not applied: %+v", got.Spec)
	}
	if r := do(t, h, "DELETE", "/api/access-rules/eng-nemotron", admin, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
}

func TestInvalidInputIsRejectedWithReason(t *testing.T) {
	h, _, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	cases := map[string]RuleInput{
		"both groups and everyone": {Name: "x", Groups: []string{"eng"}, AllSignedIn: true, Models: []string{"gemma-4-31b"}},
		"neither":                  {Name: "x", Models: []string{"gemma-4-31b"}},
		"no models":                {Name: "x", AllSignedIn: true},
		"unknown model":            {Name: "x", AllSignedIn: true, Models: []string{"gemma-4-31"}},
		"bad name":                 {Name: "Eng Rule", AllSignedIn: true, Models: []string{"gemma-4-31b"}},
		"bad group":                {Name: "x", Groups: []string{"eng; drop"}, Models: []string{"gemma-4-31b"}},
		"quote in group":           {Name: "x", Groups: []string{`a" || true || "`}, Models: []string{"gemma-4-31b"}},
	}
	for name, in := range cases {
		r := do(t, h, "POST", "/api/access-rules", admin, in)
		var e map[string]string
		json.NewDecoder(r.Body).Decode(&e)
		if r.Code != http.StatusBadRequest || e["error"] == "" {
			t.Errorf("%s: got %d %v, want 400 with a reason", name, r.Code, e)
		}
	}
}

func TestGitManagedRulesAreReadOnly(t *testing.T) {
	h, _, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	if r := do(t, h, "PUT", "/api/access-rules/ml-research-nemotron", admin, RuleInput{Groups: []string{"eng"}, Models: []string{"nemotron-3-super"}}); r.Code != http.StatusConflict {
		t.Errorf("update git-managed: %d", r.Code)
	}
	if r := do(t, h, "DELETE", "/api/access-rules/ml-research-nemotron", admin, nil); r.Code != http.StatusConflict {
		t.Errorf("delete git-managed: %d", r.Code)
	}
}

func TestWritesNeedJSON(t *testing.T) {
	h, _, auth := catalogFixture(t)
	req := httptest.NewRequest("POST", "/api/access-rules", bytes.NewBufferString("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // what a cross-site form can send
	req.AddCookie(as(auth, RoleAdmin))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d, want 415", rec.Code)
	}
}

// The console form edits groups and models only (servers come in step 4b); saving must not drop tool access.
func TestUpdateKeepsServerAccess(t *testing.T) {
	h, c, a := catalogFixture(t)
	admin := as(a, RoleAdmin)
	withTools := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng-tools", Namespace: testNS},
		Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"get_current_time"}}}}}
	if err := c.Create(t.Context(), withTools); err != nil {
		t.Fatal(err)
	}
	// A server-only rule may be saved without models.
	if r := do(t, h, "PUT", "/api/access-rules/eng-tools", admin, RuleInput{Groups: []string{"eng", "finance"}}); r.Code != 200 {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	var got v1.AccessRule
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng-tools"}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Servers) != 1 || got.Spec.Servers[0].Tools[0] != "get_current_time" || len(got.Spec.Groups) != 2 {
		t.Fatalf("spec after update: %+v", got.Spec)
	}
	// A rule with neither models nor servers is still refused.
	if r := do(t, h, "PUT", "/api/access-rules/everyone-gemma", admin, RuleInput{AllSignedIn: true}); r.Code != http.StatusBadRequest {
		t.Fatalf("empty rule: %d %s", r.Code, r.Body)
	}
}
