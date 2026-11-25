package controller

import (
	"context"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/apprecord"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const echoImage = "ghcr.io/acme/echo@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A removed Server is one app-approval record with the fields of the other app records.
func TestRemovedServerIsRecorded(t *testing.T) {
	c := &captured{}
	at := time.Date(2026, 12, 1, 8, 30, 0, 0, time.UTC)
	a := &AppRemovals{Send: c.send, Now: func() time.Time { return at }}
	srv := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "everything", Namespace: "ambit-system", Annotations: map[string]string{
		apprecord.AnnRequestedBy: "user01@example.com", apprecord.AnnApprovedBy: "user11@example.com", apprecord.AnnRequest: "everything-x1y2",
		apprecord.AnnRemovedBy: "user10@example.com", apprecord.AnnRemovedByRole: "admin"}},
		Spec: v1.ServerSpec{Image: echoImage}}
	a.Removed(srv)
	if c.n() != 1 {
		t.Fatalf("%d records, want 1", c.n())
	}
	got := attrsOf(c.recs[0])
	for k, want := range map[string]string{"ambit.kind": "app-approval", "ambit.app.action": "removed", "ambit.actor": "user10@example.com",
		"ambit.actor.role": "admin", "mcp.target": "everything", "ambit.app.request": "everything-x1y2",
		"ambit.app.requested_by": "user01@example.com", "ambit.app.image": echoImage} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["jwt.email"]; ok {
		t.Error("an app record must not set jwt.email: the Audit calls search would count it as a call")
	}
	if time.Unix(0, int64(c.recs[0].TimeUnixNano)).UTC() != at || c.recs[0].SeverityText != "INFO" {
		t.Errorf("time %v severity %s", time.Unix(0, int64(c.recs[0].TimeUnixNano)).UTC(), c.recs[0].SeverityText)
	}
}

// A Server removed outside the console (kubectl, or Argo CD pruning it in GitOps mode) has no actor; a URL server has no image.
func TestRemovedServerOutsideTheConsoleHasNoActor(t *testing.T) {
	c := &captured{}
	(&AppRemovals{Send: c.send}).Removed(&v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "jira", Namespace: "ambit-system"},
		Spec: v1.ServerSpec{URL: "https://mcp.example.com/v1/mcp"}})
	got := attrsOf(c.recs[0])
	if got["ambit.app.action"] != "removed" || got["ambit.actor"] != "" || got["ambit.actor.role"] != "" || got["ambit.app.url"] != "https://mcp.example.com/v1/mcp" {
		t.Errorf("record %v", got)
	}
	if _, ok := got["ambit.app.image"]; ok {
		t.Error("a URL server has no image attribute")
	}
	var off *AppRemovals
	off.Removed(&v1.Server{}) // off (no evidence store): no panic
}

// The watch handler records only a Server deleted in the controller's namespace, and still queues the reconcile.
func TestServerDeleteEventRecordsOnce(t *testing.T) {
	c := &captured{}
	r := &Reconciler{Namespace: "ambit-system", AppRemovals: &AppRemovals{Send: c.send}}
	queued := 0
	next := handler.Funcs{DeleteFunc: func(_ context.Context, _ event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		queued++
	}}
	h := r.recordServerRemoval(next)
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	h.Delete(t.Context(), event.DeleteEvent{Object: &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ambit-system"}}}, q)
	h.Delete(t.Context(), event.DeleteEvent{Object: &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "other"}}}, q)
	h.Delete(t.Context(), event.DeleteEvent{Object: &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ambit-system"}}}, q)
	if c.n() != 1 || attrsOf(c.recs[0])["mcp.target"] != "a" {
		t.Errorf("%d records, want 1 (server a)", c.n())
	}
	if queued != 3 {
		t.Errorf("reconcile queued %d times, want 3", queued)
	}
}
