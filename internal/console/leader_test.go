package console

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached in 10 s")
}

func TestLeaderStartsTheTermAfterRepair(t *testing.T) {
	cs := fakeclientset.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &KeyService{Client: fake.NewClientBuilder().Build(), Namespace: "ambit-system", Now: time.Now,
		Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}}
	var writableInStart atomic.Bool
	started := make(chan struct{})
	go RunLeader(ctx, cs, "ambit-system", "pod-a", func(lead context.Context) error {
		close(started)
		err := s.StartTerm(lead)
		writableInStart.Store(s.Writable())
		return err
	}, s.EndTerm)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("never became leader")
	}
	waitFor(t, s.Writable)
	if !writableInStart.Load() {
		t.Fatal("term must be published when onStart returns")
	}
	lease, err := cs.CoordinationV1().Leases("ambit-system").Get(ctx, "ambit-key-writer", metav1.GetOptions{})
	if err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "pod-a" {
		t.Fatal(lease, err)
	}
}

// Cancelling an operation when the term ends is covered by TestEndingTheTermCancelsAnOperation (Task 6).
func TestLosingTheLeaseEndsTheTerm(t *testing.T) {
	cs := fakeclientset.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &KeyService{Client: fake.NewClientBuilder().Build(), Namespace: "ambit-system", Now: time.Now,
		Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}}
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLeader(ctx, cs, "ambit-system", "pod-a", s.StartTerm, func() { s.EndTerm(); stopped.Store(true) })
	}()
	waitFor(t, s.Writable)
	cancel()
	waitFor(t, stopped.Load)
	if s.Writable() {
		t.Fatal("still writable after the lease ended")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunLeader did not return after cancel")
	}
}
