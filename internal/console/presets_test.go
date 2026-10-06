package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/presets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func crmPreset(version int) presets.Preset {
	return presets.Preset{ID: "acme-crm", Version: version, DisplayName: "Acme CRM", Category: "Sales", Verified: true,
		Server:           presets.ServerRef{URL: "https://mcp.{tenant}.crm.test/mcp", Transport: "streamable-http"},
		AuthorizationURL: "https://login.crm.test/authorize", TokenURL: "https://login.crm.test/token", Scopes: []string{"read"},
		Placeholders: []presets.Placeholder{{Name: "tenant", Label: "Tenant", Pattern: "^[a-z]{1,20}$", Example: "acme"}},
		Setup:        "Callback {callbackURL}", Live: presets.Live{Tool: "list"}}
}

type presetEnv struct {
	h   http.Handler
	cl  client.Client
	cat *Catalog
	ck  *http.Cookie
	gh  *fakeGitHub // nil in Direct mode
}

func presetFixture(t *testing.T, gitops bool) *presetEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	conns := &Connections{Client: cl, Namespace: testNS, PublicURL: "https://ambit.acme.test", Codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")},
		Store: &GrantStore{Client: cl, Namespace: "ambit-grants", Key: []byte("fedcba9876543210fedcba9876543210")}}
	unverified := crmPreset(1)
	unverified.ID, unverified.Verified = "unverified-app", false
	cat := &Catalog{Client: cl, Namespace: testNS, Conns: conns, PublicURL: "https://ambit.acme.test", Presets: []presets.Preset{crmPreset(1), unverified}}
	env := &presetEnv{cl: cl, cat: cat}
	if gitops {
		gh, fake := newFakeGitHub(t)
		cat.GitOps = &GitOps{Repo: gh, Repository: gh.Repository, Branch: "main", Dir: "ambit"}
		env.gh = fake
	}
	env.h = NewServer(auth, testSPA, LoginOptions{}, cat)
	env.ck = as(auth, RoleAdmin)
	return env
}

func addInput(op string) PresetAddInput {
	return PresetAddInput{Operation: op, Preset: "acme-crm", Version: 1, Name: "crm", DisplayName: "CRM",
		ClientID: "cid", ClientSecret: "sec", Values: map[string]string{"tenant": "acme"}}
}

func TestPresetListIsReadableWithAPlainGET(t *testing.T) {
	e := presetFixture(t, false)
	rec := do(t, e.h, "GET", "/api/presets", e.ck, nil) // no Content-Type: browsers send none on GET
	var got []PresetView
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || len(got) != 1 || got[0].ID != "acme-crm" || got[0].Setup != "Callback https://ambit.acme.test/connections/callback" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, e.h, "GET", "/api/apps/unfinished", e.ck, nil); rec.Code != 200 {
		t.Fatalf("unfinished: %d", rec.Code)
	}
}

func TestAddFromPresetCreatesConnectionAndServer(t *testing.T) {
	e := presetFixture(t, false)
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("op1abcde")); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var conn v1.Connection
	var srv v1.Server
	_ = e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "crm"}, &conn)
	_ = e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "crm"}, &srv)
	if conn.Spec.ServerURL != "https://mcp.acme.crm.test/mcp" || conn.Spec.Resource != conn.Spec.ServerURL || srv.Spec.Connection != "crm" ||
		srv.Spec.URL != conn.Spec.ServerURL || conn.Annotations["ambitproject.dev/preset"] != "acme-crm@1" || conn.Annotations["ambitproject.dev/operation"] != "op1abcde" {
		t.Fatalf("conn %+v srv %+v", conn, srv)
	}
}

func TestAddFromPresetIsIdempotentPerOperation(t *testing.T) {
	e := presetFixture(t, false)
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("op1abcde"))
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("op1abcde")); rec.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("op2abcde")); rec.Code != http.StatusConflict {
		t.Fatalf("another operation for the same name: %d", rec.Code)
	}
	var l v1.ConnectionList
	_ = e.cl.List(t.Context(), &l)
	if len(l.Items) != 1 {
		t.Fatalf("%d connections", len(l.Items))
	}
}

