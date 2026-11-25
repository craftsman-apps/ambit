package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var defaultAgents = AgentSettings{Enabled: true, MaxTotal: 2000, MaxPerPerson: 10, KeyMaxDays: 90}

func newAgentSvc(t *testing.T, objs ...client.Object) (*KeyService, *syncClock) {
	return newAgentSvcWith(t, interceptor.Funcs{}, objs...)
}

func newAgentSvcWith(t *testing.T, f interceptor.Funcs, objs ...client.Object) (*KeyService, *syncClock) {
	t.Helper()
	s, clk := newKeySvcWith(t, f, objs...)
	s.Agents = defaultAgents
	return s, clk
}

func mustAgent(t *testing.T, s *KeyService, by Session, in AgentInput) Agent {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), by, in)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func secretOf(t *testing.T, s *KeyService, id string) corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	if err := s.Client.Get(context.Background(), client.ObjectKey{Namespace: s.Namespace, Name: "ambit-key-" + id}, &sec); err != nil {
		t.Fatal(err)
	}
	return sec
}

// metadataOf is the key's metadata exactly as the gateway reads it (apiKey.<field>).
func metadataOf(t *testing.T, s *KeyService, id string) map[string]any {
	t.Helper()
	var e struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(secretOf(t, s, id).Data["key"], &e); err != nil {
		t.Fatal(err)
	}
	return e.Metadata
}

func TestPersonAgentCreateAndKey(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "Grok Bot for sales", Platform: "grok", Scope: []string{"crm/read", "wiki", "wiki"}})
	if a.Mode != ModePerson || a.Owner != "user01@acme.com" || a.State != StateActive || a.Rev != 1 || !strings.HasPrefix(a.Name, "grok-bot-for-sales-") ||
		!accountNamePattern.MatchString(a.Name) || strings.Join(a.Scope, ",") != "crm/read,wiki" || a.Platform != "grok" || a.CreatedBy != "user01@acme.com" {
		t.Fatalf("agent %+v", a)
	}
	plain, k, err := s.CreateAgentKey(ctx, user01("eng"), a.Name, "grok connector", 30, []string{"203.0.113.77/24", "2001:db8::/32"})
	if err != nil || !strings.HasPrefix(plain, "ambit_") {
		t.Fatal(plain, err)
	}
	if k.Scope != ScopeMCP || k.Email != "user01@acme.com" || k.Agent == nil || k.Agent.Mode != ModePerson {
		t.Fatalf("key %+v", k)
	}
	// What the gateway reads: apiKey.agent, apiKey.agentMode, ... (docs/agents.md "Key fields")
	m := metadataOf(t, s, k.ID)
	want := map[string]any{"agent": a.Name, "agentMode": "person", "agentOwnerAtIssue": "user01@acme.com", "platformDeclared": "grok", "agentRev": float64(1),
		"email": "user01@acme.com", "keyId": k.ID}
	for f, v := range want {
		if m[f] != v {
			t.Errorf("metadata %s = %v, want %v", f, m[f], v)
		}
	}
	if fmt.Sprint(m["agentScope"]) != "[crm/read wiki]" || fmt.Sprint(m["sourceRanges"]) != "[2001:db8::/32 203.0.113.0/24]" {
		t.Errorf("scope %v ranges %v", m["agentScope"], m["sourceRanges"])
	}
	sec := secretOf(t, s, k.ID)
	if sec.Labels[agentLabel] != a.Name || sec.Labels[keyMCPLabel] != "true" || sec.Labels[keyLLMLabel] != "" {
		t.Errorf("labels %v: an agent key is an MCP key labelled with its agent", sec.Labels)
	}
	// It is the person's key too: counted in their 20, listed in their keys, and refused after offboarding.
	if keys, _ := s.List(ctx, "user01@acme.com"); len(keys) != 1 || keys[0].Agent == nil || keys[0].Agent.Name != a.Name {
		t.Errorf("listed %+v", keys)
	}
	if n, err := s.OffboardKeys(ctx, "user01@acme.com", "test"); err != nil || n != 1 {
		t.Fatalf("offboard: %d %v", n, err)
	}
}

