package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/oauth"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubReview stands for the API server's TokenReview: it knows which tokens are whose.
type stubReview struct {
	client.Writer
	tokens map[string]authnv1.UserInfo
	aud    map[string][]string
	fail   bool
}

func (s *stubReview) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	if s.fail {
		return errors.New("api server down")
	}
	tr := obj.(*authnv1.TokenReview)
	u, ok := s.tokens[tr.Spec.Token]
	tr.Status = authnv1.TokenReviewStatus{Authenticated: ok, User: u, Audiences: s.aud[tr.Spec.Token]}
	return nil
}

func oaServer(name string, front bool, conn string) *v1.Server {
	sv := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"}, Spec: v1.ServerSpec{URL: "https://" + name + ".test/mcp", Transport: "streamable-http", Connection: conn}}
	if front {
		sv.Spec.FrontDoor = &v1.ServerFrontDoor{Enabled: true, AcceptUnmaskedServerMessages: true}
	}
	return sv
}

func oaRule(name string, spec v1.AccessRuleSpec) *v1.AccessRule {
	return &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"}, Spec: spec}
}

func newOAuthAPI(t *testing.T, objs ...client.Object) (*OAuthAPI, *KeyService, *stubReview) {
	t.Helper()
	keys, _ := newAgentSvc(t)
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = v1.AddToScheme(sc)
	cat := fake.NewClientBuilder().WithScheme(sc).WithObjects(objs...).Build()
	rv := &stubReview{tokens: map[string]authnv1.UserInfo{
		"auth-token":  {Username: OAuthSubject("ambit-system")},
		"other-token": {Username: "system:serviceaccount:ambit-system:somebody-else"},
	}, aud: map[string][]string{"auth-token": {"ambit-console"}, "other-token": {"ambit-console"}}}
	rv.tokens["wrong-aud"] = authnv1.UserInfo{Username: OAuthSubject("ambit-system")}
	rv.aud["wrong-aud"] = []string{"https://kubernetes.default.svc"}
	return &OAuthAPI{Keys: keys, Client: cat, Review: rv, Namespace: "ambit-system", Audience: "ambit-console", Subject: OAuthSubject("ambit-system"), Now: keys.Now}, keys, rv
}

func call(a *OAuthAPI, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestOAuthAPIOnlyAmbitAuthMayCall(t *testing.T) {
	a, _, rv := newOAuthAPI(t)
	for name, tc := range map[string]struct {
		token string
		want  int
	}{"no token": {"", 401}, "unknown token": {"x", 401}, "another service account": {"other-token", 401}, "a token for another audience": {"wrong-aud", 401}, "ambit-auth": {"auth-token", 200}} {
		if w := call(a, "/internal/oauth/v1/servers", tc.token, `{"email":"a@b.co"}`); w.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, w.Code, tc.want)
		}
	}
	rv.fail = true
	if w := call(a, "/internal/oauth/v1/servers", "auth-token", `{"email":"a@b.co"}`); w.Code != 503 {
		t.Errorf("TokenReview down must refuse: %d", w.Code)
	}
}

func serversOf(t *testing.T, a *OAuthAPI, email string, groups ...string) []string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"email": email, "groups": groups})
	w := call(a, "/internal/oauth/v1/servers", "auth-token", string(b))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct{ Servers []ServerChoice }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	names := []string{}
	for _, s := range out.Servers {
		names = append(names, s.Name)
	}
	return names
}

