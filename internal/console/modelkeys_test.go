package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const newFakeKey = "sk-test-NEW-9876543210"

// consoleKeyModel is a Model with the key Secret Add model would have created for it.
func consoleKeyModel(name, provider string) (*v1.Model, *corev1.Secret) {
	m := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: k8stypes.UID("model-uid-" + name), Annotations: map[string]string{operationKey: "op" + name}},
		Spec: v1.ModelSpec{Provider: provider, Model: "gpt-4.1-mini", CredentialsRef: &v1.SecretRef{Name: llmSecretPrefix + name}}}
	sec := unfinishedSecret(llmSecretPrefix+name, name, "op"+name)
	sec.UID = k8stypes.UID("secret-uid-" + name)
	sec.Data = map[string][]byte{"Authorization": []byte(fakeKey), "sessionToken": []byte("old-session")}
	return m, sec
}

func (e *modelEnv) secret(t *testing.T, name string) (corev1.Secret, bool) {
	t.Helper()
	var s corev1.Secret
	err := e.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return s, false
	} else if err != nil {
		t.Fatal(err)
	}
	return s, true
}

func (e *modelEnv) noKeys(t *testing.T, body string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if strings.Contains(body, k) || strings.Contains(e.logs.String(), k) {
			t.Errorf("a key is in the answer or the log: %s / %s", body, e.logs)
		}
	}
}

func TestChangeKeyUpdatesTheSameSecretInPlace(t *testing.T) {
	for _, gitops := range []bool{false, true} {
		m, sec := consoleKeyModel("gpt", "OpenAI")
		e := modelFixture(t, gitops, interceptor.Funcs{}, m, sec)
		rec := do(t, e.h, "PUT", "/api/models/gpt/key", e.ck, ModelKeyInput{APIKey: " " + newFakeKey + "\n"})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("gitops=%v: %d %s", gitops, rec.Code, rec.Body)
		}
		got, _ := e.secret(t, "ambit-llm-gpt")
		if got.UID != sec.UID || string(got.Data["Authorization"]) != newFakeKey {
			t.Fatalf("gitops=%v: the Secret was not changed in place: uid %s", gitops, got.UID)
		}
		if _, ok := got.Data["sessionToken"]; ok {
			t.Errorf("an old entry stayed: %v", keysOf(got.Data))
		}
		if got.Annotations[keyChangedByKey] != "admin@example.com" || got.Annotations[keyChangedAtKey] == "" || got.Labels[modelSecretLabel] != "gpt" {
			t.Errorf("metadata %+v %+v", got.Labels, got.Annotations)
		}
		if gitops && len(e.gh.prs) != 0 {
			t.Errorf("a key change opened a pull request: %v", e.gh.prs)
		}
		ms := e.models(t)
		if len(ms) != 1 || ms[0].Spec.CredentialsRef.Name != "ambit-llm-gpt" {
			t.Errorf("the Model changed: %+v", ms)
		}
		e.noKeys(t, rec.Body.String(), fakeKey, newFakeKey)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestChangeKeyRefuses(t *testing.T) {
	ownKey := func(name string) (*v1.Model, *corev1.Secret) { return consoleKeyModel(name, "OpenAI") }
	cloud := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "bed", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "Bedrock", Model: "x"}}
	external := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-ext"}}}
	externalSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-ext", Namespace: testNS}, Data: map[string][]byte{"Authorization": []byte("k")}}
	// A Model that names another model's console key Secret.
	other := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "borrow", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-own"}}}
	missing := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "nosec", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-nosec"}}}
	own, ownSec := ownKey("own")
	e := modelFixture(t, false, interceptor.Funcs{}, cloud, external, externalSec, other, missing, own, ownSec)
	for path, want := range map[string]struct {
		code int
		msg  string
	}{
		"bed":    {http.StatusBadRequest, "not changed in the console"},
		"ext":    {http.StatusConflict, "not created by the console"},
		"borrow": {http.StatusConflict, "not created by the console"},
		"nosec":  {http.StatusConflict, "does not exist"},
		"absent": {http.StatusNotFound, "no model named absent"},
	} {
		rec := do(t, e.h, "PUT", "/api/models/"+path+"/key", e.ck, ModelKeyInput{APIKey: newFakeKey})
		if rec.Code != want.code || !strings.Contains(rec.Body.String(), want.msg) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
		e.noKeys(t, rec.Body.String(), newFakeKey)
	}
	if rec := do(t, e.h, "PUT", "/api/models/own/key", e.ck, ModelKeyInput{APIKey: "  "}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty key: %d", rec.Code)
	}
	if got, _ := e.secret(t, "ambit-llm-ext"); string(got.Data["Authorization"]) != "k" {
		t.Error("the external Secret was changed")
	}
	if got, _ := e.secret(t, "ambit-llm-own"); string(got.Data["Authorization"]) != fakeKey {
		t.Error("a refused change wrote the Secret")
	}
}

