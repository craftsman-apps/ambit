package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	for _, gitops := range []bool{false, true} {
		e := modelFixture(t, gitops, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if isDryRun(opts) {
				return apierrors.NewInvalid(schema.GroupKind{Group: "ambitproject.dev", Kind: "Model"}, obj.GetName(), nil)
			}
			return c.Create(ctx, obj, opts...)
		}})
		rec := do(t, e.h, "POST", "/api/models", e.ck, enteredInput("op1abcde", "bad"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("gitops=%v: %d %s", gitops, rec.Code, rec.Body)
		}
		if len(e.secrets(t)) != 0 || len(e.models(t)) != 0 || (gitops && e.gh.pulls != 0) {
			t.Fatalf("gitops=%v: something was written", gitops)
		}
		e.noKey(t, rec)
	}
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
