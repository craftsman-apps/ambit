package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// SnapshotKind marks an access snapshot record in otel_logs (LogAttributes['ambit.kind']): the AccessRules in force,
// so an evidence export can answer "who may use what on date D" after the rules changed (spec #89, section 4).
const SnapshotKind = "access-snapshot"

// maxSnapshotBytes is the largest body sent; a larger one is dropped and counted (reason too_large).
const maxSnapshotBytes = 1 << 20

// dropTooLarge is the reason of a snapshot over maxSnapshotBytes, beside otlplog's queue_full and send_failed.
const dropTooLarge = "too_large"

// RecordDropped counts the controller's records not delivered to the collector, on the controller's /metrics.
var RecordDropped = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{Name: "ambit_controller_record_dropped_total",
	Help: "Records (access snapshots, MCP app removals) the controller did not deliver to the collector, by reason."}, []string{"reason"})

// Snapshots writes an access snapshot record when the rules in force change and once a day at 00:05 UTC. The
// reconciler calls Observe after each change is active; the manager runs Start (on the leader) for the daily record.
type Snapshots struct {
	Send func(*logspb.LogRecord) // otlplog.Exporter.Send: never blocks
	Now  func() time.Time        // nil: time.Now

	mu   sync.Mutex
	last *snapshot // what this process last recorded; nil before the first record
}

type snapshot struct {
	mode, rulesVersion, rulesHash string
	body                          string
}

// snapshotRule is one entry of the body.
type snapshotRule struct {
	Name string            `json:"name"`
	Spec v1.AccessRuleSpec `json:"spec"`
}

// snapshotBody is every AccessRule (name and spec) as a JSON array sorted by name, and its SHA-256. The hash changes
// on any spec change, also one the rules version does not see (argument limits are read by the guard, not compiled).
func snapshotBody(rules []v1.AccessRule) (body, hash string) {
	out := make([]snapshotRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, snapshotRule{r.Name, r.Spec})
	}
	slices.SortFunc(out, func(a, b snapshotRule) int { return strings.Compare(a.Name, b.Name) })
	b, _ := json.Marshal(out)
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:])
}

// Observe records the rules in force when the mode, the rules version or the rules hash differ from the last record
// of this process. The first call of a process always records: a start or a new leader writes one.
func (s *Snapshots) Observe(ctx context.Context, mode, rulesVersion string, rules []v1.AccessRule) {
	if s == nil {
		return
	}
	body, hash := snapshotBody(rules)
	next := &snapshot{mode, rulesVersion, hash, body}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil && s.last.mode == mode && s.last.rulesVersion == rulesVersion && s.last.rulesHash == hash {
		return
	}
	s.last = next
	s.send(ctx, next)
}

// Daily repeats the last record, so every date inside the retention has a snapshot even when nothing changes.
func (s *Snapshots) Daily(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil {
		s.send(ctx, s.last)
	}
}

func (s *Snapshots) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Snapshots) send(ctx context.Context, snap *snapshot) {
	if len(snap.body) > maxSnapshotBytes {
		RecordDropped.WithLabelValues(dropTooLarge).Inc()
		log.FromContext(ctx).Error(nil, "access snapshot not recorded: the AccessRules are larger than 1 MiB",
			"bytes", len(snap.body), "rulesVersion", snap.rulesVersion, "rulesHash", snap.rulesHash)
		return
	}
	// The mode is ambit.snapshot.accessMode, not the gateway's ambit.accessMode: the Audit page's audit-only summary
	// (and the one of 0.4.x after a rollback) counts every record with ambit.accessMode = 'audit'.
	s.Send(otlplog.Record(s.now(), snap.body,
		otlplog.Str("ambit.kind", SnapshotKind), otlplog.Str("ambit.rulesVersion", snap.rulesVersion),
		otlplog.Str("ambit.rulesHash", snap.rulesHash), otlplog.Str("ambit.snapshot.accessMode", snap.mode)))
}

// nextDaily is the next 00:05 UTC after t.
func nextDaily(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 5, 0, 0, time.UTC)
	if !d.After(t) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// Start writes the daily record at 00:05 UTC until ctx ends. It runs on the leader only (the manager's default),
// the same process whose reconciler calls Observe.
func (s *Snapshots) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(nextDaily(s.now()))):
			s.Daily(ctx)
		}
	}
}