func TestChangeKeyConflictAndErrorsAreRedacted(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	var fail error
	e := modelFixture(t, false, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		return fail
	}}, m, sec)
	fail = apierrors.NewConflict(corev1.Resource("secrets"), "ambit-llm-gpt", errors.New("changed"))
	if rec := do(t, e.h, "PUT", "/api/models/gpt/key", e.ck, ModelKeyInput{APIKey: newFakeKey}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "try again") {
		t.Errorf("conflict: %d %s", rec.Code, rec.Body)
	}
	fail = apierrors.NewBadRequest("bad data " + newFakeKey)
	rec := do(t, e.h, "PUT", "/api/models/gpt/key", e.ck, ModelKeyInput{APIKey: newFakeKey})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "[key]") {
		t.Errorf("bad request: %d %s", rec.Code, rec.Body)
	}
	fail = errors.New("connection reset, body had " + newFakeKey)
	rec = do(t, e.h, "PUT", "/api/models/gpt/key", e.ck, ModelKeyInput{APIKey: newFakeKey})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("other error: %d %s", rec.Code, rec.Body)
	}
	e.noKeys(t, rec.Body.String(), newFakeKey)
}

func TestModelKeyAndDeleteAreForAdminsOnly(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	e := modelFixture(t, false, interceptor.Funcs{}, m, sec)
	for _, role := range []string{RoleViewer, RoleApprover} {
		ck := as(e.auth, role)
		if rec := do(t, e.h, "PUT", "/api/models/gpt/key", ck, ModelKeyInput{APIKey: newFakeKey}); rec.Code != http.StatusForbidden {
			t.Errorf("%s change key: %d", role, rec.Code)
		}
		if rec := do(t, e.h, "DELETE", "/api/models/gpt", ck, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s delete: %d", role, rec.Code)
		}
	}
	if len(e.models(t)) != 1 {
		t.Fatal("a non-admin deleted the model")
	}
}

func TestModelListShowsWhichKeysTheConsoleCanChange(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	external := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-ext"}}}
	e := modelFixture(t, false, interceptor.Funcs{}, m, sec, external)
	view := func(role string) map[string]ModelView {
		var vs []ModelView
		_ = json.Unmarshal(do(t, e.h, "GET", "/api/models", as(e.auth, role), nil).Body.Bytes(), &vs)
		out := map[string]ModelView{}
		for _, v := range vs {
			out[v.Name] = v
		}
		return out
	}
	a := view(RoleAdmin)
	if !a["gpt"].ConsoleKey || a["gpt"].KeySecret != "ambit-llm-gpt" || a["ext"].ConsoleKey || a["ext"].KeySecret != "ambit-llm-ext" {
		t.Errorf("admin view %+v", a)
	}
	if u := view(RoleViewer); u["gpt"].ConsoleKey {
		t.Errorf("a user sees consoleKey: %+v", u)
	}
}

func rule(name string, models ...string) *v1.AccessRule {
	return &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: models}}
}

