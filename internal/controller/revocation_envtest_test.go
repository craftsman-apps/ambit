package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func modelExpr(ctx context.Context, ns, name string) (string, error) {
	o, err := genGet(ctx, ns, "AgentgatewayModel", name)
	if err != nil {
		return "", err
	}
	s, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
	if len(s) == 0 {
		return "", fmt.Errorf("no expression")
	}
	return s[0], nil
}

func revokeFixture(t *testing.T, ns, mode string) (*Reconciler, chan error) {
	t.Helper()
	gateFixture(t, ns)
	ctx := t.Context()
	ops := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"ops"}, Models: []string{"gemma"}}}
	if err := envClient.Create(ctx, ops); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, AccessMode: mode,
		Activation: &Activation{Timeout: 20 * time.Second, Poll: 50 * time.Millisecond}}
	return r, reconcileAsync(r)
}

// #430: a rule that is deleted while a change waits for agentgateway is removed from the authorization at once: the
// waiting pass ends, and the next one writes the new expression first, without any verdict on the old one.
func TestRemovedRuleIsWrittenWithoutWaitingForTheChangeInProgress(t *testing.T) {
	needEnv(t)
	ns := "ambit-revoke-enforce"
	r, done := revokeFixture(t, ns, "enforce")
	ctx := t.Context()
	eventually(t, "authorization written", func() error {
		e, err := modelExpr(ctx, ns, "gemma")
		if err == nil && !(strings.Contains(e, `"eng"`) && strings.Contains(e, `"ops"`)) {
			return fmt.Errorf("expression %s", e)
		}
		return err
	})
	// The change waits for the verdict on the authorization (never played here). Remove the eng rule.
	start := time.Now()
	if err := envClient.Delete(ctx, &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	wait(t, done) // the waiting pass ended with a restart, not with the timeout of 20 s
	r2 := reconcileAsync(r)
	eventually(t, "the removal is written", func() error {
		e, err := modelExpr(ctx, ns, "gemma")
		if err == nil && strings.Contains(e, `"eng"`) {
			return fmt.Errorf("expression still has eng: %s", e)
		}
		return err
	})
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the removal was written after %v, want well under the settle times of the change in progress", took)
	}
	if d, _ := accessMode(ctx, ns); d["rulesVersion"] != "" {
		t.Errorf("published without a verdict: %v", d)
	}
	// It is a normal change from here: the access log waits for the verdict on the authorization.
	stays(t, "the access log still waits for the authorization verdict", func() error {
		if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
			return fmt.Errorf("access log written: %v", err)
		}
		return nil
	})
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, r2)
	if d, _ := accessMode(ctx, ns); len(d["rulesVersion"]) != 12 || d["pending"] != "" {
		t.Errorf("published %v", d)
	}
}

// A grant (a new rule) does not restart the change in progress: the order and the waits stay.
func TestAddedRuleDoesNotRestartTheChangeInProgress(t *testing.T) {
	needEnv(t)
	ns := "ambit-revoke-grant"
	_, done := revokeFixture(t, ns, "enforce")
	ctx := t.Context()
	eventually(t, "authorization written", func() error { _, err := modelExpr(ctx, ns, "gemma"); return err })
	if err := envClient.Create(ctx, &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: ns},
		Spec: v1.AccessRuleSpec{Groups: []string{"ml"}, Models: []string{"gemma"}}}); err != nil {
		t.Fatal(err)
	}
	stays(t, "the change keeps waiting at authorization, and nothing after it moves", func() error {
		select {
		case err := <-done:
			return fmt.Errorf("the pass ended: %v", err)
		default:
		}
		if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
			return fmt.Errorf("access log written before the verdict: %v", err)
		}
		if e, _ := modelExpr(ctx, ns, "gemma"); strings.Contains(e, `"ml"`) {
			return fmt.Errorf("the grant was written by the pass in progress: %s", e)
		}
		return nil
	})
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	eventually(t, "access log written after the verdict", func() error { _, err := accessLogOf(ctx, ns); return err })
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, done)
}

// Audit mode: authorization is relaxed to everyone, so a removal changes only the records; the change keeps its order
// (access log first) and is not restarted.
func TestRemovedRuleDoesNotRestartAnAuditChange(t *testing.T) {
	needEnv(t)
	ns := "ambit-revoke-audit"
	_, done := revokeFixture(t, ns, "audit")
	ctx := t.Context()
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	if err := envClient.Delete(ctx, &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	stays(t, "the audit change keeps waiting for the access log; authorization is not written", func() error {
		select {
		case err := <-done:
			return fmt.Errorf("the pass ended: %v", err)
		default:
		}
		if _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); !apierrors.IsNotFound(err) {
			return fmt.Errorf("authorization written before the access log was active: %v", err)
		}
		return nil
	})
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	select { // the status of the deleted rule can no longer be written: the pass ends with NotFound and is tried again
	case err := <-done:
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reconcile did not finish")
	}
	if d, _ := accessMode(ctx, ns); d["mode"] != "audit" || d["pending"] != "" {
		t.Errorf("published %v", d)
	}
}
