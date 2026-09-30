package console

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Journey U1 from the console: an admin adds an app (a Connection) without kubectl. The client secret
// goes into a Secret in the grants namespace, never into the Connection or Git.
func TestAdminAddsAndRemovesAnApp(t *testing.T) {
	conns, c := connFixture(t, newFakeSaaS(t))
	a := testAuthOnly()
	h := NewServer(a, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, Conns: conns, PublicURL: "https://console.acme.test"})
	admin := user(a, "priya@acme.test", RoleAdmin)
	good := AppInput{Name: "github", DisplayName: "GitHub", AuthorizationURL: "https://github.com/login/oauth/authorize",
		TokenURL: "https://github.com/login/oauth/access_token", ClientID: "Iv1.abc", ClientSecret: "s3cret-value", Scopes: []string{"repo", "read:org"}}
	if r := do(t, h, "POST", "/api/apps", user(a, "alice@acme.test", RoleViewer), good); r.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d", r.Code)
	}
	r := do(t, h, "POST", "/api/apps", admin, good)
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "s3cret-value") {
		t.Fatal("the client secret came back in the response")
	}
	var conn v1.Connection
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "github"}, &conn); err != nil || conn.Spec.ClientSecretRef.Name != "ambit-conn-github" ||
		conn.Spec.ClientID != "Iv1.abc" || len(conn.Spec.Scopes) != 2 || conn.Annotations[createdByKey] != "priya@acme.test" {
		t.Fatalf("connection %+v %v", conn, err)
	}
	var sec corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ambit-grants", Name: "ambit-conn-github"}, &sec); err != nil ||
		(string(sec.Data["clientSecret"]) != "s3cret-value" && sec.StringData["clientSecret"] != "s3cret-value") {
		t.Fatalf("secret %+v %v", sec, err)
	}
	list := decodeAs[struct {
		CallbackURL string    `json:"callbackURL"`
		Apps        []AppView `json:"apps"`
	}](t, do(t, h, "GET", "/api/apps", admin, nil).Body.String())
	if list.CallbackURL != "https://console.acme.test/connections/callback" || len(list.Apps) != 2 {
		t.Fatalf("list %+v", list)
	}
	for name, in := range map[string]AppInput{
		"existing name":  func() AppInput { x := good; return x }(),
		"plain http":     func() AppInput { x := good; x.Name = "gh2"; x.TokenURL = "http://github.com/token"; return x }(),
		"no secret":      func() AppInput { x := good; x.Name = "gh3"; x.ClientSecret = ""; return x }(),
		"bad name":       func() AppInput { x := good; x.Name = "Git Hub"; return x }(),
		"query in URL":   func() AppInput { x := good; x.Name = "gh4"; x.AuthorizationURL += "?x=1"; return x }(),
		"space in scope": func() AppInput { x := good; x.Name = "gh5"; x.Scopes = []string{"repo read"}; return x }(),
	} {
		if r := do(t, h, "POST", "/api/apps", admin, in); r.Code != http.StatusBadRequest && r.Code != http.StatusConflict {
			t.Errorf("%s accepted: %d %s", name, r.Code, r.Body)
		}
	}
	if r := do(t, h, "DELETE", "/api/apps/github", admin, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "github"}, &conn); err == nil {
		t.Fatal("connection still there")
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ambit-grants", Name: "ambit-conn-github"}, &sec); err == nil {
		t.Fatal("client secret still there")
	}
}