func TestAgentCreateRefusals(t *testing.T) {
	s, _ := newAgentSvc(t, blockList(`["blocked@example.com"]`))
	ctx := context.Background()
	long := strings.Repeat("a", 201)
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("srv%d", i)
	}
	for name, c := range map[string]struct {
		by   Session
		in   AgentInput
		want error
	}{
		"no name":                {user01(), AgentInput{}, ErrBadInput},
		"name too long":          {user01(), AgentInput{DisplayName: strings.Repeat("n", 101)}, ErrBadInput},
		"control character":      {user01(), AgentInput{DisplayName: "a\x00b"}, ErrBadInput},
		"bad platform":           {user01(), AgentInput{DisplayName: "a", Platform: "Grok Bot!"}, ErrBadInput},
		"scope without a server": {user01(), AgentInput{DisplayName: "a", Scope: []string{"/tool"}}, ErrBadInput},
		"scope with spaces":      {user01(), AgentInput{DisplayName: "a", Scope: []string{"crm/read all"}}, ErrBadInput},
		"scope entry too long":   {user01(), AgentInput{DisplayName: "a", Scope: []string{"crm/" + long}}, ErrBadInput},
		"51 scope entries":       {user01(), AgentInput{DisplayName: "a", Scope: tooMany}, ErrBadInput},
		"bad connection":         {user01(), AgentInput{DisplayName: "a", Consent: []string{"Slack!"}}, ErrBadInput},
		"unknown mode":           {user01(), AgentInput{DisplayName: "a", Mode: "team"}, ErrBadInput},
		"not signed in again":    {Session{Email: "user01@acme.com"}, AgentInput{DisplayName: "a"}, ErrSignInAgain},
		"service domain":         {Session{Email: "ci@svc.ambit.internal", SignedIn: t0.Unix()}, AgentInput{DisplayName: "a"}, ErrBadInput},
		"offboarded":             {Session{Email: "blocked@example.com", SignedIn: t0.Unix()}, AgentInput{DisplayName: "a"}, ErrOffboarded},
		"a person makes itself":  {user01(), AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "eng"}, ErrNotAllowed},
		"itself without a group": {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci"}, ErrBadInput},
		"itself, bad name":       {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "Bad_Name", Group: "eng"}, ErrBadInput},
		"itself, role group":     {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "ambit-admins"}, ErrBadInput},
		"itself, bad owner":      {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "eng", Owner: "not an email"}, ErrBadInput},
		"itself, service owner":  {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "eng", Owner: "x@svc.ambit.internal"}, ErrBadInput},
		"itself, offboarded":     {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "eng", Owner: "blocked@example.com"}, ErrOffboarded},
		"itself with consent":    {adminA, AgentInput{DisplayName: "a", Mode: ModeItself, Name: "ci", Group: "eng", Consent: []string{"slack"}}, ErrBadInput},
	} {
		if _, err := s.CreateAgent(ctx, c.by, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if all, _ := s.AllAgents(ctx); len(all) != 0 {
		t.Errorf("a refused request left %d records", len(all))
	}
	if len(cleanScopeMust(t, "crm/"+strings.Repeat("a", 128))) != 1 { // 4 + 128 = 132 characters: allowed
		t.Error("a 128-character tool name is allowed")
	}
	// switched off
	s.Agents.Enabled = false
	if _, err := s.CreateAgent(ctx, user01(), AgentInput{DisplayName: "a"}); !errors.Is(err, ErrAgentsOff) {
		t.Errorf("agents off: %v", err)
	}
}

func cleanScopeMust(t *testing.T, e ...string) []string {
	t.Helper()
	out, err := cleanScope(e)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAgentLimits(t *testing.T) {
	s, _ := newAgentSvc(t)
	s.Agents.MaxTotal, s.Agents.MaxPerPerson = 12, 10
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		mustAgent(t, s, user01(), AgentInput{DisplayName: fmt.Sprintf("agent %d", i)})
	}
	if _, err := s.CreateAgent(ctx, user01(), AgentInput{DisplayName: "eleventh"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("the 11th agent of one person: %v", err)
	}
	other := Session{Email: "user02@acme.com", SignedIn: t0.Unix()}
	mustAgent(t, s, other, AgentInput{DisplayName: "other"})
	mustAgent(t, s, adminA, AgentInput{DisplayName: "ci agent", Mode: ModeItself, Name: "ci-agent", Group: "eng"}) // 12 in all
	if _, err := s.CreateAgent(ctx, other, AgentInput{DisplayName: "over the total"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("the 13th agent: %v", err)
	}
	if _, err := s.CreateAgent(ctx, adminA, AgentInput{DisplayName: "x", Mode: ModeItself, Name: "ci-two", Group: "eng"}); !errors.Is(err, ErrAgentLimit) {
		t.Fatalf("itself mode counts in the total: %v", err)
	}
	// a delete makes room again
	mine, _ := s.AgentsOf(ctx, "user01@acme.com")
	if err := s.DeleteAgent(ctx, user01(), mine[0].Name); err != nil {
		t.Fatal(err)
	}
	mustAgent(t, s, user01(), AgentInput{DisplayName: "again"})
}

func TestAgentKeyRules(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot", Scope: []string{"crm"}})
	user02 := Session{Email: "user02@acme.com", SignedIn: t0.Unix()}
	for name, c := range map[string]struct {
		by     Session
		name   string
		days   int
		ranges []string
		want   error
	}{
		"another person":     {user02, a.Name, 30, nil, ErrNoAgent},
		"unknown agent":      {user01(), "nope", 30, nil, ErrNoAgent},
		"longer than agents": {user01(), a.Name, 365, nil, ErrBadInput},
		"odd days":           {user01(), a.Name, 45, nil, ErrBadInput},
		"11 ranges":          {user01(), a.Name, 30, []string{"10.0.0.0/8", "10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16", "10.4.0.0/16", "10.5.0.0/16", "10.6.0.0/16", "10.7.0.0/16", "10.8.0.0/16", "10.9.0.0/16", "10.10.0.0/16"}, ErrBadInput},
		"not a CIDR":         {user01(), a.Name, 30, []string{"203.0.113.7"}, ErrBadInput},
		"zone":               {user01(), a.Name, 30, []string{"fe80::1%eth0/64"}, ErrBadInput},
		"empty key name":     {user01(), a.Name, 30, nil, ErrBadInput},
	} {
		keyName := "k"
		if name == "empty key name" {
			keyName = " "
		}
		if _, _, err := s.CreateAgentKey(ctx, c.by, c.name, keyName, c.days, c.ranges); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// exactly 10 ranges are fine, and the answer is sorted and canonical
	ten := []string{"10.9.0.0/16", "10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16", "10.4.0.0/16", "10.5.0.0/16", "10.6.0.0/16", "10.7.0.0/16", "10.8.0.0/16", "10.0.0.5/8"}
	_, k, err := s.CreateAgentKey(ctx, user01(), a.Name, "ten", 90, ten)
	if err != nil || len(k.Agent.SourceRanges) != 10 || k.Agent.SourceRanges[0] != "10.0.0.0/8" {
		t.Fatalf("10 ranges: %v %+v", err, k.Agent)
	}
	if _, _, err := s.CreateAgentKey(ctx, user01(), a.Name, "two", 30, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAgentKey(ctx, user01(), a.Name, "three", 30, nil); !errors.Is(err, ErrTooManyAgentKeys) {
		t.Errorf("a third live key: %v", err)
	}
	// a second agent has its own two
	b := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot 2"})
	if _, _, err := s.CreateAgentKey(ctx, user01(), b.Name, "own", 30, nil); err != nil {
		t.Errorf("the other agent: %v", err)
	}
	// a stopped agent gets no key; an offboarded person gets none either
	if _, err := s.changeAgent(ctx, b.Name, func(a *Agent) (bool, error) { a.State = StateStopped; return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAgentKey(ctx, user01(), b.Name, "late", 30, nil); !errors.Is(err, ErrAgentNotActive) {
		t.Errorf("stopped agent: %v", err)
	}
}

func TestItselfAgent(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, adminA, AgentInput{DisplayName: "Nightly report", Mode: ModeItself, Name: "nightly", Group: "ops", Platform: "agentcore",
		Owner: "Owner@Acme.com", Scope: []string{"reports"}})
	if a.Mode != ModeItself || a.Owner != "owner@acme.com" || a.Group != "ops" || a.Name != "nightly" || a.CreatedBy != "admin@example.com" {
		t.Fatalf("agent %+v", a)
	}
	if accts, _ := s.Accounts(ctx); len(accts) != 1 || accts[0].Owner.Email != AccountIdentity("nightly") || accts[0].Owner.Groups[0] != "ops" {
		t.Fatalf("the agent is a service account: %+v", accts)
	}
	// The Service accounts page may not hand out an unscoped key of the agent's account.
	if _, _, err := s.CreateAccountKey(ctx, adminA, "nightly", "bypass", ScopeMCP, 30); !errors.Is(err, ErrBadInput) {
		t.Fatalf("a plain key of an agent's account: %v", err)
	}
	// Only admins make keys of an itself-mode agent; person agents' owners cannot.
	if _, _, err := s.CreateAgentKey(ctx, user01(), "nightly", "k", 30, nil); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("a person: %v", err)
	}
	_, k, err := s.CreateAgentKey(ctx, adminA, "nightly", "report job", 90, []string{"198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	m := metadataOf(t, s, k.ID)
	if k.Email != AccountIdentity("nightly") || m["agentMode"] != "itself" || m["agentOwnerAtIssue"] != "owner@acme.com" || m["platformDeclared"] != "agentcore" ||
		fmt.Sprint(m["agentScope"]) != "[reports]" || k.IssuedBy != "admin@example.com" || !k.Until.Equal(k.Ceiling) {
		t.Errorf("key %+v metadata %v", k, m)
	}
	if !identity.IsService(k.Email) {
		t.Error("an itself-mode key is a service account key")
	}
	if _, _, err := s.CreateAgentKey(ctx, adminA, "nightly", "one too many", 90, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAgentKey(ctx, adminA, "nightly", "third", 90, nil); !errors.Is(err, ErrTooManyAccountKeys) {
		t.Errorf("the account's two keys: %v", err)
	}
	// a name taken by a service account leaves nothing behind
	if _, err := s.CreateAccount(ctx, adminA, "taken", "eng", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(ctx, adminA, AgentInput{DisplayName: "x", Mode: ModeItself, Name: "taken", Group: "eng"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("taken: %v", err)
	}
	if _, err := s.GetAgent(ctx, "taken"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("a record was left: %v", err)
	}
	// deleting the agent deletes its keys and record; the account name stays taken
	if err := s.DeleteAgent(ctx, adminA, "nightly"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListAll(ctx); len(keys) != 0 {
		t.Errorf("%d keys left", len(keys))
	}
	if _, err := s.CreateAgent(ctx, adminA, AgentInput{DisplayName: "x", Mode: ModeItself, Name: "nightly", Group: "eng"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a deleted agent's name: %v", err)
	}
}

// Deleting a service account on the Service accounts page also deletes an agent's record.
func TestDeletingTheAccountDeletesTheAgentRecord(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	mustAgent(t, s, adminA, AgentInput{DisplayName: "n", Mode: ModeItself, Name: "nightly", Group: "ops"})
	if err := s.DeleteAccount(ctx, adminA, "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAgent(ctx, "nightly"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("record: %v", err)
	}
}

func TestUpdateAgentCopiesTheScopeToKeys(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot", Platform: "grok", Scope: []string{"crm", "wiki"}})
	_, k, err := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	narrow := []string{"wiki/read"}
	b, err := s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &narrow})
	if err != nil || b.Rev != 2 || fmt.Sprint(b.Scope) != "[wiki/read]" {
		t.Fatalf("%+v %v", b, err)
	}
	m := metadataOf(t, s, k.ID)
	if fmt.Sprint(m["agentScope"]) != "[wiki/read]" || m["agentRev"] != float64(2) {
		t.Errorf("the key still has %v (rev %v)", m["agentScope"], m["agentRev"])
	}
	// platform change: a new revision too; display name and consent change nothing in keys
	p, name, consent := "claude-ai", "renamed", []string{"slack"}
	if b, err = s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Platform: &p, DisplayName: &name, Consent: &consent}); err != nil || b.Rev != 3 || b.DisplayName != "renamed" || fmt.Sprint(b.ConnectionConsent) != "[slack]" {
		t.Fatalf("%+v %v", b, err)
	}
	if m := metadataOf(t, s, k.ID); m["platformDeclared"] != "claude-ai" || m["agentOwnerAtIssue"] != "user01@acme.com" {
		t.Errorf("metadata %v", m)
	}
	// nothing changed: no new revision
	if b, _ = s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &narrow}); b.Rev != 3 {
		t.Errorf("same scope raised the revision to %d", b.Rev)
	}
	// an empty scope is a scope: the key may use no tool
	none := []string{}
	if b, err = s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &none}); err != nil || len(b.Scope) != 0 || b.Rev != 4 {
		t.Fatalf("%+v %v", b, err)
	}
	if _, has := metadataOf(t, s, k.ID)["agentScope"]; has {
		t.Error("an empty scope is left out of the key; the gateway then lets the key use no tool")
	}
	// others cannot change it
	if _, err := s.UpdateAgent(ctx, Session{Email: "user02@acme.com", SignedIn: t0.Unix()}, a.Name, AgentPatch{Scope: &narrow}); !errors.Is(err, ErrNoAgent) {
		t.Errorf("another person: %v", err)
	}
	if _, err := s.UpdateAgent(ctx, adminA, a.Name, AgentPatch{Scope: &narrow}); !errors.Is(err, ErrNoAgent) {
		t.Errorf("an admin does not change a person's agent: %v", err)
	}
}

// A failed copy to a key is reported, the same request finishes it, and so does the repair.
func TestScopeChangeThatCouldNotReachAKeyIsFinishedLater(t *testing.T) {
	failKeyUpdate := false
	s, _ := newAgentSvcWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if sec, ok := obj.(*corev1.Secret); ok && failKeyUpdate && sec.Labels[agentLabel] != "" {
			return errors.New("api server unavailable")
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "bot", Scope: []string{"crm", "wiki"}})
	_, k, _ := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
	narrow := []string{"wiki"}
	failKeyUpdate = true
	if _, err := s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &narrow}); !errors.Is(err, ErrKeysBehind) {
		t.Fatalf("want ErrKeysBehind, got %v", err)
	}
	if m := metadataOf(t, s, k.ID); fmt.Sprint(m["agentScope"]) != "[crm wiki]" {
		t.Fatalf("the key changed anyway: %v", m["agentScope"])
	}
	failKeyUpdate = false
	if _, err := s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &narrow}); err != nil { // the same request
		t.Fatal(err)
	}
	if m := metadataOf(t, s, k.ID); fmt.Sprint(m["agentScope"]) != "[wiki]" {
		t.Errorf("repeat did not finish it: %v", m["agentScope"])
	}
	// the repair does the same without a request
	failKeyUpdate = true
	wide := []string{"crm", "wiki", "mail"}
	_, _ = s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &wide})
	failKeyUpdate = false
	rep, err := s.Repair(ctx)
	if err != nil || rep.Updated != 1 {
		t.Fatalf("repair %+v %v", rep, err)
	}
	if m := metadataOf(t, s, k.ID); fmt.Sprint(m["agentScope"]) != "[crm mail wiki]" {
		t.Errorf("repair left %v", m["agentScope"])
	}
}