func TestTamperedOrUnverifiedPresetRefused(t *testing.T) {
	e := presetFixture(t, false)
	wrongVersion, unverified, injected, shortOp := addInput("opaaaaaa"), addInput("opbbbbbb"), addInput("opcccccc"), addInput("x1")
	wrongVersion.Version = 9
	unverified.Preset = "unverified-app"
	injected.Values = map[string]string{"tenant": "acme.evil.test/x"}
	for _, in := range []PresetAddInput{wrongVersion, unverified, injected, shortOp} {
		if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, in); rec.Code != http.StatusBadRequest {
			t.Fatalf("%+v: %d", in, rec.Code)
		}
	}
	var secs corev1.SecretList
	_ = e.cl.List(t.Context(), &secs)
	if len(secs.Items) != 0 {
		t.Fatal("a refused addition created a Secret")
	}
}

func TestAdvancedOverridesMakeACustomApp(t *testing.T) {
	e := presetFixture(t, false)
	in := addInput("opdddddd")
	in.Overrides = &PresetOverrides{Scopes: []string{"read", "write"}}
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, in)
	var conn v1.Connection
	_ = e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "crm"}, &conn)
	if conn.Annotations["ambitproject.dev/preset"] != "custom from acme-crm@1" || len(conn.Spec.Scopes) != 2 {
		t.Fatalf("%+v", conn)
	}
}

func TestServerFailureLeavesUnfinishedAdditionThatFinishes(t *testing.T) {
	e := presetFixture(t, false)
	e.cat.Client = failOnCreate{Client: e.cl, kind: "Server"}
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opeeeeee"))
	e.cat.Client = e.cl
	var un []Unfinished
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/apps/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 1 || un[0].Operation != "opeeeeee" || len(un[0].Missing) != 1 || un[0].Missing[0] != "Server" {
		t.Fatalf("unfinished %+v", un)
	}
	if rec := do(t, e.h, "POST", "/api/apps/unfinished/opeeeeee/finish", e.ck, map[string]string{}); rec.Code != http.StatusOK {
		t.Fatalf("finish %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/apps/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 0 {
		t.Fatalf("still unfinished %+v", un)
	}
}

func TestGitOpsTimeoutIsRecoveredByBranch(t *testing.T) {
	e := presetFixture(t, true)
	e.gh.failNextPullCreate = true
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opffffff")); rec.Code != http.StatusBadGateway {
		t.Fatalf("first attempt: %d", rec.Code)
	}
	var un []Unfinished
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/apps/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 1 {
		t.Fatalf("unfinished %+v", un)
	}
	if rec := do(t, e.h, "POST", "/api/apps/unfinished/opffffff/finish", e.ck, map[string]string{}); rec.Code != http.StatusAccepted {
		t.Fatalf("finish %d %s", rec.Code, rec.Body)
	}
	if e.gh.commits != 1 || e.gh.pulls != 1 {
		t.Fatalf("commits %d pulls %d", e.gh.commits, e.gh.pulls)
	}
	if rec := do(t, e.h, "DELETE", "/api/apps/unfinished/opffffff", e.ck, nil); rec.Code != http.StatusConflict {
		t.Fatalf("remove while the pull request is open: %d", rec.Code)
	}
}

