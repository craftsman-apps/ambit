package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const fakeKey = "sk-test-FAKE-0123456789"

type modelEnv struct {
	h    http.Handler
	cl   client.Client
	cat  *Catalog
	auth *Auth
	ck   *http.Cookie
	gh   *fakeGitHub
	logs *bytes.Buffer
}

// modelFixture builds the console with a fake cluster. funcs may intercept calls; logs captures slog output.
func modelFixture(t *testing.T, gitops bool, funcs interceptor.Funcs, objs ...client.Object) *modelEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	logs := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	cat := &Catalog{Client: cl, Namespace: testNS}
	e := &modelEnv{cl: cl, cat: cat, auth: auth, logs: logs}
	if gitops {
		gh, fake := newFakeGitHub(t)
		cat.GitOps = &GitOps{Repo: gh, Repository: gh.Repository, Branch: "main", Dir: "ambit"}
		e.gh = fake
	}
	e.h = NewServer(auth, testSPA, LoginOptions{}, cat)
	e.ck = as(auth, RoleAdmin)
	return e
}

func enteredInput(op, name string) ModelAddInput {
	return ModelAddInput{Operation: op, Name: name, Provider: "OpenAI", Model: "gpt-4.1-mini", Credentials: "entered", APIKey: fakeKey}
}

