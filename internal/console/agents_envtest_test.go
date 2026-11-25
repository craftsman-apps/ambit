package console

// Agents at their documented limits, against a real API server (envtest): 2,000 records, the largest records, and how
// long the lists take. Run with `make test-go`; plain `go test` skips it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestAgentsAtTheLimitsEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (run make test-go)")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ambit-system"}}); err != nil {
		t.Fatal(err)
	}
	s := &KeyService{Client: c, Namespace: "ambit-system", Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365},
		Services: ServiceSettings{Enabled: true, MaxDays: 90, ExposureDays: 7}, Agents: defaultAgents, RecordKey: testRecordKey, Now: time.Now}
	if err := s.StartTerm(ctx); err != nil {
		t.Fatal(err)
	}
	// 2,000 records, each with the largest scope (50 entries of 192 characters), 10 per person
	scope := make([]string, 50)
	for i := range scope {
		scope[i] = fmt.Sprintf("%s%02d/%s", strings.Repeat("s", 61), i, strings.Repeat("t", 128))
	}
	start := time.Now()
	for i := 0; i < s.Agents.MaxTotal; i++ {
		a := Agent{Name: fmt.Sprintf("agent-%04d", i), DisplayName: strings.Repeat("n", 100), Owner: fmt.Sprintf("user%03d@acme.com", i/10), Mode: ModePerson,
			Platform: "grok", Scope: scope, ConnectionConsent: []string{"slack"}, State: StateActive, Rev: 1, Created: time.Now(), CreatedBy: "x"}
		if err := c.Create(ctx, s.record(a)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	t.Logf("created %d records in %s", s.Agents.MaxTotal, time.Since(start).Round(time.Millisecond))

	timed := func(what string, f func() error) time.Duration {
		t.Helper()
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		d := time.Since(start)
		t.Logf("%s: %s", what, d.Round(time.Millisecond))
		return d
	}
	var all []Agent
	d := timed("list all 2,000 agents (admin list)", func() (err error) { all, err = s.AllAgents(ctx); return })
	if len(all) != 2000 {
		t.Fatalf("%d agents listed", len(all))
	}
	if d > 10*time.Second {
		t.Errorf("the admin list of 2,000 agents took %s", d)
	}
	var mine []Agent
	d = timed("list one person's agents (label selector)", func() (err error) { mine, err = s.AgentsOf(ctx, "user007@acme.com"); return })
	if len(mine) != 10 {
		t.Fatalf("%d agents of one person", len(mine))
	}
	if d > 3*time.Second {
		t.Errorf("one person's list took %s", d)
	}

	// At the limit: create is refused with the documented error, and nothing changes.
	person := Session{Email: "newcomer@acme.com", SignedIn: time.Now().Unix()}
	if _, err := s.CreateAgent(ctx, person, AgentInput{DisplayName: "one too many"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("agent 2,001: %v", err)
	}
	if after, _ := s.AllAgents(ctx); len(after) != 2000 {
		t.Errorf("%d agents after a refused create", len(after))
	}
	// A person with 10 agents cannot have an 11th, even below the total.
	if err := s.DeleteAgent(ctx, adminA, "agent-1999"); err != nil {
		t.Fatal(err)
	}
	owner := Session{Email: "user000@acme.com", SignedIn: time.Now().Unix()}
	if _, err := s.CreateAgent(ctx, owner, AgentInput{DisplayName: "eleventh"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("11th of one person: %v", err)
	}
	timed("create at 1,999 agents (lists all to count)", func() error {
		_, err := s.CreateAgent(ctx, person, AgentInput{DisplayName: "the 2,000th"})
		return err
	})
	if _, err := s.CreateAgent(ctx, person, AgentInput{DisplayName: "the 2,001st"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("refused again: %v", err)
	}
	// The repair reads every record and key once.
	timed("repair with 2,000 agents", func() error { _, err := s.Repair(ctx); return err })
}
