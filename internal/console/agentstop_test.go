package console

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// playController writes the acknowledgment the controller would: the list generation each Gateway has accepted.
func playController(t *testing.T, s *KeyService, ackedGen map[string]int64, degraded string) {
	t.Helper()
	l, _, err := s.stopList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := controller.StopAck{Gen: l.Gen, Gateways: map[string]controller.GatewayAck{}, Degraded: degraded}
	for g, n := range ackedGen {
		a.Gateways[g] = controller.GatewayAck{AckedGen: n, At: s.now()}
	}
	b, _ := json.Marshal(a)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controller.StopsAckConfigMap, Namespace: s.Namespace}, Data: map[string]string{"ack.json": string(b)}}
	if err := s.Client.Create(context.Background(), cm); apierrors.IsAlreadyExists(err) {
		var cur corev1.ConfigMap
		_ = s.Client.Get(context.Background(), client.ObjectKeyFromObject(cm), &cur)
		cur.Data = cm.Data
		err = s.Client.Update(context.Background(), &cur)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func listOf(t *testing.T, s *KeyService) controller.StopList {
	t.Helper()
	l, _, err := s.stopList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestStopStateMachine(t *testing.T) {
	s, clk := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot", Scope: []string{"crm"}})
	_, k, _ := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
	other := Session{Email: "user02@acme.com", SignedIn: t0.Unix()}
	if _, err := s.StopAgent(ctx, other, a.Name); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("another person stops: %v", err)
	}
	// S1 and S2: on the list, record stopping, key deleted; not stopped until the controller acknowledged
	b, err := s.StopAgent(ctx, user01(), a.Name)
	if err != nil || b.State != StateStopping || b.StopRequestedAt == nil {
		t.Fatalf("%+v %v", b, err)
	}
	l := listOf(t, s)
	if l.Gen != 1 || len(l.Entries) != 1 || l.Entries[0].Name != a.Name || l.Entries[0].Gen != 1 || l.Entries[0].KeysDeletedAt == nil {
		t.Fatalf("list %+v", l)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Errorf("the key is still there: %v", err)
	}
	if _, _, err := s.CreateAgentKey(ctx, user01(), a.Name, "late", 30, nil); !errors.Is(err, ErrAgentNotActive) {
		t.Errorf("a key for a stopping agent: %v", err)
	}
	// repeating changes nothing (same entry, same generation)
	if _, err := s.StopAgent(ctx, user01(), a.Name); err != nil {
		t.Fatal(err)
	}
	if l := listOf(t, s); l.Gen != 1 || len(l.Entries) != 1 {
		t.Fatalf("a repeated stop changed the list: %+v", l)
	}
	// no answer from the controller: stopping. A partial rollout: stopping. A list that could not be rendered: stopping.
	for name, c := range map[string]struct {
		acked    map[string]int64
		degraded string
	}{"no ConfigMap": {nil, ""}, "one Gateway behind": {map[string]int64{"ambit": 1, "ambit-public": 0}, ""},
		"degraded": {map[string]int64{"ambit": 1, "ambit-public": 1}, "too many entries"}, "old generation": {map[string]int64{"ambit": 0}, ""}} {
		if name != "no ConfigMap" {
			playController(t, s, c.acked, c.degraded)
		}
		if _, err := s.AdvanceStops(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetAgent(ctx, a.Name); got.State != StateStopping {
			t.Fatalf("%s: %s", name, got.State)
		}
	}
	// 60 s without confirmation: reported once
	clk.Set(clk.Now().Add(61 * time.Second))
	for i := 0; i < 2; i++ {
		if _, err := s.AdvanceStops(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetAgent(ctx, a.Name)
	if got.StopNotConfirmedAt == nil {
		t.Fatal("no report that the stop is not confirmed")
	}
	nc := 0
	evs, _ := keyChanges(s)
	for _, e := range evs {
		if e.Action == "agent-stop-not-confirmed" {
			nc++
		}
	}
	if nc != 1 {
		t.Errorf("%d not-confirmed events", nc)
	}
	// every Gateway acknowledged: S6
	playController(t, s, map[string]int64{"ambit": 1, "ambit-public": 1}, "")
	if more, err := s.AdvanceStops(ctx); err != nil || !more {
		t.Fatalf("advance: %v %v", more, err)
	}
	got, _ = s.GetAgent(ctx, a.Name)
	if got.State != StateStopped || got.StoppedAt == nil {
		t.Fatalf("%+v", got)
	}
	// retention: the entry stays for 25 minutes after the credentials are gone, then it is pruned; the agent stays stopped
	clk.Set(clk.Now().Add(20 * time.Minute))
	_, _ = s.AdvanceStops(ctx)
	if len(listOf(t, s).Entries) != 1 {
		t.Fatal("the entry went before its retention")
	}
	clk.Set(clk.Now().Add(10 * time.Minute))
	if more, _ := s.AdvanceStops(ctx); more {
		t.Error("no work should remain")
	}
	if l := listOf(t, s); len(l.Entries) != 0 || l.Gen != 2 {
		t.Fatalf("after the retention: %+v", l)
	}
	if got, _ = s.GetAgent(ctx, a.Name); got.State != StateStopped {
		t.Errorf("state %s", got.State)
	}
}

func TestStartAgent(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot"})
	if _, err := s.StartAgent(ctx, user01(), a.Name); err != nil {
		t.Errorf("starting an active agent: %v", err)
	}
	_, _ = s.StopAgent(ctx, user01(), a.Name)
	if _, err := s.StartAgent(ctx, user01(), a.Name); !errors.Is(err, ErrNotStopped) {
		t.Errorf("starting a stopping agent: %v", err)
	}
	playController(t, s, map[string]int64{"ambit": 1}, "")
	_, _ = s.AdvanceStops(ctx)
	if _, err := s.StartAgent(ctx, Session{Email: "user02@acme.com", SignedIn: t0.Unix()}, a.Name); !errors.Is(err, ErrNoAgent) {
		t.Errorf("another person starts: %v", err)
	}
	b, err := s.StartAgent(ctx, user01(), a.Name)
	if err != nil || b.State != StateActive || b.StoppedAt != nil || b.StopRequestedAt != nil {
		t.Fatalf("%+v %v", b, err)
	}
	if l := listOf(t, s); len(l.Entries) != 0 || l.Gen != 2 {
		t.Errorf("the entry stays: %+v", l)
	}
	if _, _, err := s.CreateAgentKey(ctx, user01(), a.Name, "new", 30, nil); err != nil {
		t.Errorf("a new key after the start: %v", err)
	}
	// an offboarded owner's agent is not started
	c := mustAgent(t, s, Session{Email: "gone@acme.com", SignedIn: t0.Unix()}, AgentInput{DisplayName: "c"})
	_, _ = s.StopAgent(ctx, Session{Email: "gone@acme.com", SignedIn: t0.Unix()}, c.Name)
	playController(t, s, map[string]int64{"ambit": 3}, "")
	_, _ = s.AdvanceStops(ctx)
	cm := blockList(`["gone@acme.com"]`)
	if err := s.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAgent(ctx, adminA, c.Name); !errors.Is(err, ErrOffboarded) {
		t.Errorf("start of an offboarded owner's agent: %v", err)
	}
}

// Stopping works with agents switched off, and an admin stops anyone's agent.
func TestStopNeedsNoSetting(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot"})
	n := mustAgent(t, s, adminA, AgentInput{DisplayName: "n", Mode: ModeItself, Name: "nightly", Group: "ops"})
	_, k, _ := s.CreateAgentKey(ctx, adminA, "nightly", "k", 30, nil)
	s.Agents.Enabled = false
	if _, err := s.StopAgent(ctx, adminA, a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopAgent(ctx, user01(), n.Name); !errors.Is(err, ErrNoAgent) { // an owner of an itself-mode agent is not its manager
		t.Errorf("a person stops an itself-mode agent: %v", err)
	}
	if _, err := s.StopAgent(ctx, adminA, n.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Errorf("the account key survived: %v", err)
	}
}

// The list is full: the credentials are deleted, the agent is marked unlisted and is not confirmed until there is room.
func TestStopWithAFullList(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	l := controller.StopList{Gen: 7}
	for i := 0; i < controller.MaxStopEntries; i++ {
		l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: fmt.Sprintf("old-%04d", i), Gen: 7, RequestedAt: t0})
	}
	b, _ := json.Marshal(l)
	if err := s.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controller.StopsConfigMap, Namespace: s.Namespace, Labels: map[string]string{agentStopsLabel: "true"}},
		Data: map[string]string{"list.json": string(b)}}); err != nil {
		t.Fatal(err)
	}
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot"})
	_, k, _ := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
	got, err := s.StopAgent(ctx, user01(), a.Name)
	if err != nil || !got.StopUnlisted || got.State != StateStopping {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Errorf("the key survived the stop of an unlisted agent: %v", err)
	}
	if got := listOf(t, s); len(got.Entries) != 1000 || got.Gen != 7 {
		t.Errorf("the list changed: %d entries, gen %d", len(got.Entries), got.Gen)
	}
	playController(t, s, map[string]int64{"ambit": 7}, "")
	_, _ = s.AdvanceStops(ctx)
	if got, _ = s.GetAgent(ctx, a.Name); got.State != StateStopping || !got.StopUnlisted {
		t.Fatalf("an unlisted agent was confirmed: %+v", got)
	}
	// room appears (a stopped agent's entry is pruned): the agent is listed, then confirmed
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) { l.Entries = l.Entries[:999]; return true, false }); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AdvanceStops(ctx)
	if got, _ = s.GetAgent(ctx, a.Name); got.StopUnlisted || entryIndex(listOf(t, s), a.Name) < 0 {
		t.Fatalf("not listed after room appeared: %+v", got)
	}
	playController(t, s, map[string]int64{"ambit": 9}, "")
	_, _ = s.AdvanceStops(ctx)
	if got, _ = s.GetAgent(ctx, a.Name); got.State != StateStopped {
		t.Errorf("%+v", got)
	}
}