func TestOAuthServersAreRulesIntersectedWithTheFrontDoor(t *testing.T) {
	a, keys, _ := newOAuthAPI(t,
		oaServer("crm", true, ""), oaServer("wiki", true, ""), oaServer("payroll", false, ""), // payroll: not on the front door
		oaServer("jira", true, "jira-login"), // a server with a Connection is not on the front door (yet)
		oaRule("everyone", v1.AccessRuleSpec{AllSignedIn: true, Servers: []v1.ServerAccess{{Name: "crm"}, {Name: "payroll"}, {Name: "jira"}}}),
		oaRule("eng", v1.AccessRuleSpec{Groups: []string{"eng"}, Servers: []v1.ServerAccess{{Name: "wiki", Tools: []string{"search"}}}}),
		oaRule("pilot", v1.AccessRuleSpec{Users: []string{"pilot@acme.com"}, Servers: []v1.ServerAccess{{Name: "wiki"}}}),
	)
	_ = keys
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	eq(serversOf(t, a, "alice@acme.com"), "crm")                // payroll is not served, jira has a Connection, wiki needs a group
	eq(serversOf(t, a, "Alice@Acme.com", "eng"), "crm", "wiki") // one tool of wiki is enough to offer the server (the scope narrows it later)
	eq(serversOf(t, a, "pilot@acme.com"), "crm", "wiki")        // by name
	eq(serversOf(t, a, "svc-bot@svc.ambit.internal", "eng"))    // a service account is offered nothing
	eq(serversOf(t, a, "not an email"))                         // nor is nonsense
}

