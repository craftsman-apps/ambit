package console

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func modeFixture(t *testing.T, accessModeFlag string, data map[string]string) (http.Handler, *Auth) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	if data != nil {
		b = b.WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ambit-access-mode", Namespace: testNS}, Data: data})
	}
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: b.Build(), Namespace: testNS, AccessMode: accessModeFlag}), auth
}

// /api/config tells the web UI the access mode, from what the controller published (not from the flag, which
// is only the target of a switch that may not be active yet).
func TestConfigReportsPublishedAccessMode(t *testing.T) {
	h, a := modeFixture(t, "audit", map[string]string{"mode": "audit", "modeSince": "2026-10-12T10:00:00Z",
		"rulesVersion": "3f9a1c2b7d10", "rulesSince": "2026-10-14T16:20:00Z"})
	cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "x@acme.test", RoleViewer), nil).Body.String())
	if cfg.AccessMode != "audit" || cfg.ModeSince != "2026-10-12T10:00:00Z" || cfg.RulesVersion != "3f9a1c2b7d10" || cfg.RulesSince != "2026-10-14T16:20:00Z" || cfg.Pending != nil {
		t.Fatalf("%+v", cfg)
	}
}

// Before the controller has published anything (a new install, or an old controller), the flag gives the mode;
// without both, the mode is enforce.
func TestConfigAccessModeFallsBack(t *testing.T) {
	for flag, want := range map[string]string{"audit": "audit", "enforce": "enforce", "": "enforce"} {
		h, a := modeFixture(t, flag, nil)
		cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "x@acme.test", RoleViewer), nil).Body.String())
		if cfg.AccessMode != want || cfg.ModeSince != "" {
			t.Errorf("flag %q: %+v, want mode %q", flag, cfg, want)
		}
	}
}

// A switch that is waiting for agentgateway is shown to admins only; the published values stay the old ones.
func TestConfigPendingIsForAdmins(t *testing.T) {
	h, a := modeFixture(t, "audit", map[string]string{"mode": "enforce", "modeSince": "2026-10-01T00:00:00Z", "rulesVersion": "aaaaaaaaaaaa", "rulesSince": "2026-10-01T00:00:00Z",
		"pending": `{"mode":"audit","rulesVersion":"bbbbbbbbbbbb","step":"authorization","error":"waiting for agentgateway to apply AgentgatewayModel/m generation 3"}`})
	cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "priya@acme.test", RoleAdmin), nil).Body.String())
	if cfg.AccessMode != "enforce" || cfg.Pending == nil || cfg.Pending.Mode != "audit" || cfg.Pending.Step != "authorization" || cfg.Pending.Error == "" {
		t.Fatalf("admin %+v", cfg)
	}
	cfg = decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "x@acme.test", RoleViewer), nil).Body.String())
	if cfg.AccessMode != "enforce" || cfg.Pending != nil {
		t.Fatalf("viewer %+v", cfg)
	}
}

// The rejected objects the controller names reach the admin.
func TestConfigPendingNamesTheRejectedObjects(t *testing.T) {
	h, a := modeFixture(t, "", map[string]string{"mode": "enforce",
		"pending": `{"mode":"enforce","rulesVersion":"bbbbbbbbbbbb","step":"authorization","error":"agentgateway rejected Model/mistral-x: bad key. Check the provider secret of Model/mistral-x, or remove the Model.","objects":["Model/mistral-x"]}`})
	cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "priya@acme.test", RoleAdmin), nil).Body.String())
	if cfg.Pending == nil || len(cfg.Pending.Objects) != 1 || cfg.Pending.Objects[0] != "Model/mistral-x" {
		t.Fatalf("%+v", cfg.Pending)
	}
}

// A broken pending entry never hides the published mode.
func TestConfigBrokenPendingIsIgnored(t *testing.T) {
	h, a := modeFixture(t, "", map[string]string{"mode": "audit", "pending": "{not json"})
	cfg := decodeAs[Config](t, do(t, h, "GET", "/api/config", user(a, "priya@acme.test", RoleAdmin), nil).Body.String())
	if cfg.AccessMode != "audit" || cfg.Pending != nil {
		t.Fatalf("%+v", cfg)
	}
}
