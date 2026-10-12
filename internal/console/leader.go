package console

import (
	"context"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RunLeader holds the key Lease until ctx ends and blocks meanwhile. onStart (KeyService.StartTerm) runs with the
// leading context when the Lease is won; onStop (KeyService.EndTerm) runs when it is lost. The Lease is not
// released on cancel: a successor waits for it to expire, so no work of this term can overlap the next one.
func RunLeader(ctx context.Context, cs kubernetes.Interface, ns, id string, onStart func(context.Context) error, onStop func()) {
	cfg := leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Name: "ambit-key-writer", Namespace: ns},
			Client: cs.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: id}},
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lead context.Context) {
				if err := onStart(lead); err != nil {
					slog.Warn("key term did not start", "err", err)
				}
			},
			OnStoppedLeading: func() {
				onStop()
				slog.Warn("key lease lost: key writes stop")
			},
			OnNewLeader: func(holder string) { slog.Info("key lease holder", "holder", holder) },
		},
	}
	for ctx.Err() == nil {
		leaderelection.RunOrDie(ctx, cfg)
	}
}
