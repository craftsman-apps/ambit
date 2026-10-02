package console

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func priceFixture(t *testing.T, gitops bool) (http.Handler, client.Client, *Auth, *recordingRepo) {
	t.Helper()
	_, c, a := serversFixture(t)
	_ = c.Create(t.Context(), &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gpt", Namespace: testNS, Labels: map[string]string{"team": "x"}},
		Spec: v1.ModelSpec{Provider: "OpenAI", Model: "gpt-4.1", DisplayName: "GPT", CredentialsRef: v1.SecretRef{Name: "ambit-llm-openai"}}})
	l, _ := ParseLiteLLM([]byte(sampleList), "built in", "", "")
	cat := &Catalog{Client: c, Namespace: testNS, Prices: &PriceSource{list: l}}
	repo := &recordingRepo{}
	if gitops {
		cat.GitOps = &GitOps{Repo: repo, Repository: "acme/config", Branch: "main", Dir: "ambit"}
	}
	return NewServer(a, testSPA, LoginOptions{}, cat), c, a, repo
}

func TestPriceSuggestionAndEdit(t *testing.T) {
	h, c, a, _ := priceFixture(t, false)
	admin := user(a, "priya@acme.test", RoleAdmin)
	g := decodeAs[map[string]any](t, do(t, h, "GET", "/api/models/gpt/price", admin, nil).Body.String())
	s := g["suggestion"].(map[string]any)
	if g["price"] != nil || s["inputPerMillionTokens"] != "2" || s["outputPerMillionTokens"] != "8" {
		t.Fatalf("%v", g)
	}
	var m v1.Model
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "gpt"}, &m)
	if m.Spec.Price != nil {
		t.Fatal("a suggestion alone changed the price")
	}
	if r := do(t, h, "PUT", "/api/models/gpt/price", admin, map[string]string{"inputPerMillionTokens": "1.8", "outputPerMillionTokens": "7.2"}); r.Code != http.StatusOK {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "gpt"}, &m)
	if m.Spec.Price == nil || m.Spec.Price.InputPerMillionTokens != "1.8" || m.Spec.DisplayName != "GPT" {
		t.Fatalf("%+v", m.Spec)
	}
	if r := do(t, h, "PUT", "/api/models/gpt/price", admin, map[string]string{"inputPerMillionTokens": "-1", "outputPerMillionTokens": "1"}); r.Code != http.StatusBadRequest {
		t.Fatalf("negative price: %d", r.Code)
	}
	if r := do(t, h, "PUT", "/api/models/gpt/price", user(a, "alice@acme.test", RoleViewer), map[string]string{"inputPerMillionTokens": "1", "outputPerMillionTokens": "1"}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d", r.Code)
	}
}

func TestRemovePriceKeepsOtherFields(t *testing.T) {
	h, c, a, _ := priceFixture(t, false)
	admin := user(a, "priya@acme.test", RoleAdmin)
	do(t, h, "PUT", "/api/models/gpt/price", admin, map[string]string{"inputPerMillionTokens": "1", "outputPerMillionTokens": "1"})
	if r := do(t, h, "PUT", "/api/models/gpt/price", admin, map[string]bool{"remove": true}); r.Code != http.StatusOK {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	var m v1.Model
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "gpt"}, &m)
	if m.Spec.Price != nil || m.Spec.Model != "gpt-4.1" || m.Spec.CredentialsRef.Name != "ambit-llm-openai" || m.Labels["team"] != "x" {
		t.Fatalf("%+v %v", m.Spec, m.Labels)
	}
}

func TestPriceEditInGitOpsOpensAPullRequest(t *testing.T) {
	h, c, a, repo := priceFixture(t, true)
	r := do(t, h, "PUT", "/api/models/gpt/price", user(a, "priya@acme.test", RoleAdmin), map[string]string{"inputPerMillionTokens": "1", "outputPerMillionTokens": "4"})
	if r.Code != http.StatusAccepted || len(repo.files) != 1 || repo.files[0].Path != "ambit/models/gpt.yaml" {
		t.Fatalf("%d %s %v", r.Code, r.Body, repo.files)
	}
	var m v1.Model
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "gpt"}, &m)
	if m.Spec.Price != nil {
		t.Fatal("GitOps mode changed the cluster")
	}
}

// The pull request rewrites the whole file: it must keep the object's labels (here team: x).
func TestPriceEditInGitOpsKeepsLabels(t *testing.T) {
	h, _, a, repo := priceFixture(t, true)
	r := do(t, h, "PUT", "/api/models/gpt/price", user(a, "priya@acme.test", RoleAdmin), map[string]string{"inputPerMillionTokens": "1", "outputPerMillionTokens": "4"})
	if r.Code != http.StatusAccepted || len(repo.files) != 1 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if got := string(repo.files[0].Content); !strings.Contains(got, "labels:\n    team: x\n") {
		t.Fatalf("labels dropped:\n%s", got)
	}
}