// Fail closed: a key whose agent has no readable record, or is not active, is deleted by the repair.
func TestRepairDeletesKeysOfAgentsWithoutARecord(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	gone := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "gone"})
	stopped := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "stopped"})
	broken := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "broken"})
	live := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: "live"})
	ids := map[string]string{}
	for _, a := range []Agent{gone, stopped, broken, live} {
		_, k, err := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[a.Name] = k.ID
	}
	var cm corev1.ConfigMap
	_ = s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: agentRecordPrefix + gone.Name}, &cm)
	if err := s.Client.Delete(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.changeAgent(ctx, stopped.Name, func(a *Agent) (bool, error) { a.State = StateStopped; return true, nil }); err != nil {
		t.Fatal(err)
	}
	_ = s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: agentRecordPrefix + broken.Name}, &cm)
	cm.Data["agent.json"] = "{not json"
	if err := s.Client.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.Repair(ctx); err != nil || rep.Deleted != 3 {
		t.Fatalf("repair %+v %v", rep, err)
	}
	for _, a := range []Agent{gone, stopped, broken} {
		if _, err := s.getKey(ctx, ids[a.Name]); !apierrors.IsNotFound(err) {
			t.Errorf("%s: key still there (%v)", a.Name, err)
		}
	}
	if _, err := s.getKey(ctx, ids[live.Name]); err != nil {
		t.Errorf("the live agent's key: %v", err)
	}
	// a failed listing of the records deletes nothing
	failList := false
	s2, _ := newAgentSvcWith(t, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if _, ok := l.(*corev1.ConfigMapList); ok && failList {
			if o := (&client.ListOptions{}).ApplyOptions(opts); o.LabelSelector != nil && strings.Contains(o.LabelSelector.String(), agentLabel) {
				return errors.New("unavailable")
			}
		}
		return c.List(ctx, l, opts...)
	}})
	b := mustAgent(t, s2, user01("eng"), AgentInput{DisplayName: "b"})
	_, k, err := s2.CreateAgentKey(ctx, user01(), b.Name, "k", 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	failList = true
	if _, err := s2.Repair(ctx); err == nil {
		t.Error("a repair that could not read the agents reported success")
	}
	if _, err := s2.getKey(ctx, k.ID); err != nil {
		t.Errorf("the key was deleted although the records could not be read: %v", err)
	}
}

