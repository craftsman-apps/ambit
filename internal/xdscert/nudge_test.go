package xdscert

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func nudged(t *testing.T, c client.Client) string {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "ambit-system", Name: "kgateway-xds-cert"}, &s); err != nil {
		t.Fatal(err)
	}
	return s.Annotations[Annotation]
}

// agentgateway renews at 12 hours left; twice (2026-10-01, 2026-10-03) it did not, and a change to its
// Secret made it renew. Under 11 hours left the Nudger makes that change, at most once an hour.
func TestNudgeWhenNotRenewed(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ambit-system", Name: "kgateway-xds-cert"}}).Build()
	now := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	expires := now.Add(20 * time.Hour)
	n := &Nudger{Client: c, Namespace: "ambit-system", Secret: "kgateway-xds-cert", Addr: "xds:9978",
		Now:    func() time.Time { return now },
		Expiry: func(context.Context, string) (time.Time, error) { return expires, nil }}

	steps := []struct {
		at   time.Duration // after the start
		left time.Duration
		want string // annotation after the check; "" = never set
	}{
		{0, 20 * time.Hour, ""},                                   // renewed normally: nothing to do
		{0, 11*time.Hour + time.Minute, ""},                       // still above the threshold
		{0, 10 * time.Hour, "2026-10-05T06:00:00Z"},               // not renewed: nudge
		{30 * time.Minute, 9 * time.Hour, "2026-10-05T06:00:00Z"}, // within the hour: no second nudge
		{61 * time.Minute, 9 * time.Hour, "2026-10-05T07:01:00Z"}, // an hour later, still not renewed: again
		{2 * time.Hour, 23 * time.Hour, "2026-10-05T07:01:00Z"},   // renewed: nothing more
	}
	start := now
	for _, s := range steps {
		now = start.Add(s.at)
		expires = now.Add(s.left)
		if err := n.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := nudged(t, c); got != s.want {
			t.Fatalf("at +%v with %v left: annotation %q, want %q", s.at, s.left, got, s.want)
		}
	}
}

// When the certificate cannot be read, nothing is changed and the error is returned (and logged).
func TestNudgeReadFailure(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ambit-system", Name: "kgateway-xds-cert"}}).Build()
	n := &Nudger{Client: c, Namespace: "ambit-system", Secret: "kgateway-xds-cert", Addr: "xds:9978",
		Expiry: func(context.Context, string) (time.Time, error) { return time.Time{}, errors.New("connection refused") }}
	if err := n.Check(t.Context()); err == nil {
		t.Fatal("no error for an unreadable certificate")
	}
	if got := nudged(t, c); got != "" {
		t.Fatalf("annotation %q after a failed read", got)
	}
}

// A failed patch (Secret missing, RBAC) is returned, and the next check tries again instead of waiting an hour.
func TestNudgePatchFailureRetries(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	now := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	n := &Nudger{Client: c, Namespace: "ambit-system", Secret: "kgateway-xds-cert", Addr: "xds:9978",
		Now:    func() time.Time { return now },
		Expiry: func(context.Context, string) (time.Time, error) { return now.Add(time.Hour), nil }}
	if err := n.Check(t.Context()); err == nil {
		t.Fatal("no error when the Secret is missing")
	}
	_ = c.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ambit-system", Name: "kgateway-xds-cert"}})
	now = now.Add(10 * time.Minute)
	if err := n.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := nudged(t, c); got != "2026-10-05T06:10:00Z" {
		t.Fatalf("annotation %q, want the retry at 06:10", got)
	}
}