func TestDeleteModelRefusedWhileNamed(t *testing.T) {
	llm := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: testNS}, Spec: v1.ModelRouterSpec{Default: "cheap",
		Classifier: v1.RouterClassifier{Type: "LLM", LLM: &v1.RouterLLM{Model: "gpt"}}}}
	routed := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: testNS}, Spec: v1.ModelRouterSpec{Default: "cheap",
		Routes: []v1.RouterRoute{{Model: "gpt"}}}}
	byDefault := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "dflt", Namespace: testNS}, Spec: v1.ModelRouterSpec{Default: "gpt"}}
	fallback := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "y", Fallback: []string{"gpt"}}}
	for name, objs := range map[string][]client.Object{
		"the access rules eng":                 {rule("eng", "other", "gpt")},
		"the model routers auto":               {llm},
		"the model routers route":              {routed},
		"the model routers dflt":               {byDefault},
		"the models main (as a fallback)":      {fallback},
		"the access rules eng and the models ": {rule("eng", "gpt"), fallback},
	} {
		for _, gitops := range []bool{false, true} {
			m, sec := consoleKeyModel("gpt", "OpenAI")
			e := modelFixture(t, gitops, interceptor.Funcs{}, append(objs, m, sec)...)
			rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), name) {
				t.Errorf("%s gitops=%v: %d %s", name, gitops, rec.Code, rec.Body)
			}
			if len(e.models(t)) == 0 || len(e.secrets(t)) != 1 {
				t.Errorf("%s: something was deleted", name)
			}
			if gitops && len(e.gh.prs) != 0 {
				t.Errorf("%s: a pull request was opened", name)
			}
		}
	}
}

