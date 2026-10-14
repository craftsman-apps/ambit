package checks

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

func TestRunTurnsAPanicIntoUnknown(t *testing.T) {
	old := Catalogue
	t.Cleanup(func() { Catalogue = old })
	Catalogue = []Check{{"X1", GroupPre, func(context.Context, *Env) Result { var m map[string]int; m["a"]++; return Result{} }}}
	rs := Run(context.Background(), fx{}.env(t), map[string]bool{GroupPre: true})
	if len(rs) != 1 || rs[0].ID != "X1" || rs[0].Status != Unknown || !strings.Contains(rs[0].Message, "internal error") {
		t.Fatalf("rs = %+v", rs)
	}
}

func TestSummaryAndExitCode(t *testing.T) {
	rs := []Result{{Status: Pass}, {Status: Pass}, {Status: Warn}, {Status: Fail}, {Status: Unknown}, {Status: NA}}
	if s := Summary(rs); s != "2 passed, 1 warning, 1 failed, 1 unknown, 1 n/a" {
		t.Fatalf("summary %q", s)
	}
	if s := Summary(rs[:3]); s != "2 passed, 1 warning, 0 failed" {
		t.Fatalf("summary %q", s)
	}
	if ExitCode(rs, false) != 1 || ExitCode(rs[:3], false) != 0 {
		t.Fatal("exit code: a failed check must give 1, warnings 0")
	}
	if ExitCode([]Result{{Status: Unknown}}, false) != 0 || ExitCode([]Result{{Status: Unknown}}, true) != 1 {
		t.Fatal("--fail-on-unknown")
	}
}

func TestPrint(t *testing.T) {
	var b bytes.Buffer
	p := out.New(out.Options{Out: &b, Err: &b}, &out.Redactor{})
	Print(p, []Result{
		{ID: "P1", Status: Fail, Message: "gateway.replicas is 1; production needs 2 or more", Next: "set gateway.replicas: 2", Source: "values, PDB, Deployment"},
		{ID: "P7", Status: NA, Message: "no MCP Server", Source: "values, objects"},
	})
	want := "✗ P1  gateway.replicas is 1; production needs 2 or more [values, PDB, Deployment]\n      next: set gateway.replicas: 2\n" +
		"- P7  no MCP Server [values, objects]\n0 passed, 0 warnings, 1 failed, 1 n/a\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestRunSelectsGroups(t *testing.T) {
	rs := Run(context.Background(), fx{}.env(t), map[string]bool{GroupProduction: true})
	if len(rs) != 10 || rs[0].ID != "P1" || rs[9].ID != "P10" {
		t.Fatalf("production group = %d results, first %s", len(rs), rs[0].ID)
	}
	for _, r := range rs {
		if r.Group != GroupProduction {
			t.Fatalf("%s has group %s", r.ID, r.Group)
		}
	}
}

// Spec 9.2a in miniature: with every read forbidden, every check ends as pass, warn, fail, unknown or n/a, and
// every unknown names a permission.
func TestEveryCheckSurvivesNoPermissions(t *testing.T) {
	e := fx{kubeDenied: true, ctrlDenied: true, helm: &fakeHelm{err: forbidden}, values: map[string]any{
		"evidence": map[string]any{"enabled": true}, "gateway": map[string]any{"tls": map[string]any{"enabled": true, "certificateSecret": "tls"}}},
		deep: true, token: "t", bundle: "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: a.b}\n"}.env(t)
	all := map[string]bool{GroupPre: true, GroupHealth: true, GroupProduction: true}
	for _, r := range Run(context.Background(), e, all) {
		switch r.Status {
		case Pass, Warn, Fail, NA:
		case Unknown:
			if !strings.HasPrefix(r.Message, "cannot ") && !strings.Contains(r.Message, "cannot ") {
				t.Errorf("%s unknown without a permission: %q", r.ID, r.Message)
			}
		default:
			t.Errorf("%s: status %q", r.ID, r.Status)
		}
	}
}

// Spec 3 principle 5: check changes no object. Every group runs, --deep included, against fakes that record each
// call; the only write allowed is creating a SelfSubjectAccessReview (R2), which asks a question and stores nothing.
func TestChecksChangeNoObject(t *testing.T) {
	e := fx{allowed: true, forward: ok200, deep: true, token: "t", values: map[string]any{}, helm: vals(map[string]any{
		"console":  map[string]any{"url": "https://console.example.com"},
		"evidence": map[string]any{"enabled": true}, "gateway": map[string]any{"tls": map[string]any{"enabled": true, "certificateSecret": "tls"}}}),
		bundle: "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: a.b}\n"}.env(t)
	var writes []string
	record := func(verb string) { writes = append(writes, verb) }
	e.Ctrl = interceptor.NewClient(e.Ctrl.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			record("create")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			record("update")
			return nil
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			record("patch")
			return nil
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			record("delete")
			return nil
		},
		DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
			record("deleteallof")
			return nil
		},
	})
	Run(context.Background(), e, map[string]bool{GroupPre: true, GroupHealth: true, GroupProduction: true})
	for _, a := range e.Kube.(*fake.Clientset).Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
		case "create":
			if a.GetResource().Resource != "selfsubjectaccessreviews" {
				record("create " + a.GetResource().Resource)
			}
		default:
			record(a.GetVerb() + " " + a.GetResource().Resource)
		}
	}
	if len(writes) > 0 {
		t.Fatalf("checks changed objects: %v", writes)
	}
}
