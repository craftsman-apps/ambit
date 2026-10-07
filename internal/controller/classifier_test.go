package controller

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClassifierStateFromDeployment(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	d := func(want, avail int32) *appsv1.Deployment {
		return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &want}, Status: appsv1.DeploymentStatus{AvailableReplicas: avail}}
	}
	for name, tc := range map[string]struct {
		d      *appsv1.Deployment
		wanted bool
		since  time.Time
		state  string
		reason string
	}{
		"off":               {d(0, 0), false, time.Time{}, "Off", ""},
		"starting":          {d(2, 0), true, now.Add(-time.Minute), "Starting", ""},
		"ready":             {d(2, 2), true, time.Time{}, "Ready", ""},
		"unhealthy":         {d(2, 1), true, time.Time{}, "Unhealthy", "ClassifierUnhealthy"},
		"start fails":       {d(2, 0), true, now.Add(-16 * time.Minute), "Starting", "ClassifierStartFailed"},
		"first time seen":   {d(2, 0), true, time.Time{}, "Starting", ""},
		"missing":           {nil, true, time.Time{}, "Off", "ClassifierNotInstalled"},
		"missing, unwanted": {nil, false, time.Time{}, "Off", ""},
	} {
		s, c := classifierState(tc.d, tc.wanted, tc.since, now)
		if s != tc.state || c.Reason != tc.reason {
			t.Errorf("%s: %s %s", name, s, c.Reason)
		}
	}
}

func TestCredentialsObject(t *testing.T) {
	cfg := routerconfig.Config{Routers: []routerconfig.Router{{Name: "auto", LLM: &routerconfig.LLM{CredentialKey: "local"}}}}
	o, err := credentialsObject("ns", cfg, map[string]string{"local": "sk-local", "unused": "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(o.Object, "data")
	if o.GetKind() != "Secret" || o.GetAPIVersion() != "v1" || o.GetName() != routerconfig.SecretName || o.GetNamespace() != "ns" ||
		o.GetLabels()["app.kubernetes.io/managed-by"] != "ambit" || len(data) != 1 || data["local"] != base64.StdEncoding.EncodeToString([]byte("sk-local")) {
		t.Fatalf("%v", o.Object)
	}
}

// Only requests for a router of type LLM go to ambit-router: the condition reads the requested model, so other chat
// requests do not pay the round trip (performance row, direct chat CPU). Wire names are quoted like keywords.
// Bodies over 4,000,000 bytes skip it (no label): agentgateway v1.6.0 buffers up to 32 MiB, and ambit-router's gRPC
// server takes at most 4 MiB per message, so a larger body would fail the request with 500 once it is sent.
func TestClassifyPolicyConditionNamesRouters(t *testing.T) {
	cfg := routerconfig.Config{Hash: "h", Routers: []routerconfig.Router{{Name: "a", Wire: "auto"}, {Name: "b", Wire: `team/"x"`}}}
	cs, _, _ := unstructured.NestedSlice(classifyPolicy("ns", "ambit-llm", "ambit-router", cfg).Object, "spec", "traffic", "extProc", "conditional")
	got := cs[0].(map[string]any)["condition"]
	want := `request.path == "/v1/chat/completions" && size(request.body) <= 4000000 && json(request.body).model in ["auto", "team/\"x\""]`
	if got != want {
		t.Fatalf("condition\n got %s\nwant %s", got, want)
	}
}