func TestDeleteModelDirect(t *testing.T) {
	t.Run("removes the Model and its console key Secret", func(t *testing.T) {
		m, sec := consoleKeyModel("gpt", "OpenAI")
		e := modelFixture(t, false, interceptor.Funcs{}, m, sec, rule("other", "else"))
		rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
		var out ModelDeleted
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK || out != (ModelDeleted{Secret: "ambit-llm-gpt", SecretRemoved: true}) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if len(e.models(t)) != 0 || len(e.secrets(t)) != 0 {
			t.Fatalf("left %d models, %d secrets", len(e.models(t)), len(e.secrets(t)))
		}
		if !strings.Contains(e.logs.String(), "model deleted") {
			t.Errorf("no log line: %s", e.logs)
		}
	})
	t.Run("keeps an external Secret", func(t *testing.T) {
		m := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "x", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-ext"}}}
		e := modelFixture(t, false, interceptor.Funcs{}, m, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-ext", Namespace: testNS}})
		rec := do(t, e.h, "DELETE", "/api/models/ext", e.ck, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not created by the console") || len(e.secrets(t)) != 1 || len(e.models(t)) != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("keeps a Secret another Model's additional endpoint uses", func(t *testing.T) {
		m, sec := consoleKeyModel("gpt", "OpenAI")
		user := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "y",
			CredentialsRef: &v1.SecretRef{Name: "ambit-llm-multi"}, AdditionalEndpoints: []v1.ModelEndpoint{{BaseURL: "http://b/v1", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-gpt"}}}}}
		e := modelFixture(t, false, interceptor.Funcs{}, m, sec, user)
		rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "used by the model multi") || len(e.secrets(t)) != 1 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a cloud identity model has no Secret", func(t *testing.T) {
		m := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "bed", Namespace: testNS}, Spec: v1.ModelSpec{Provider: "Bedrock", Model: "x"}}
		e := modelFixture(t, false, interceptor.Funcs{}, m)
		rec := do(t, e.h, "DELETE", "/api/models/bed", e.ck, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"secret":""`) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("refuses a model Argo CD manages", func(t *testing.T) {
		m, sec := consoleKeyModel("gpt", "OpenAI")
		m.Annotations["argocd.argoproj.io/tracking-id"] = "x"
		e := modelFixture(t, false, interceptor.Funcs{}, m, sec)
		if rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil); rec.Code != http.StatusConflict || len(e.models(t)) != 1 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("404", func(t *testing.T) {
		e := modelFixture(t, false, interceptor.Funcs{})
		if rec := do(t, e.h, "DELETE", "/api/models/none", e.ck, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestDeleteModelSecretFailureIsListed(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	e := modelFixture(t, false, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetName() == "ambit-llm-gpt" {
			return apierrors.NewServiceUnavailable("slow")
		}
		return c.Delete(ctx, obj, opts...)
	}}, m, sec)
	rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"secretRemoved":false`) || !strings.Contains(rec.Body.String(), "Keys without a model") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var un []UnfinishedModel
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 1 || un[0].Secret != "ambit-llm-gpt" {
		t.Fatalf("the kept Secret is not listed: %+v", un)
	}
}

func TestDeleteModelGitOps(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	e := modelFixture(t, true, interceptor.Funcs{}, m, sec)
	e.gh.files = map[string]string{"ambit/models/gpt.yaml": "kind: Model"}
	rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
	var p Proposed
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusAccepted || !strings.HasPrefix(p.PullRequest.Branch, "ambit/delete-model-gpt-") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.gh.fileCommits["ambit/models/gpt.yaml"] != "delete" {
		t.Errorf("the pull request does not delete the manifest: %v", e.gh.fileCommits)
	}
	got, _ := e.secret(t, "ambit-llm-gpt")
	if got.Annotations[deleteBranchKey] != p.PullRequest.Branch || len(e.models(t)) != 1 {
		t.Fatalf("the Secret is not marked, or the Model was deleted: %+v", got.Annotations)
	}
	// A second click returns the same pull request.
	again := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
	var p2 Proposed
	_ = json.Unmarshal(again.Body.Bytes(), &p2)
	if again.Code != http.StatusAccepted || p2.PullRequest.Number != p.PullRequest.Number || e.gh.pulls != 1 {
		t.Fatalf("second delete: %d %s, %d pull requests", again.Code, again.Body, e.gh.pulls)
	}
	// Before the merge the key is not listed; after the merge and the sync (the Model is gone) it is, and can be removed.
	var un []UnfinishedModel
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 0 {
		t.Fatalf("listed while the Model exists: %+v", un)
	}
	st := e.gh.prsByBranch["ambit/add-model-gpt-opgpt"]
	st.Merged = true // the add pull request was merged long ago
	e.gh.prsByBranch["ambit/add-model-gpt-opgpt"] = st
	if err := e.cl.Delete(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(do(t, e.h, "GET", "/api/models/unfinished", e.ck, nil).Body.Bytes(), &un)
	if len(un) != 1 || !un[0].Deleted || un[0].Secret != "ambit-llm-gpt" {
		t.Fatalf("after the merge: %+v", un)
	}
	if rec := do(t, e.h, "DELETE", "/api/models/unfinished/ambit-llm-gpt", e.ck, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if _, ok := e.secret(t, "ambit-llm-gpt"); ok {
		t.Fatal("the Secret is still there")
	}
}

func TestDeleteModelGitOpsManifestNotInRepository(t *testing.T) {
	m, sec := consoleKeyModel("gpt", "OpenAI")
	e := modelFixture(t, true, interceptor.Funcs{}, m, sec)
	rec := do(t, e.h, "DELETE", "/api/models/gpt", e.ck, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not created through this repository") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got, _ := e.secret(t, "ambit-llm-gpt"); got.Annotations[deleteBranchKey] != "" {
		t.Fatal("the Secret was marked without a pull request")
	}
}

func TestRedactSeveral(t *testing.T) {
	msg := redact("a=sk-one b=c2stdHdv c=", "sk-one", "", "sk-two")
	if msg != "a=[key] b=[key] c=" {
		t.Fatal(msg)
	}
}
