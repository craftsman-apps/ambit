package console

// Add model against a real API server: the dry-run create applies the Model CRD's own rules, and nothing is
// written when it refuses. Run with `make test-go`; plain `go test` skips it.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestAddModelDryRunAppliesTheCRDRules(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (run make test-go)")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "charts", "ambit-crds", "templates", "ambit"), filepath.Join("..", "..", "deploy", "crds")}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	_ = clientgoscheme.AddToScheme(clientgoscheme.Scheme)
	_ = v1.AddToScheme(clientgoscheme.Scheme)
	cl, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}); err != nil {
		t.Fatal(err)
	}
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	h := NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: cl, Namespace: testNS})
	ck := as(auth, RoleAdmin)

	bad := enteredInput("op1abcde", "bad")
	bad.Azure = &v1.AzureSettings{ResourceName: "acme", ResourceType: "OpenAI"} // azure settings only with provider Azure
	rec := do(t, h, "POST", "/api/models", ck, bad)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "azure settings are required with provider Azure and only there") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var secs corev1.SecretList
	var ms v1.ModelList
	_ = cl.List(t.Context(), &secs, client.InNamespace(testNS))
	_ = cl.List(t.Context(), &ms, client.InNamespace(testNS))
	if len(secs.Items) != 0 || len(ms.Items) != 0 {
		t.Fatalf("the refused addition wrote %d secrets, %d models", len(secs.Items), len(ms.Items))
	}

	if rec := do(t, h, "POST", "/api/models", ck, enteredInput("op2abcde", "good")); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var m v1.Model
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "good"}, &m); err != nil || m.Spec.CredentialsRef.Name != "ambit-llm-good" {
		t.Fatalf("%+v %v", m, err)
	}
	if rec := do(t, h, "POST", "/api/models", ck, enteredInput("op3abcde", "good")); rec.Code != http.StatusConflict {
		t.Fatalf("same name again: %d %s", rec.Code, rec.Body)
	}
}