// A key that cannot be deleted leaves the agent stopping; the loop finishes it.
func TestStopRetriesKeyDeletion(t *testing.T) {
	fail := true
	s, _ := newAgentSvcWith(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if sec, ok := obj.(*corev1.Secret); ok && fail && sec.Labels[agentLabel] != "" {
			return errors.New("unavailable")
		}
		return c.Delete(ctx, obj, opts...)
	}})
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot"})
	_, k, _ := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
	got, err := s.StopAgent(ctx, user01(), a.Name)
	if !errors.Is(err, ErrDeleteIncomplete) || got.State != StateStopping {
		t.Fatalf("%+v %v", got, err)
	}
	playController(t, s, map[string]int64{"ambit": 1}, "")
	if _, err := s.AdvanceStops(ctx); err == nil {
		t.Error("the loop did not report the failure")
	}
	if got, _ = s.GetAgent(ctx, a.Name); got.State != StateStopping {
		t.Fatalf("stopped while a key lives: %s", got.State)
	}
	fail = false
	_, _ = s.AdvanceStops(ctx)
	if got, _ = s.GetAgent(ctx, a.Name); got.State != StateStopped {
		t.Fatalf("%+v", got)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Error("the key lives")
	}
}

func TestOffboardingAgents(t *testing.T) {
	s, clk := newAgentSvc(t)
	ctx := context.Background()
	mine := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "mine"})
	_, k, _ := s.CreateAgentKey(ctx, user01(), mine.Name, "k", 30, nil)
	mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "mine two"})
	n := mustAgent(t, s, adminA, AgentInput{DisplayName: "n", Mode: ModeItself, Name: "nightly", Group: "ops", Owner: "user01@acme.com"})
	g := mustAgent(t, s, adminA, AgentInput{DisplayName: "g", Mode: ModeItself, Name: "grouped", Group: "ops", Owner: "group:platform"})
	other := mustAgent(t, s, Session{Email: "user02@acme.com", SignedIn: t0.Unix()}, AgentInput{DisplayName: "theirs"})
	stopped, orphaned, err := s.OffboardAgents(ctx, "User01@Acme.com", "scim")
	if err != nil || stopped != 2 || orphaned != 1 {
		t.Fatalf("%d %d %v", stopped, orphaned, err)
	}
	if got, _ := s.GetAgent(ctx, mine.Name); got.State != StateStopping {
		t.Errorf("%+v", got)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Error("the key lives")
	}
	for name, want := range map[string]string{other.Name: StateActive, g.Name: StateActive, n.Name: StateActive} {
		if got, _ := s.GetAgent(ctx, name); got.State != want {
			t.Errorf("%s: %s", name, got.State)
		}
	}
	if got, _ := s.GetAgent(ctx, n.Name); got.OrphanedAt == nil {
		t.Error("the itself-mode agent of the leaver is not marked orphaned")
	}
	// repeated: nothing changes
	if st, or, _ := s.OffboardAgents(ctx, "user01@acme.com", "scim"); or != 0 || st > 2 {
		t.Errorf("repeat: %d %d", st, or)
	}
	// it keeps working for 7 days; then it is stopped
	clk.Set(clk.Now().Add(6 * 24 * time.Hour))
	_, _ = s.AdvanceStops(ctx)
	if got, _ := s.GetAgent(ctx, n.Name); got.State != StateActive {
		t.Fatalf("stopped after 6 days: %s", got.State)
	}
	clk.Set(clk.Now().Add(25 * time.Hour))
	_, _ = s.AdvanceStops(ctx)
	if got, _ := s.GetAgent(ctx, n.Name); got.State != StateStopping {
		t.Fatalf("not stopped after 7 days: %s", got.State)
	}
	// an admin who moves it in time keeps it
	m := mustAgent(t, s, adminA, AgentInput{DisplayName: "m", Mode: ModeItself, Name: "moved", Group: "ops", Owner: "user01@acme.com"})
	_, _, _ = s.OffboardAgents(ctx, "user01@acme.com", "scim again")
	if got, _ := s.GetAgent(ctx, m.Name); got.OrphanedAt == nil {
		t.Fatal("not orphaned")
	}
	if got, err := s.MoveAgent(ctx, adminA, m.Name, "new@acme.com"); err != nil || got.OrphanedAt != nil {
		t.Fatalf("%+v %v", got, err)
	}
}