func TestMoveAndDeleteAgent(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, adminA, AgentInput{DisplayName: "n", Mode: ModeItself, Name: "nightly", Group: "ops", Owner: "old@acme.com"})
	_, k, _ := s.CreateAgentKey(ctx, adminA, "nightly", "k", 30, nil)
	if _, err := s.MoveAgent(ctx, user01(), a.Name, "new@acme.com"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("a person moves: %v", err)
	}
	b, err := s.MoveAgent(ctx, adminA, a.Name, "group:platform")
	if err != nil || b.Owner != "group:platform" {
		t.Fatalf("%+v %v", b, err)
	}
	if m := metadataOf(t, s, k.ID); m["agentOwnerAtIssue"] != "old@acme.com" {
		t.Errorf("the key's owner at issue changed to %v", m["agentOwnerAtIssue"])
	}
	if owners, _ := s.AgentOwners(ctx); owners["nightly"] != "group:platform" {
		t.Errorf("owners %v", owners)
	}
	if _, err := s.MoveAgent(ctx, adminA, a.Name, "blocked or bad"); !errors.Is(err, ErrBadInput) {
		t.Errorf("bad owner: %v", err)
	}
	p := mustAgent(t, s, user01(), AgentInput{DisplayName: "mine"})
	if _, err := s.MoveAgent(ctx, adminA, p.Name, "user02@acme.com"); !errors.Is(err, ErrBadInput) {
		t.Errorf("a person-mode agent has no owner to move: %v", err)
	}
	// admins may delete anyone's person-mode agent; other people may not
	if err := s.DeleteAgent(ctx, Session{Email: "user02@acme.com", SignedIn: t0.Unix()}, p.Name); !errors.Is(err, ErrNoAgent) {
		t.Errorf("another person: %v", err)
	}
	if err := s.DeleteAgent(ctx, adminA, p.Name); err != nil {
		t.Errorf("admin: %v", err)
	}
	if err := s.DeleteAgent(ctx, adminA, p.Name); !errors.Is(err, ErrNoAgent) {
		t.Errorf("deleting twice: %v", err)
	}
	// a person may not delete an itself-mode agent they own
	if err := s.DeleteAgent(ctx, user01(), "nightly"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("a person deletes an itself-mode agent: %v", err)
	}
}