func TestRemoveUnfinishedDeletesWithPreconditions(t *testing.T) {
	e := presetFixture(t, false)
	e.cat.Client = failOnCreate{Client: e.cl, kind: "Server"}
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opgggggg"))
	e.cat.Client = e.cl
	if rec := do(t, e.h, "DELETE", "/api/apps/unfinished/opgggggg", e.ck, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var l v1.ConnectionList
	var secs corev1.SecretList
	_ = e.cl.List(t.Context(), &l)
	_ = e.cl.List(t.Context(), &secs)
	if len(l.Items) != 0 || len(secs.Items) != 0 {
		t.Fatalf("left behind: %d connections, %d secrets", len(l.Items), len(secs.Items))
	}
}

func TestNewerPresetIsShownNotApplied(t *testing.T) {
	e := presetFixture(t, false)
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("ophhhhhh"))
	v2 := crmPreset(2)
	v2.Scopes = []string{"read", "offline"}
	e.cat.Presets[0] = v2
	var apps struct{ Apps []AppView }
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/apps", e.ck, nil).Body.Bytes(), &apps)
	a := apps.Apps[0]
	if a.Preset != "acme-crm@1" || a.NewerPreset == nil || a.NewerPreset.Version != 2 || len(a.NewerPreset.Changes) != 1 {
		t.Fatalf("%+v", a)
	}
}

func TestRetryWithOtherValuesIsRefused(t *testing.T) {
	e := presetFixture(t, false)
	e.cat.Client = failOnCreate{Client: e.cl, kind: "Server"}
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opiiiiii"))
	e.cat.Client = e.cl
	changed := addInput("opiiiiii")
	changed.Values = map[string]string{"tenant": "other"}
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, changed); rec.Code != http.StatusConflict {
		t.Fatalf("retry with other values: %d", rec.Code)
	}
	renamed := addInput("opiiiiii")
	renamed.Name = "crm2" // same operation, another app name: must not start a second addition
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, renamed); rec.Code != http.StatusConflict {
		t.Fatalf("same operation under another name: %d", rec.Code)
	}
}

func TestClosedPullRequestIsReplaced(t *testing.T) {
	e := presetFixture(t, true)
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opjjjjjj"))
	e.gh.closeAll() // the reviewer closed it without merging (closeAll below)
	if rec := do(t, e.h, "POST", "/api/apps/unfinished/opjjjjjj/finish", e.ck, map[string]string{}); rec.Code != http.StatusAccepted {
		t.Fatalf("finish after close: %d %s", rec.Code, rec.Body)
	}
	if e.gh.pulls != 2 {
		t.Fatalf("pulls %d: the closed pull request was reported instead of replaced", e.gh.pulls)
	}
}

func TestBindOlderApp(t *testing.T) {
	e := presetFixture(t, false)
	_ = e.cl.Create(t.Context(), &v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "jira", Namespace: testNS}, Spec: v1.ConnectionSpec{DisplayName: "Jira",
		AuthorizationURL: "https://a.test/a", TokenURL: "https://a.test/t", ClientID: "c", ClientSecretRef: v1.SecretRef{Name: "ambit-conn-jira"}}})
	_ = e.cl.Create(t.Context(), &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "jira", Namespace: testNS}, Spec: v1.ServerSpec{URL: "https://mcp.jira.test/mcp", Connection: "jira"}})
	var apps struct{ Apps []AppView }
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/apps", e.ck, nil).Body.Bytes(), &apps)
	if apps.Apps[0].Bound || apps.Apps[0].BindTo != "https://mcp.jira.test/mcp" {
		t.Fatalf("%+v", apps.Apps[0])
	}
	if rec := do(t, e.h, "PUT", "/api/apps/jira/bind", e.ck, map[string]string{}); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var conn v1.Connection
	_ = e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	if conn.Spec.ServerURL != "https://mcp.jira.test/mcp" || conn.Spec.Resource != "" {
		t.Fatalf("bind must set serverURL only (resource would break existing grants): %+v", conn.Spec)
	}
}

// failOnCreate fails Create for one kind, like a webhook or API server error.
type failOnCreate struct {
	client.Client
	kind string
}

func (f failOnCreate) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*v1.Server); ok && f.kind == "Server" {
		return errors.New("injected create failure")
	}
	return f.Client.Create(ctx, obj, opts...)
}

// closeAll closes every pull request without merging, as a reviewer would.
func (f *fakeGitHub) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for b, st := range f.prsByBranch {
		st.Closed = true
		f.prsByBranch[b] = st
	}
}