// The agent-stop export: timeline, the calls after the request with their times, and the measured counts.
func TestAgentStopEvidence(t *testing.T) {
	ch := goodCH(0)
	e := newEvidenceFixture(t, ch)
	k := e.cat.Keys
	k.Agents, k.RecordKey, k.Settings = defaultAgents, testRecordKey, KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}
	ctx := context.Background()
	if err := k.Client.Create(ctx, scopeReady()); err != nil {
		t.Fatal(err)
	}
	if err := k.StartTerm(ctx); err != nil {
		t.Fatal(err)
	}
	a := mustAgent(t, k, user01("eng"), AgentInput{DisplayName: "bot"})
	if _, err := k.StopAgent(ctx, user01(), a.Name); err != nil {
		t.Fatal(err)
	}
	if resp, body, _ := e.get(t, ctx, "/api/evidence/agent-stop/nope.zip", e.admin); resp.StatusCode != 404 {
		t.Fatalf("unknown agent: %d %s", resp.StatusCode, body)
	}
	playController(t, k, map[string]int64{"ambit": 1, "ambit-public": 1}, "")
	_, _ = k.AdvanceStops(ctx)
	r0 := *k.mustAgent(t, a.Name).StopRequestedAt
	ch.agentCalls = [][]driver.Value{
		{r0.Add(120 * time.Millisecond), "private", "key", "k1", "tools/call", "200"},
		{r0.Add(400 * time.Millisecond), "private", "key", "k1", "tools/call", "200"},
		{r0.Add(900 * time.Millisecond), "private", "key", "k1", "tools/call", "403"},
	}
	resp, body, err := e.get(t, ctx, "/api/evidence/agent-stop/"+a.Name+".zip", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	files := readZip(t, body)
	sum, _ := csv.NewReader(bytes.NewReader(files["summary.csv"])).ReadAll()
	got := map[string]string{}
	for _, r := range sum[1:] {
		got[r[0]+"/"+r[1]] = r[2]
	}
	if got["accepted_calls_after_request/private"] != "2" || got["first_refused_call_seconds_from_request/private"] != "0.900" ||
		got["accepted_calls_after_request/public"] != "0" || got["last_accepted_call_seconds_from_request/"] != "0.400" {
		t.Errorf("summary %v", got)
	}
	tl, _ := csv.NewReader(bytes.NewReader(files["timeline.csv"])).ReadAll()
	steps := ""
	for _, r := range tl[1:] {
		steps += r[0] + ":" + r[3] + " "
	}
	for _, want := range []string{"S1: ", "S2: ", "S5:ambit ", "S5:ambit-public ", "S6: "} {
		if !strings.Contains(steps, want) {
			t.Errorf("timeline lacks %q: %s", want, steps)
		}
	}
	var m manifest
	_ = json.Unmarshal(files["manifest.json"], &m)
	if m.Export != "agent-stop" || !strings.Contains(strings.Join(m.Notes, " "), "does not promise") {
		t.Errorf("manifest %+v", m)
	}
}

func (s *KeyService) mustAgent(t *testing.T, name string) Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