func TestOAuthServersOffboardedPersonGetsNothing(t *testing.T) {
	a, keys, _ := newOAuthAPI(t, oaServer("crm", true, ""), oaRule("everyone", v1.AccessRuleSpec{AllSignedIn: true, Servers: []v1.ServerAccess{{Name: "crm"}}}))
	if got := serversOf(t, a, "alice@acme.com"); len(got) != 1 {
		t.Fatalf("before: %v", got)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ambit-offboarded", Namespace: "ambit-system"}, Data: map[string]string{"emails": `["alice@acme.com"]`}}
	if err := keys.Client.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if got := serversOf(t, a, "alice@acme.com"); len(got) != 0 {
		t.Fatalf("an offboarded person is offered %v", got)
	}
}

func TestOAuthAgentCreatedAndActive(t *testing.T) {
	a, keys, _ := newOAuthAPI(t)
	grant := "g-" + "abc123" + "-AbCdEfGhIj"
	body := `{"owner":"Alice@Acme.com","platform":"chatgpt","displayName":"ChatGPT","scope":["crm"],"connections":[],"grantID":"` + grant + `"}`
	w := call(a, "/internal/oauth/v1/agents", "auth-token", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct{ Name string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	ag, err := keys.GetAgent(context.Background(), out.Name)
	if err != nil || ag.Owner != "alice@acme.com" || ag.Mode != ModePerson || ag.Platform != "chatgpt" || len(ag.Grants) != 1 || ag.Grants[0] != grant || len(ag.Scope) != 1 {
		t.Fatalf("agent %+v %v", ag, err)
	}
	active := func(agent, g string) bool {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"agent": agent, "grant": g})
		w := call(a, "/internal/oauth/v1/active", "auth-token", string(b))
		if w.Code != 200 {
			t.Fatalf("active: %d", w.Code)
		}
		var o struct{ Active bool }
		_ = json.Unmarshal(w.Body.Bytes(), &o)
		return o.Active
	}
	if !active(out.Name, grant) {
		t.Fatal("a new agent and its grant are active")
	}
	if active(out.Name, "g-abc123-SomeOtherGrant") || active("no-such-agent", grant) {
		t.Fatal("another grant, or an agent that does not exist, is not active")
	}
	// deleted: not active, whatever the stop list says
	if err := keys.DeleteAgent(context.Background(), Session{Email: "alice@acme.com", SignedIn: t0.Unix()}, out.Name); err != nil {
		t.Fatal(err)
	}
	if active(out.Name, grant) {
		t.Fatal("a deleted agent must not be active")
	}
}

func TestOAuthAgentRefusals(t *testing.T) {
	a, _, _ := newOAuthAPI(t)
	for name, body := range map[string]string{
		"a service account as owner": `{"owner":"bot@svc.ambit.internal","platform":"x","displayName":"x","grantID":"g-abc123-AbCdEfGhIj"}`,
		"no owner":                   `{"platform":"x","displayName":"x","grantID":"g-abc123-AbCdEfGhIj"}`,
		"a made-up grant name":       `{"owner":"alice@acme.com","platform":"x","displayName":"x","grantID":"anything"}`,
		"a grant with a path":        `{"owner":"alice@acme.com","platform":"x","displayName":"x","grantID":"g-abc123-../../x"}`,
	} {
		if w := call(a, "/internal/oauth/v1/agents", "auth-token", body); w.Code == 200 {
			t.Errorf("%s: accepted", name)
		}
	}
	// the limit per person: 409 with a sentence
	a.Keys.Agents.MaxPerPerson = 1
	ok := `{"owner":"alice@acme.com","platform":"x","displayName":"x","grantID":"g-abc123-AbCdEfGhIj"}`
	if w := call(a, "/internal/oauth/v1/agents", "auth-token", ok); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := call(a, "/internal/oauth/v1/agents", "auth-token", ok); w.Code != http.StatusConflict {
		t.Fatalf("second: %d, want 409", w.Code)
	}
}

func TestOAuthClientsAdminAPI(t *testing.T) {
	deleted := ""
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/admin/clients":
			_, _ = w.Write([]byte(`[{"client_id":"c1","client_name":"ChatGPT","redirect_uris":["https://chatgpt.com/cb","https://chatgpt.com/cb2"],"created_at":"2026-11-01T00:00:00Z","metadata":{"registered_via":"dcr","last_used":"2026-11-20T10:00:00Z"}}]`))
		case r.Method == "DELETE" && r.URL.Path == "/admin/clients/c1":
			deleted = "c1"
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer h.Close()
	keys, _ := newAgentSvc(t)
	c := &Catalog{Keys: keys}
	// off: 404
	w := httptest.NewRecorder()
	c.listOAuthClients(w, httptest.NewRequest("GET", "/api/admin/oauth/clients", nil), Session{Role: RoleAdmin})
	if w.Code != 404 {
		t.Fatalf("OAuth off: %d", w.Code)
	}
	c.Hydra = &oauth.HydraAdmin{Base: h.URL}
	w = httptest.NewRecorder()
	c.listOAuthClients(w, httptest.NewRequest("GET", "/api/admin/oauth/clients", nil), Session{Role: RoleAdmin})
	var out struct{ Clients []oauthClientView }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || len(out.Clients) != 1 || out.Clients[0].Name != "ChatGPT" || len(out.Clients[0].RedirectHosts) != 1 || out.Clients[0].RedirectHosts[0] != "chatgpt.com" || out.Clients[0].Via != "dcr" || out.Clients[0].LastUsed.IsZero() {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("DELETE", "/api/admin/oauth/clients/c1", nil)
	r.SetPathValue("id", "c1")
	w = httptest.NewRecorder()
	c.deleteOAuthClient(w, r, Session{Role: RoleAdmin, Email: "admin@acme.com"})
	if w.Code != 204 || deleted != "c1" {
		t.Fatalf("delete: %d %q", w.Code, deleted)
	}
}

// With OAuth the stop machine waits the whole retention before it lets an agent start again (an access token that was issued
// before the stop lives up to 15 minutes); without OAuth one minute is enough for the proxies to drop deleted keys.
func TestStartWaitIsTheRetentionWithOAuth(t *testing.T) {
	s := &KeyService{Agents: AgentSettings{StopRetention: 25 * time.Minute}}
	if s.startWait() != time.Minute {
		t.Fatalf("without OAuth: %v", s.startWait())
	}
	s.RevokeGrants = func(context.Context, Agent) error { return nil }
	if s.startWait() != 25*time.Minute {
		t.Fatalf("with OAuth: %v", s.startWait())
	}
}