// The kind test preset (Task 19) reaches Keycloak over http at *.localtest.me; only a test preset may.
func TestTestingPresetMayUseTestHostsOnly(t *testing.T) {
	e := presetFixture(t, false)
	test, err := presets.LoadTesting()
	if err != nil || len(test) == 0 {
		t.Fatal(err)
	}
	e.cat.Presets = append(e.cat.Presets, test...)
	in := PresetAddInput{Operation: "opkkkkkk", Preset: test[0].ID, Version: test[0].Version, Name: "whoami", DisplayName: "Who am I", ClientID: "cid", ClientSecret: "sec"}
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, in); rec.Code != http.StatusCreated {
		t.Fatalf("test preset: %d %s", rec.Code, rec.Body)
	}
	evil := in
	evil.Operation, evil.Name = "opllllll", "whoami2"
	evil.Overrides = &PresetOverrides{TokenURL: "http://evil.example.com/token"}
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, evil); rec.Code != http.StatusBadRequest {
		t.Fatalf("http override outside the test hosts: %d", rec.Code)
	}
	notTest := crmPreset(1)
	notTest.ID, notTest.AuthorizationURL = "plain", "http://keycloak.localtest.me:8080/auth"
	e.cat.Presets = append(e.cat.Presets, notTest)
	plain := addInput("opmmmmmm")
	plain.Preset, plain.Name = "plain", "plain"
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, plain); rec.Code != http.StatusBadRequest {
		t.Fatalf("http in a catalog preset: %d", rec.Code)
	}
}

// A name taken by another object must be refused before the Secret exists, or the Secret is orphaned.
func TestNameInUseLeavesNoSecret(t *testing.T) {
	e := presetFixture(t, false)
	_ = e.cl.Create(t.Context(), &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "crm", Namespace: testNS}, Spec: v1.ServerSpec{URL: "https://other.test/mcp"}})
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opnnnnnn")); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var secs corev1.SecretList
	_ = e.cl.List(t.Context(), &secs)
	if len(secs.Items) != 0 {
		t.Fatal("a refused addition left a Secret")
	}
}

// Removing an "unfinished" addition that is complete would delete a working app without revoking grants.
func TestRemoveCompleteAdditionRefused(t *testing.T) {
	e := presetFixture(t, false)
	_ = do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("opoooooo"))
	if rec := do(t, e.h, "DELETE", "/api/apps/unfinished/opoooooo", e.ck, nil); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPresetSecretStaysOutOfObjectsAndPullRequests(t *testing.T) {
	e := presetFixture(t, true)
	in := addInput("opqqqqqq")
	in.ClientSecret = "s3cret-value"
	if rec := do(t, e.h, "POST", "/api/apps/from-preset", e.ck, in); rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var secs corev1.SecretList
	_ = e.cl.List(t.Context(), &secs)
	if len(secs.Items) != 1 || strings.Contains(fmt.Sprint(secs.Items[0].Annotations), "s3cret") {
		t.Fatalf("secret annotations %+v", secs.Items)
	}
	for _, pr := range e.gh.prs {
		if strings.Contains(pr["body"], "s3cret") {
			t.Fatal("the client secret is in the pull request")
		}
	}
}

// A double click sends the same operation twice at once: one app, one Secret, no error for either click.
func TestAddFromPresetDoubleClick(t *testing.T) {
	e := presetFixture(t, false)
	codes := make(chan int, 2)
	for range 2 {
		go func() { codes <- do(t, e.h, "POST", "/api/apps/from-preset", e.ck, addInput("oprrrrrr")).Code }()
	}
	for range 2 {
		if c := <-codes; c != http.StatusCreated && c != http.StatusOK {
			t.Fatalf("click: %d", c)
		}
	}
	var l v1.ConnectionList
	var secs corev1.SecretList
	_ = e.cl.List(t.Context(), &l)
	_ = e.cl.List(t.Context(), &secs)
	if len(l.Items) != 1 || len(secs.Items) != 1 {
		t.Fatalf("%d connections, %d secrets", len(l.Items), len(secs.Items))
	}
}