func (e *modelEnv) secrets(t *testing.T) []corev1.Secret {
	t.Helper()
	var l corev1.SecretList
	if err := e.cl.List(t.Context(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func (e *modelEnv) models(t *testing.T) []v1.Model {
	t.Helper()
	var l v1.ModelList
	if err := e.cl.List(t.Context(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// noKey fails when the key text is in the response or in any log line.
func (e *modelEnv) noKey(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), fakeKey) {
		t.Errorf("the key is in the response: %s", rec.Body)
	}
	if strings.Contains(e.logs.String(), fakeKey) {
		t.Errorf("the key is in a log line: %s", e.logs)
	}
}

func isDryRun(opts []client.CreateOption) bool {
	var o client.CreateOptions
	o.ApplyOptions(opts)
	return len(o.DryRun) > 0
}

func TestAddModelDirectEnteredKeyCreatesSecretThenModel(t *testing.T) {
	var order []string
	e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if !isDryRun(opts) {
			order = append(order, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
		}
		return c.Create(ctx, obj, opts...)
	}})
	in := enteredInput("op1abcde", "gpt-mini")
	in.APIKey = "  " + fakeKey + "\n" // surrounding whitespace is trimmed
	rec := do(t, e.h, "POST", "/api/models", e.ck, in)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"allowedUsers":[]`) { // the web reads .length of it
		t.Fatalf("the new model's view lacks allowedUsers: %s", rec.Body)
	}
	secs, ms := e.secrets(t), e.models(t)
	if len(secs) != 1 || len(ms) != 1 {
		t.Fatalf("%d secrets, %d models", len(secs), len(ms))
	}
	sec, m := secs[0], ms[0]
	if sec.Name != "ambit-llm-gpt-mini" || string(sec.Data["Authorization"]) != fakeKey || sec.Type != corev1.SecretTypeOpaque ||
		sec.Labels["app.kubernetes.io/managed-by"] != "ambit-console" || sec.Labels["ambitproject.dev/model-secret"] != "gpt-mini" ||
		sec.Annotations["ambitproject.dev/operation"] != "op1abcde" {
		t.Fatalf("secret %+v", sec)
	}
	for k, v := range sec.Annotations {
		if strings.Contains(v, fakeKey) {
			t.Errorf("the key is in annotation %s", k)
		}
	}
	for k := range sec.Labels {
		if strings.HasPrefix(k, "ambitproject.dev/key") {
			t.Errorf("label %s would make this a personal API key Secret", k)
		}
	}
	if m.Spec.CredentialsRef == nil || m.Spec.CredentialsRef.Name != "ambit-llm-gpt-mini" || m.Spec.Model != "gpt-4.1-mini" {
		t.Fatalf("model %+v", m.Spec)
	}
	if strings.Join(order, ",") != "Secret/ambit-llm-gpt-mini,Model/gpt-mini" && strings.Join(order, ",") != "/ambit-llm-gpt-mini,/gpt-mini" {
		t.Fatalf("order %v", order)
	}
	if !strings.Contains(e.logs.String(), `msg="model added"`) || !strings.Contains(e.logs.String(), "credentials=entered") || !strings.Contains(e.logs.String(), "by=admin@acme.test") {
		t.Fatalf("log %s", e.logs)
	}
	e.noKey(t, rec)
}

func TestAddModelDryRunRejectionWritesNothing(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if isDryRun(opts) {
			return apierrors.NewInvalid(schema.GroupKind{Group: "ambitproject.dev", Kind: "Model"}, obj.GetName(), nil)
		}
		return c.Create(ctx, obj, opts...)
	}})
	rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "bad"))
	if rec.Code != http.StatusBadRequest || len(e.secrets(t)) != 0 || len(e.models(t)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.noKey(t, rec)
}

// GitOps mode has no right to create Models, so it validates offline, before any Secret is written.
func TestAddModelGitOpsValidatesOfflineBeforeTheSecret(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*v1.Model); ok {
			t.Error("GitOps mode asked the API server to create a Model")
		}
		return c.Create(ctx, obj, opts...)
	}})
	bad := enteredInput("op1abcde", "bad")
	bad.Azure = &v1.AzureSettings{ResourceName: "acme", ResourceType: "OpenAI"} // azure settings only with provider Azure
	rec := do(t, e.h, "POST", "/api/models", e.ck, bad)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "azure") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.secrets(t)) != 0 || e.gh.pulls != 0 {
		t.Fatalf("written: %d secrets, %d pulls", len(e.secrets(t)), e.gh.pulls)
	}
	e.noKey(t, rec)
}

func TestAddModelRefusesExistingModelName(t *testing.T) {
	existing := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-x"}}}
	for _, gitops := range []bool{false, true} {
		e := modelFixture(t, gitops, interceptor.Funcs{}, existing.DeepCopy())
		if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "taken")); rec.Code != http.StatusConflict {
			t.Fatalf("gitops=%v: %d %s", gitops, rec.Code, rec.Body)
		}
		if len(e.secrets(t)) != 0 || (gitops && e.gh.pulls != 0) {
			t.Fatalf("gitops=%v: something was written", gitops)
		}
	}
}

// listingRepo makes the fake Git host list open pull requests (the fake lists a fixed set).
type listingRepo struct {
	PullRequester
	open []PullRequest
}

func (l listingRepo) OpenPullRequests(context.Context) ([]PullRequest, error) { return l.open, nil }

func TestAddModelRefusesNameWithAnOpenPullRequest(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{})
	e.cat.GitOps.Repo = listingRepo{PullRequester: e.cat.GitOps.Repo, open: []PullRequest{
		{Number: 5, URL: "u5", Branch: "ambit/add-model-dup-opzzzzzz"},
		{Number: 6, URL: "u6", Branch: "ambit/add-model-dup-x-opyyyyyy"}, // another name that starts the same
	}}
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "dup")); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "u5") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.secrets(t)) != 0 || e.gh.pulls != 0 {
		t.Fatal("something was written")
	}
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "du")); rec.Code != http.StatusAccepted {
		t.Fatalf("a name that is only a prefix of another: %d %s", rec.Code, rec.Body)
	}
}

func TestAddModelSameOperationTwiceMakesOneSecretAndOneModel(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{})
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "once")); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "once")); rec.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if len(e.secrets(t)) != 1 || len(e.models(t)) != 1 {
		t.Fatalf("%d secrets, %d models", len(e.secrets(t)), len(e.models(t)))
	}
}

func TestAddModelSameOperationTwiceInGitOpsMakesOneSecretAndOnePR(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{})
	for i := 0; i < 2; i++ {
		if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "once")); rec.Code != http.StatusAccepted {
			t.Fatalf("attempt %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if len(e.secrets(t)) != 1 || e.gh.pulls != 1 || e.gh.commits != 1 {
		t.Fatalf("%d secrets, %d pulls, %d commits", len(e.secrets(t)), e.gh.pulls, e.gh.commits)
	}
}

func TestAddModelGitOpsPullRequestHasNoKeyAndSecretIsInTheCluster(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{})
	rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "gitm"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.models(t)) != 0 || len(e.secrets(t)) != 1 || len(e.gh.prs) != 1 {
		t.Fatalf("%d models, %d secrets, %d prs", len(e.models(t)), len(e.secrets(t)), len(e.gh.prs))
	}
	pr := e.gh.prs[0]
	if !strings.Contains(pr["body"], "credentialsRef") || !strings.Contains(pr["body"], "ambit-llm-gitm") || !strings.Contains(pr["body"], "gpt-4.1-mini") {
		t.Fatalf("manifest missing from the pull request: %s", pr["body"])
	}
	if strings.Contains(pr["body"]+pr["title"]+pr["head"], fakeKey) || len(e.gh.lastTree) != 1 {
		t.Fatalf("pull request %+v tree %v", pr, e.gh.lastTree)
	}
	e.noKey(t, rec)
}

func TestAddModelOnlyAdminsMayUseTheModelRoutes(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{})
	for _, role := range []string{RoleApprover, RoleViewer} {
		ck := as(e.auth, role)
		for _, req := range []struct{ method, path string }{{"POST", "/api/models"}, {"GET", "/api/model-secrets"}, {"GET", "/api/models/unfinished"}, {"DELETE", "/api/models/unfinished/ambit-llm-x"}} {
			if rec := do(t, e.h, req.method, req.path, ck, map[string]string{}); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s: %d", role, req.method, req.path, rec.Code)
			}
		}
	}
	if len(e.secrets(t)) != 0 || len(e.models(t)) != 0 {
		t.Fatal("something was written")
	}
	// A cross-site form cannot send JSON: other content types are refused.
	req := httptest.NewRequest("POST", "/api/models", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(e.ck)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", rec.Code)
	}
}

func TestAddModelWithAnExistingSecretCreatesOnlyTheModel(t *testing.T) {
	seed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-shared", Namespace: testNS}, Data: map[string][]byte{"Authorization": []byte("other-team-key")}}
	e := modelFixture(t, false, interceptor.Funcs{}, seed)
	in := ModelAddInput{Operation: "op1abcde", Name: "viaref", Provider: "OpenAI", Model: "gpt-4.1", Credentials: "existing", SecretName: "ambit-llm-shared"}
	rec := do(t, e.h, "POST", "/api/models", e.ck, in)
	if rec.Code != http.StatusCreated || len(e.secrets(t)) != 1 || len(e.models(t)) != 1 || e.models(t)[0].Spec.CredentialsRef.Name != "ambit-llm-shared" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "other-team-key") || !strings.Contains(e.logs.String(), "credentials=existing") {
		t.Fatalf("body %s log %s", rec.Body, e.logs)
	}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-console-keys", Namespace: testNS}}
	_ = e.cl.Create(t.Context(), other)
	for name, secret := range map[string]string{"outside the prefix": "ambit-console-keys", "does not exist": "ambit-llm-missing", "empty": ""} {
		in := ModelAddInput{Operation: "op2abcde", Name: "refused", Provider: "OpenAI", Model: "m", Credentials: "existing", SecretName: secret}
		if rec := do(t, e.h, "POST", "/api/models", e.ck, in); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(e.models(t)) != 1 {
		t.Fatal("a refused addition created a model")
	}
}

func TestAddModelCredentialChoicePerProvider(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{})
	cases := []struct {
		name string
		in   ModelAddInput
		want int
	}{
		{"cloud identity, Bedrock", ModelAddInput{Operation: "opaaaaaa", Name: "bed", Provider: "Bedrock", Model: "us.anthropic.claude", Credentials: "cloud", Bedrock: &v1.BedrockSettings{Region: "eu-west-1"}}, http.StatusCreated},
		{"cloud identity, VertexAI", ModelAddInput{Operation: "opbbbbbb", Name: "vtx", Provider: "VertexAI", Model: "gemini-2.5-pro", Credentials: "cloud", VertexAI: &v1.VertexAISettings{ProjectID: "acme-ml-prod"}}, http.StatusCreated},
		{"cloud identity, OpenAI", ModelAddInput{Operation: "opcccccc", Name: "oa", Provider: "OpenAI", Model: "m", Credentials: "cloud"}, http.StatusBadRequest},
		{"entered key, Bedrock", ModelAddInput{Operation: "opdddddd", Name: "bed2", Provider: "Bedrock", Model: "m", Credentials: "entered", APIKey: fakeKey}, http.StatusBadRequest},
		{"entered key, VertexAI", ModelAddInput{Operation: "opeeeeee", Name: "vtx2", Provider: "VertexAI", Model: "m", Credentials: "entered", APIKey: fakeKey}, http.StatusBadRequest},
		{"empty key", ModelAddInput{Operation: "opffffff", Name: "empty", Provider: "OpenAI", Model: "m", Credentials: "entered", APIKey: " \t "}, http.StatusBadRequest},
		{"no choice", ModelAddInput{Operation: "opgggggg", Name: "none", Provider: "OpenAI", Model: "m"}, http.StatusBadRequest},
		{"key with an existing Secret", ModelAddInput{Operation: "ophhhhhh", Name: "mixed", Provider: "OpenAI", Model: "m", Credentials: "existing", SecretName: "ambit-llm-a", APIKey: fakeKey}, http.StatusBadRequest},
		{"bad name", ModelAddInput{Operation: "opiiiiii", Name: "Bad_Name", Provider: "OpenAI", Model: "m", Credentials: "entered", APIKey: fakeKey}, http.StatusBadRequest},
		{"no operation", ModelAddInput{Name: "noop", Provider: "OpenAI", Model: "m", Credentials: "entered", APIKey: fakeKey}, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := do(t, e.h, "POST", "/api/models", e.ck, c.in)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		e.noKey(t, rec)
	}
	for _, m := range e.models(t) {
		if m.Spec.CredentialsRef != nil {
			t.Errorf("cloud identity model %s has credentialsRef", m.Name)
		}
	}
	if len(e.secrets(t)) != 0 {
		t.Fatal("a Secret was written")
	}
}

func TestAddModelRefusesToOverwriteASecret(t *testing.T) {
	seed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-clash", Namespace: testNS}, Data: map[string][]byte{"Authorization": []byte("original")}}
	e := modelFixture(t, false, interceptor.Funcs{}, seed)
	rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "clash"))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Use an existing Secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := e.secrets(t); len(got) != 1 || string(got[0].Data["Authorization"]) != "original" || len(e.models(t)) != 0 {
		t.Fatalf("secrets %+v models %d", got, len(e.models(t)))
	}
	e.noKey(t, rec)
}

func TestAddModelFailureDeletesTheNewSecretOnly(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*v1.Model); ok && !isDryRun(opts) {
			return errors.New("etcd said no, for key " + fakeKey)
		}
		return c.Create(ctx, obj, opts...)
	}}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-keepme", Namespace: testNS}})
	rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "failing"))
	if rec.Code < 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := e.secrets(t); len(got) != 1 || got[0].Name != "ambit-llm-keepme" || len(e.models(t)) != 0 {
		t.Fatalf("left behind: %+v", got)
	}
	e.noKey(t, rec)
}

func TestAddModelDeleteUsesTheSecretsUID(t *testing.T) {
	var uid *string
	e := modelFixture(t, false, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1.Model); ok && !isDryRun(opts) {
				return errors.New("no")
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var o client.DeleteOptions
			o.ApplyOptions(opts)
			if o.Preconditions != nil && o.Preconditions.UID != nil {
				s := string(*o.Preconditions.UID)
				uid = &s
			}
			return c.Delete(ctx, obj, opts...)
		}})
	_ = do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "failing"))
	if uid == nil {
		t.Fatal("the Secret was deleted without a UID precondition")
	}
}

func TestAddModelKeyNeverInResponsesOrLogsWhenCallsFail(t *testing.T) {
	leak := errors.New("backend echoed the request: Authorization=" + fakeKey)
	t.Run("Secret create fails", func(t *testing.T) {
		e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return leak
			}
			return c.Create(ctx, obj, opts...)
		}})
		rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "leaky"))
		if rec.Code < 500 || len(e.models(t)) != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		e.noKey(t, rec)
	})
	t.Run("dry run fails", func(t *testing.T) {
		e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if isDryRun(opts) {
				return apierrors.NewBadRequest(leak.Error())
			}
			return c.Create(ctx, obj, opts...)
		}})
		rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "leaky"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		e.noKey(t, rec)
	})
	t.Run("GitHub fails", func(t *testing.T) {
		e := modelFixture(t, true, interceptor.Funcs{})
		e.gh.failNextPullCreate = true
		rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "leaky"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		e.noKey(t, rec)
		// The Secret stays and is listed, with a way to remove it.
		if len(e.secrets(t)) != 1 {
			t.Fatal("the Secret is gone")
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		e := modelFixture(t, false, interceptor.Funcs{})
		req := httptest.NewRequest("POST", "/api/models", strings.NewReader(`{"apiKey":"`+fakeKey+`","credentials":5`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(e.ck)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		e.noKey(t, rec)
	})
}

func TestModelSecretListReturnsNamesOnly(t *testing.T) {
	mk := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}, Data: map[string][]byte{"Authorization": []byte("topsecret-" + name)}, StringData: map[string]string{"x": "topsecret"}}
	}
	e := modelFixture(t, false, interceptor.Funcs{}, mk("ambit-llm-b"), mk("ambit-llm-a"), mk("ambit-console-keys"), mk("unrelated"))
	rec := do(t, e.h, "GET", "/api/model-secrets", e.ck, nil)
	var got []string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || strings.Join(got, ",") != "ambit-llm-a,ambit-llm-b" || strings.Contains(rec.Body.String(), "topsecret") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func unfinishedSecret(name, model, op string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "ambit-console", "ambitproject.dev/model-secret": model},
		Annotations: map[string]string{"ambitproject.dev/operation": op, "ambitproject.dev/branch": "ambit/add-model-" + model + "-" + op}}}
}

func TestUnfinishedModelKeysAreListedAndRemovable(t *testing.T) {
	done := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: testNS, Annotations: map[string]string{"ambitproject.dev/operation": "opdone11"}},
		Spec: v1.ModelSpec{Provider: "OpenAI", Model: "m", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-done"}}}
	inUse := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "user", Namespace: testNS},
		Spec: v1.ModelSpec{Provider: "OpenAI", Model: "m", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-reused"}}}
	e := modelFixture(t, false, interceptor.Funcs{}, done, inUse,
		unfinishedSecret("ambit-llm-done", "done", "opdone11"),
		unfinishedSecret("ambit-llm-half", "half", "ophalf11"),
		unfinishedSecret("ambit-llm-reused", "gone", "opgone11"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-handmade", Namespace: testNS}, Data: map[string][]byte{"Authorization": []byte("k")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-nolabel", Namespace: testNS, Annotations: map[string]string{"ambitproject.dev/operation": "opxxxxxx"}}})
	var un []UnfinishedModel
	rec := do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &un)
	names := map[string]bool{}
	for _, u := range un {
		names[u.Secret] = true
	}
	if rec.Code != 200 || len(names) != 2 || !names["ambit-llm-half"] || !names["ambit-llm-reused"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for name, want := range map[string]int{
		"ambit-llm-handmade": http.StatusConflict, // not created by the console for a model
		"ambit-llm-nolabel":  http.StatusConflict, // annotation without the label
		"ambit-llm-reused":   http.StatusConflict, // a Model uses it
		"ambit-llm-missing":  http.StatusNotFound,
		"ambit-llm-half":     http.StatusNoContent, // unfinished
	} {
		if rec := do(t, e.h, "DELETE", "/api/models/unfinished/"+name, e.ck, nil); rec.Code != want {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	left := map[string]bool{}
	for _, s := range e.secrets(t) {
		left[s.Name] = true
	}
	if left["ambit-llm-half"] || !left["ambit-llm-handmade"] || !left["ambit-llm-nolabel"] || !left["ambit-llm-reused"] || !left["ambit-llm-done"] {
		t.Fatalf("left %v", left)
	}
}

func TestUnfinishedModelKeyInGitOpsWaitsForItsPullRequest(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{})
	_ = do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "gitm"))
	var un []UnfinishedModel
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 0 {
		t.Fatalf("listed while its pull request is open: %+v", un)
	}
	if rec := do(t, e.h, "DELETE", "/api/models/unfinished/ambit-llm-gitm", e.ck, nil); rec.Code != http.StatusConflict {
		t.Fatalf("removed while its pull request is open: %d", rec.Code)
	}
	st := e.gh.prsByBranch["ambit/add-model-gitm-op1abcde"]
	st.Closed = true
	e.gh.prsByBranch["ambit/add-model-gitm-op1abcde"] = st
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 1 || un[0].Secret != "ambit-llm-gitm" {
		t.Fatalf("closed without merge, not listed: %+v", un)
	}
}

func TestAddModelKeepsTheSecretWhenTheModelWasStoredDespiteAnError(t *testing.T) {
	for name, fail := range map[string]error{"canceled": context.Canceled, "timeout": context.DeadlineExceeded, "unavailable": apierrors.NewServiceUnavailable("slow")} {
		e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			err := c.Create(ctx, obj, opts...)
			if _, ok := obj.(*v1.Model); ok && !isDryRun(opts) && err == nil {
				return fail // stored, but the caller sees an error
			}
			return err
		}})
		rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "stored"))
		if rec.Code != http.StatusCreated || len(e.secrets(t)) != 1 || len(e.models(t)) != 1 {
			t.Errorf("%s: %d %s, %d secrets, %d models", name, rec.Code, rec.Body, len(e.secrets(t)), len(e.models(t)))
		}
	}
}

func TestAddModelLogsTheBaseURLAndTheSecretName(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{})
	in := enteredInput("op1abcde", "selfhosted")
	in.BaseURL = "http://vllm.models.svc:8000/v1"
	if rec := do(t, e.h, "POST", "/api/models", e.ck, in); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(e.logs.String(), "baseURL=http://vllm.models.svc:8000/v1") || !strings.Contains(e.logs.String(), "credentialsRef=ambit-llm-selfhosted") {
		t.Fatalf("log %s", e.logs)
	}
}

func TestRemoveKeyReportsAModelThatStartedUsingTheSecret(t *testing.T) {
	var e *modelEnv
	e = modelFixture(t, false, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetName() == "ambit-llm-half" { // a Model begins to use the Secret just before it is deleted
			_ = c.Create(ctx, &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: testNS},
				Spec: v1.ModelSpec{Provider: "OpenAI", Model: "m", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-half"}}})
		}
		return c.Delete(ctx, obj, opts...)
	}}, unfinishedSecret("ambit-llm-half", "half", "ophalf11"))
	rec := do(t, e.h, "DELETE", "/api/models/unfinished/ambit-llm-half", e.ck, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "late") || !strings.Contains(e.logs.String(), "level=WARN") ||
		!strings.Contains(e.logs.String(), "secret=ambit-llm-half") || !strings.Contains(e.logs.String(), "model=late") {
		t.Fatalf("%d %s\n%s", rec.Code, rec.Body, e.logs)
	}
}

func TestAddModelSameOperationWithOtherSettingsIsRefused(t *testing.T) {
	for _, gitops := range []bool{false, true} {
		first := enteredInput("op1abcde", "once")
		other := first
		other.Model = "a-different-model"
		e := modelFixture(t, gitops, interceptor.Funcs{})
		if gitops { // the merged pull request has reached the cluster
			_ = e.cl.Create(t.Context(), e.cat.modelFrom(first, "admin@acme.test"))
		} else if rec := do(t, e.h, "POST", "/api/models", e.ck, first); rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		rec := do(t, e.h, "POST", "/api/models", e.ck, other)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "operation op1abcde already added model once with other settings") {
			t.Fatalf("gitops=%v: %d %s", gitops, rec.Code, rec.Body)
		}
		if rec := do(t, e.h, "POST", "/api/models", e.ck, first); rec.Code != http.StatusOK {
			t.Fatalf("gitops=%v: the same input: %d %s", gitops, rec.Code, rec.Body)
		}
	}
}

func TestAddModelSecretCreateErrorThatStoredTheSecret(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		err := c.Create(ctx, obj, opts...)
		if _, ok := obj.(*corev1.Secret); ok && err == nil {
			return context.DeadlineExceeded // stored, but the answer was lost
		}
		return err
	}})
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "lost")); rec.Code != http.StatusCreated || len(e.models(t)) != 1 || len(e.secrets(t)) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// If the Model then fails, the Secret this request owns is removed.
	e = modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*v1.Model); ok && !isDryRun(opts) {
			return errors.New("no")
		}
		err := c.Create(ctx, obj, opts...)
		if _, ok := obj.(*corev1.Secret); ok && err == nil {
			return context.DeadlineExceeded
		}
		return err
	}})
	_ = do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "lost"))
	if len(e.secrets(t)) != 0 {
		t.Fatal("the Secret stayed")
	}
	// An error that stored nothing is a failure, and nothing is written.
	e = modelFixture(t, false, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			return errors.New("down")
		}
		return c.Create(ctx, obj, opts...)
	}})
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "none")); rec.Code != http.StatusBadGateway || len(e.models(t)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnfinishedListShowsAYoungSecretUnlessItsRequestIsRunning(t *testing.T) {
	young := unfinishedSecret("ambit-llm-half", "half", "ophalf11")
	young.CreationTimestamp = metav1.NewTime(time.Now())
	e := modelFixture(t, false, interceptor.Funcs{}, young)
	list := func() []UnfinishedModel {
		var un []UnfinishedModel
		_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
		return un
	}
	if len(list()) != 1 {
		t.Fatal("a Secret whose request died is hidden")
	}
	e.cat.modelOps.Store("ophalf11", true)
	if len(list()) != 0 {
		t.Fatal("a running request's Secret is listed")
	}
}

func TestAddModelRefusesAnUnknownProviderForEveryCredentialChoice(t *testing.T) {
	e := modelFixture(t, false, interceptor.Funcs{}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-a", Namespace: testNS}})
	for _, in := range []ModelAddInput{
		{Credentials: "entered", APIKey: fakeKey}, {Credentials: "existing", SecretName: "ambit-llm-a"}, {Credentials: "cloud"},
	} {
		in.Operation, in.Name, in.Model = "op1abcde", "prov", "m"
		for _, p := range []string{"Nonsense", "", "openai"} {
			in.Provider = p
			if rec := do(t, e.h, "POST", "/api/models", e.ck, in); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %q: %d %s", in.Credentials, p, rec.Code, rec.Body)
			}
		}
	}
	if len(e.models(t)) != 0 || len(e.secrets(t)) != 1 {
		t.Fatal("something was written")
	}
}

func TestModelProvidersMatchTheCRDEnum(t *testing.T) {
	src, err := os.ReadFile("../../api/v1alpha1/types.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`Enum=((?:[A-Za-z]+;)+[A-Za-z]+)\n\tProvider string`).FindSubmatch(src)
	if m == nil {
		t.Fatal("provider enum not found")
	}
	if got := strings.Split(string(m[1]), ";"); !slices.Equal(got, modelProviders) {
		t.Fatalf("CRD %v, console %v", got, modelProviders)
	}
}

// What a Git host or Kubernetes says is logged, not shown; and these paths never read a Secret's data.
func TestModelKeyRoutesAnswerGenericallyAndReadMetadataOnly(t *testing.T) {
	var readData []string
	e := modelFixture(t, true, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && key.Name != "ambit-llm-gitm" { // the retry check of an entered key reads its own Secret
				readData = append(readData, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				readData = append(readData, "list")
			}
			return c.List(ctx, list, opts...)
		}}, unfinishedSecret("ambit-llm-half", "half", "ophalf11"))
	e.cat.GitOps.Repo = failingRepo{PullRequester: e.cat.GitOps.Repo}
	for _, req := range []struct{ method, path string }{{"GET", "/api/models/unfinished"}, {"DELETE", "/api/models/unfinished/ambit-llm-half"}} {
		rec := do(t, e.h, req.method, req.path, e.ck, nil)
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "ghs_LEAK") || !strings.Contains(rec.Body.String(), "could not read pull request state from") {
			t.Errorf("%s: %d %s", req.path, rec.Code, rec.Body)
		}
	}
	if !strings.Contains(e.logs.String(), "ghs_LEAK") {
		t.Error("the detail was not logged")
	}
	do(t, e.h, "GET", "/api/model-secrets", e.ck, nil)
	if len(readData) != 0 {
		t.Fatalf("Secret data was read: %v", readData)
	}
}

type failingRepo struct{ PullRequester }

func (failingRepo) PullRequestByBranch(context.Context, string) (PullRequestState, bool, error) {
	return PullRequestState{}, false, errors.New("github said: bad credentials ghs_LEAK")
}

func TestAddModelGitOpsClosedPullRequestMovesToANewBranch(t *testing.T) {
	e := modelFixture(t, true, interceptor.Funcs{})
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "gitm")); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st := e.gh.prsByBranch["ambit/add-model-gitm-op1abcde"]
	st.Closed = true
	e.gh.prsByBranch["ambit/add-model-gitm-op1abcde"] = st
	if rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "gitm")); rec.Code != http.StatusAccepted || e.gh.pulls != 2 {
		t.Fatalf("%d %s, %d pulls", rec.Code, rec.Body, e.gh.pulls)
	}
	var sec metav1.PartialObjectMetadata
	sec.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	_ = e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "ambit-llm-gitm"}, &sec)
	if !strings.HasPrefix(sec.Annotations["ambitproject.dev/branch"], "ambit/add-model-gitm-op1abcde-r") || len(e.secrets(t)) != 1 {
		t.Fatalf("branch %q", sec.Annotations["ambitproject.dev/branch"])
	}
}

func TestRemoveKeyLogsAListErrorAfterTheDelete(t *testing.T) {
	var lists int
	e := modelFixture(t, false, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if _, ok := l.(*v1.ModelList); ok {
			if lists++; lists > 1 { // the check before the delete worked; the re-check after it fails
				return errors.New("list failed")
			}
		}
		return c.List(ctx, l, opts...)
	}}, unfinishedSecret("ambit-llm-half", "half", "ophalf11"))
	rec := do(t, e.h, "DELETE", "/api/models/unfinished/ambit-llm-half", e.ck, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "re-check") || strings.Contains(rec.Body.String(), "list failed") ||
		!strings.Contains(e.logs.String(), "level=WARN") || !strings.Contains(e.logs.String(), "secret=ambit-llm-half") {
		t.Fatalf("%d %s\n%s", rec.Code, rec.Body, e.logs)
	}
}

func TestAddModelReadOfAnAmbiguousSecretCreateHasATimeout(t *testing.T) {
	var deadline bool
	e := modelFixture(t, false, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			err := c.Create(ctx, obj, opts...)
			if _, ok := obj.(*corev1.Secret); ok && err == nil {
				return context.DeadlineExceeded
			}
			return err
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				_, deadline = ctx.Deadline()
			}
			return c.Get(ctx, key, obj, opts...)
		}})
	do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "lost"))
	if !deadline {
		t.Fatal("the Secret read after an ambiguous create has no timeout")
	}
}
