package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus/testutil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type captured struct {
	mu   sync.Mutex
	recs []*logspb.LogRecord
}

func (c *captured) send(r *logspb.LogRecord) { c.mu.Lock(); c.recs = append(c.recs, r); c.mu.Unlock() }

func (c *captured) n() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.recs) }

func attrsOf(r *logspb.LogRecord) map[string]string {
	m := map[string]string{}
	for _, kv := range r.Attributes {
		m[kv.Key] = kv.Value.GetStringValue()
	}
	return m
}

func snapRule(name string, groups []string, models ...string) v1.AccessRule {
	return v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.AccessRuleSpec{Groups: groups, Models: models}}
}

func TestSnapshotRecordsEachChangeOnce(t *testing.T) {
	ctx := t.Context()
	c := &captured{}
	at := time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC)
	s := &Snapshots{Send: c.send, Now: func() time.Time { return at }}
	rules := []v1.AccessRule{snapRule("z-ops", []string{"ops"}, "gpt-5"), snapRule("eng-models", []string{"=eng,\"x\"\n"}, "gemma")}

	s.Observe(ctx, "enforce", "3f9a1c0e2b7d", rules) // a start: always one
	if c.n() != 1 {
		t.Fatalf("start: %d records, want 1", c.n())
	}
	r := c.recs[0]
	a := attrsOf(r)
	if a["ambit.kind"] != SnapshotKind || a["ambit.rulesVersion"] != "3f9a1c0e2b7d" || a["ambit.snapshot.accessMode"] != "enforce" {
		t.Errorf("attributes %v", a)
	}
	body := r.Body.GetStringValue()
	sum := sha256.Sum256([]byte(body))
	if a["ambit.rulesHash"] != hex.EncodeToString(sum[:]) {
		t.Errorf("rulesHash %q is not the SHA-256 of the body", a["ambit.rulesHash"])
	}
	var got []snapshotRule
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got) != 2 || got[0].Name != "eng-models" || got[1].Name != "z-ops" ||
		got[0].Spec.Groups[0] != "=eng,\"x\"\n" {
		t.Errorf("body not every rule sorted by name: %s (%v)", body, err)
	}
	if time.Unix(0, int64(r.TimeUnixNano)).UTC() != at {
		t.Errorf("time %v", time.Unix(0, int64(r.TimeUnixNano)).UTC())
	}
	// No attribute that a gateway query matches: person (jwt.email, jwt.sub), invalid key (ambit.identity), Usage and
	// Savings (protocol), the audit-only summary (ambit.accessMode).
	for _, k := range []string{"jwt.email", "jwt.sub", "ambit.identity", "protocol", "ambit.accessMode", "ambit.key_id"} {
		if _, ok := a[k]; ok {
			t.Errorf("snapshot has %s", k)
		}
	}

	s.Observe(ctx, "enforce", "3f9a1c0e2b7d", rules) // no change
	if c.n() != 1 {
		t.Errorf("no change: %d records, want 1", c.n())
	}
	s.Observe(ctx, "enforce", "000000000001", rules) // a new rules version
	if c.n() != 2 {
		t.Errorf("new rules version: %d records, want 2", c.n())
	}
	s.Observe(ctx, "audit", "000000000001", rules) // the access mode only
	if c.n() != 3 || attrsOf(c.recs[2])["ambit.snapshot.accessMode"] != "audit" {
		t.Errorf("mode change: %d records", c.n())
	}
	// An argument limit only: the rules version stays (the guard reads arguments, the gateway does not); the hash moves.
	limited := []v1.AccessRule{rules[0], rules[1]}
	limited[0].Spec.Servers = []v1.ServerAccess{{Name: "github", Arguments: []v1.ArgumentRule{{Name: "per_page", Max: "1"}}}}
	s.Observe(ctx, "audit", "000000000001", limited)
	if c.n() != 4 || attrsOf(c.recs[3])["ambit.rulesHash"] == attrsOf(c.recs[2])["ambit.rulesHash"] {
		t.Errorf("argument limit change: %d records", c.n())
	}
	// Order of the list does not matter.
	s.Observe(ctx, "audit", "000000000001", []v1.AccessRule{limited[1], limited[0]})
	if c.n() != 4 {
		t.Errorf("same rules in another order: %d records, want 4", c.n())
	}

	// The daily record repeats the state.
	at = at.Add(14 * time.Hour)
	s.Daily(ctx)
	if c.n() != 5 || c.recs[4].Body.GetStringValue() != c.recs[3].Body.GetStringValue() || attrsOf(c.recs[4])["ambit.rulesHash"] != attrsOf(c.recs[3])["ambit.rulesHash"] {
		t.Errorf("daily: %d records", c.n())
	}

	// A restart (a new process, a new leader) writes one at start, also without a change.
	s2 := &Snapshots{Send: c.send}
	s2.Daily(ctx) // nothing observed yet: nothing to repeat
	s2.Observe(ctx, "audit", "000000000001", limited)
	if c.n() != 6 {
		t.Errorf("restart: %d records, want 6", c.n())
	}
}

func TestSnapshotOverOneMiBIsDroppedAndCounted(t *testing.T) {
	c := &captured{}
	s := &Snapshots{Send: c.send}
	before := testutil.ToFloat64(RecordDropped.WithLabelValues(dropTooLarge))
	big := snapRule("big", nil, "gemma")
	big.Spec.Users = []string{strings.Repeat("a", maxSnapshotBytes) + "@example.com"}
	s.Observe(t.Context(), "enforce", "v", []v1.AccessRule{big})
	if c.n() != 0 || testutil.ToFloat64(RecordDropped.WithLabelValues(dropTooLarge))-before != 1 {
		t.Errorf("records %d, too_large %v", c.n(), testutil.ToFloat64(RecordDropped.WithLabelValues(dropTooLarge))-before)
	}
	s.Observe(t.Context(), "enforce", "v", []v1.AccessRule{big}) // the same state is not tried again on every pass
	if testutil.ToFloat64(RecordDropped.WithLabelValues(dropTooLarge))-before != 1 {
		t.Error("the same too-large state was counted again")
	}
}

// A collector that is down costs the record, counted once: one send, no retries.
func TestSnapshotCollectorDownIsCountedOnce(t *testing.T) {
	exp, err := otlplog.New("127.0.0.1:1", "ambit-controller", RecordDropped)
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(RecordDropped.WithLabelValues(otlplog.DropSendFailed))
	s := &Snapshots{Send: exp.Send}
	s.Observe(t.Context(), "enforce", "v", []v1.AccessRule{snapRule("eng", []string{"eng"}, "gemma")})
	exp.Close() // flushes: the one failed export has happened
	if d := testutil.ToFloat64(RecordDropped.WithLabelValues(otlplog.DropSendFailed)) - before; d != 1 {
		t.Errorf("send_failed %v, want 1", d)
	}
}

func TestNextDaily(t *testing.T) {
	for _, c := range []struct{ now, want string }{
		{"2026-12-31T00:00:00Z", "2026-12-31T00:05:00Z"},
		{"2026-12-31T00:05:00Z", "2027-01-01T00:05:00Z"},
		{"2026-12-31T23:59:59Z", "2027-01-01T00:05:00Z"},
		{"2026-12-31T09:00:00+09:00", "2026-12-31T00:05:00Z"}, // 00:00 UTC: the day is the UTC day
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		if got := nextDaily(now).Format(time.RFC3339); got != c.want {
			t.Errorf("nextDaily(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}