// 2,000 agents of 50 scope entries of 200 characters: the largest record, key metadata and list.
func TestLargestAgentFitsEveryLimit(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	scope := make([]string, 50)
	for i := range scope {
		// the longest entry a Server name (63) and a tool name (128) allow: 192 characters
		scope[i] = fmt.Sprintf("%s%02d/%s", strings.Repeat("s", 61), i, strings.Repeat("t", 128))
	}
	if len(scope[0]) != 192 {
		t.Fatalf("entry is %d characters", len(scope[0]))
	}
	a := mustAgent(t, s, user01("eng"), AgentInput{DisplayName: strings.Repeat("n", 100), Scope: scope, Consent: []string{"slack", "github"}})
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: agentRecordPrefix + a.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if n := len(cm.Data["agent.json"]); n > maxAgentRecord || n < 9000 {
		t.Errorf("the largest record is %d bytes (limit %d)", n, maxAgentRecord)
	}
	_, k, err := s.CreateAgentKey(ctx, user01(), a.Name, "k", 30, []string{"10.0.0.0/8", "10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16", "10.4.0.0/16", "10.5.0.0/16", "10.6.0.0/16", "10.7.0.0/16", "10.8.0.0/16", "10.9.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(secretOf(t, s, k.ID).Data["key"]); n > 16<<10 {
		t.Errorf("the key's metadata is %d bytes; the design allows 16 KiB", n)
	}
	// 51 entries or a 201-character entry change nothing
	scope = append(scope, "one-more")
	if _, err := s.UpdateAgent(ctx, user01(), a.Name, AgentPatch{Scope: &scope}); !errors.Is(err, ErrBadInput) {
		t.Errorf("51 entries: %v", err)
	}
}

// The agent record name never collides with other ConfigMaps (events, offboarding), and only records are listed.
func TestAgentRecordsAreLabelled(t *testing.T) {
	s, _ := newAgentSvc(t)
	ctx := context.Background()
	a := mustAgent(t, s, user01(), AgentInput{DisplayName: "bot"})
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "ambit-agent-" + a.Name}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Labels[agentLabel] != "true" || cm.Labels[agentOwnerLabel] != ownerHash("user01@acme.com") {
		t.Errorf("labels %v", cm.Labels)
	}
	// a ConfigMap of the same name without the label (planted) is not an agent
	cm2 := cm.DeepCopy()
	cm2.Labels = nil
	if _, err := agentFromRecord(cm2); err == nil {
		t.Error("an unlabelled record was accepted")
	}
	// a record whose content names another agent is refused
	cm3 := cm.DeepCopy()
	cm3.Name = "ambit-agent-other"
	if _, err := agentFromRecord(cm3); err == nil {
		t.Error("a record under another name was accepted")
	}
	if _, err := s.GetAgent(ctx, "../etc"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("odd name: %v", err)
	}
	if err := s.Client.Delete(ctx, &cm); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}
