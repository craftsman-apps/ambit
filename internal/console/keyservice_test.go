package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var t0 = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syncClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newKeySvc(t *testing.T, objs ...client.Object) (*KeyService, *syncClock) {
	return newKeySvcWith(t, interceptor.Funcs{}, objs...)
}

func newKeySvcWith(t *testing.T, f interceptor.Funcs, objs ...client.Object) (*KeyService, *syncClock) {
	t.Helper()
	clk := &syncClock{t: t0}
	c := fake.NewClientBuilder().WithObjects(objs...).WithInterceptorFuncs(f).Build()
	s := &KeyService{Client: c, Namespace: "ambit-system", Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365},
		Now: clk.Now}
	if err := s.StartTerm(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func alice(groups ...string) Session {
	return Session{Email: "Alice@Acme.com", Groups: groups, SignedIn: t0.Unix()}
}

func isKeySecret(obj client.Object) bool {
	_, ok := obj.(*corev1.Secret)
	return ok && obj.GetLabels()[keyLabel] == "true"
}

func TestCreateWritesOwnerFirstAndAKey(t *testing.T) {
	s, _ := newKeySvc(t)
	plain, k, err := s.Create(context.Background(), alice("eng"), "laptop", ScopeBoth, 90)
	if err != nil || plain == "" {
		t.Fatal(err)
	}
	if k.Email != "alice@acme.com" || !k.Until.Equal(t0.AddDate(0, 0, 30)) || !k.Ceiling.Equal(t0.AddDate(0, 0, 90)) {
		t.Fatalf("key %+v", k)
	}
	var o corev1.Secret
	if err := s.Client.Get(context.Background(), client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash("alice@acme.com")}, &o); err != nil {
		t.Fatal("owner record missing:", err)
	}
	if ev, _ := keyChanges(s); len(ev) != 1 || ev[0].Action != "create" || ev[0].KeyID != k.ID {
		t.Fatalf("events %+v", ev)
	}
}

func TestCreateWritesOwnerBeforeKey(t *testing.T) {
	var order []string
	s, _ := newKeySvcWith(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			order = append(order, obj.GetName())
		}
		return c.Create(ctx, obj, opts...)
	}})
	_, k, err := s.Create(context.Background(), alice("eng"), "laptop", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "ambit-key-owner-"+ownerHash("alice@acme.com") || order[1] != "ambit-key-"+k.ID {
		t.Fatalf("write order %v", order)
	}
}

func TestCreateRefusals(t *testing.T) {
	s, _ := newKeySvc(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system"},
		Data: map[string]string{"emails": `["bob@acme.com"]`}})
	ctx := context.Background()
	for _, c := range []struct {
		sess  Session
		name  string
		scope Scope
		days  int
		want  error
	}{
		{alice(), "", ScopeLLM, 90, ErrBadInput},
		{alice(), "   ", ScopeLLM, 90, ErrBadInput},
		{alice(), strings.Repeat("n", 101), ScopeLLM, 90, ErrBadInput},
		{alice(), "x", "all", 90, ErrBadInput},
		{alice(), "x", ScopeLLM, 60, ErrBadInput},
		{alice(), "x", ScopeLLM, 0, ErrBadInput},
		{Session{Email: "alice@acme.com"}, "x", ScopeLLM, 90, ErrBadInput}, // no verified sign-in time
		{Session{Email: "Bob@ACME.com", SignedIn: t0.Unix()}, "x", ScopeLLM, 90, ErrOffboarded},
		{Session{Email: "no email", SignedIn: t0.Unix()}, "x", ScopeLLM, 90, ErrBadInput},
	} {
		if _, _, err := s.Create(ctx, c.sess, c.name, c.scope, c.days); !errors.Is(err, c.want) {
			t.Errorf("%+v: got %v want %v", c, err, c.want)
		}
	}
	s.Settings.MaxDays = 90
	if _, _, err := s.Create(ctx, alice(), "x", ScopeLLM, 365); !errors.Is(err, ErrBadInput) {
		t.Error("365 days above maxDays accepted")
	}
	s.Settings.Enabled = false
	if _, _, err := s.Create(ctx, alice(), "x", ScopeLLM, 30); !errors.Is(err, ErrKeysOff) {
		t.Error("created while switched off")
	}
	s.Settings.Enabled = true
	s.EndTermForTest()
	if _, _, err := s.Create(ctx, alice(), "x", ScopeLLM, 30); !errors.Is(err, ErrNotLeader) {
		t.Error("created without the lease")
	}
	if all, _ := s.ListAll(ctx); len(all) != 0 {
		t.Fatalf("a refused request wrote a key: %+v", all)
	}
}

func TestCreateIsSerialized(t *testing.T) {
	s, _ := newKeySvc(t)
	s.PerHour = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	var ok, tooMany, other atomic.Int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := s.Create(context.Background(), alice(), "k", ScopeLLM, 30)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrTooMany):
				tooMany.Add(1)
			default:
				other.Add(1)
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	keys, _ := s.List(context.Background(), "alice@acme.com")
	if ok.Load() != 20 || tooMany.Load() != 5 || other.Load() != 0 || len(keys) != 20 {
		t.Fatalf("keys %d ok %d tooMany %d other %d", len(keys), ok.Load(), tooMany.Load(), other.Load())
	}
}

func TestHourlyCreationLimit(t *testing.T) {
	s, clk := newKeySvc(t)
	for i := 0; i < 10; i++ {
		if _, _, err := s.Create(context.Background(), alice(), "k", ScopeLLM, 30); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := s.Create(context.Background(), alice(), "k", ScopeLLM, 30); !errors.Is(err, ErrTooFast) {
		t.Fatal("11th in an hour accepted")
	}
	// Another person has their own allowance.
	if _, _, err := s.Create(context.Background(), Session{Email: "bob@acme.com", SignedIn: t0.Unix()}, "k", ScopeLLM, 30); err != nil {
		t.Fatal("bob refused:", err)
	}
	clk.Set(t0.Add(61 * time.Minute))
	if _, _, err := s.Create(context.Background(), alice(), "k", ScopeLLM, 30); err != nil {
		t.Fatal("after an hour:", err)
	}
}

func TestLimitCountsOnlyLiveKeys(t *testing.T) {
	s, clk := newKeySvc(t)
	s.PerHour = 100
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, _, err := s.Create(ctx, alice(), "k", ScopeLLM, 30); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := s.Create(ctx, alice(), "k", ScopeLLM, 30); !errors.Is(err, ErrTooMany) {
		t.Fatal("21st key accepted:", err)
	}
	// All 20 stopped (past until) but are not yet cleaned up: they no longer count.
	clk.Set(t0.AddDate(0, 0, 30).Add(time.Second))
	late := Session{Email: "alice@acme.com", SignedIn: clk.Now().Unix()}
	if _, _, err := s.Create(ctx, late, "k", ScopeLLM, 30); err != nil {
		t.Fatal("stopped keys counted against the limit:", err)
	}
}

func TestRefreshIsMonotonicAndDeletesExpired(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, alice("eng", "admins"), "live", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	now := t0.AddDate(0, 0, 25)
	clk.Set(now)
	newer := Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: now.Unix()}
	if err := s.Refresh(ctx, newer); err != nil {
		t.Fatal(err)
	}
	older := Session{Email: "alice@acme.com", Groups: []string{"eng", "admins"}, SignedIn: t0.Unix() + 60}
	if err := s.Refresh(ctx, older); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	if len(keys) != 1 || len(keys[0].Groups) != 1 || !keys[0].Until.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("an older refresh must not win: %+v", keys)
	}
	ev, _ := keyChanges(s)
	if len(ev) != 2 || ev[0].Action != "refresh" || ev[0].Detail != "keys=1" {
		t.Fatalf("one refresh event expected (the older one changed nothing): %+v", ev)
	}
	clk.Set(keys[0].Until.Add(time.Minute)) // inactive past until
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 0 {
		t.Fatalf("an expired key was revived: %+v", keys)
	}
}

func TestRefreshFindsOwnerByCanonicalEmail(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx, Session{Email: " ALICE@acme.COM", Groups: []string{"ops"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, " Alice@ACME.com")
	if len(keys) != 1 || keys[0].Groups[0] != "ops" {
		t.Fatalf("%+v", keys)
	}
}

func TestRefreshIgnoresOffboarded(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	// Owner record says offboarded (keys not yet deleted, e.g. a crash mid-offboarding).
	o, _ := s.owner(ctx, "alice@acme.com")
	o.Offboarded = true
	if err := s.putOwner(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"admins"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 1 || keys[0].Groups[0] != "eng" {
		t.Fatalf("refresh changed an offboarded owner's key: %+v", keys)
	}
	// Email in the offboarding records, owner record not (yet) marked.
	s2, _ := newKeySvc(t)
	if _, _, err := s2.Create(ctx, alice("eng"), "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	if err := s2.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system"},
		Data: map[string]string{"emails": `["alice@acme.com"]`}}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"admins"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s2.List(ctx, "alice@acme.com"); len(keys) != 1 || keys[0].Groups[0] != "eng" {
		t.Fatalf("refresh changed a blocked person's key: %+v", keys)
	}
	if o, _ := s2.owner(ctx, "alice@acme.com"); o.Groups[0] != "eng" {
		t.Fatalf("refresh changed a blocked person's owner record: %+v", o)
	}
}

func TestRefreshWithoutOwnerRecordIsANoop(t *testing.T) {
	s, _ := newKeySvc(t)
	if err := s.Refresh(context.Background(), Session{Email: "carol@acme.com", Groups: []string{"eng"}, SignedIn: t0.Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.owner(context.Background(), "carol@acme.com"); !apierrors.IsNotFound(err) {
		t.Fatal("refresh created an owner record:", err)
	}
}

func TestRefreshAfterLoweredLimitKeepsTheLowerCeiling(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, k, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 365)
	if err != nil {
		t.Fatal(err)
	}
	s.Settings.MaxDays = 30
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	s.Settings.MaxDays = 365 // the operator raises it again
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: t0.Unix() + 20}); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	if len(keys) != 1 || !keys[0].Ceiling.Equal(k.Created.AddDate(0, 0, 30)) || keys[0].Until.After(keys[0].Ceiling) {
		t.Fatalf("ceiling not kept lowered: %+v", keys)
	}
}

func TestRepairFromOwnerRecords(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := s.Create(ctx, alice("eng", "admins"), "k", ScopeLLM, 365); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a refresh that crashed after writing the owner record and one key.
	o, _ := s.owner(ctx, "alice@acme.com")
	o.Groups, o.SignedIn = []string{"eng"}, t0.Add(time.Hour)
	if err := s.putOwner(ctx, o); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	keys[0].Groups = []string{"eng"}
	if err := s.putKey(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	s.Settings.MaxDays = 30 // operator lowered the limit
	clk.Set(t0.Add(2 * time.Hour))
	rep, err := s.Repair(ctx)
	if err != nil || rep.Updated < 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	keys, _ = s.List(ctx, "alice@acme.com")
	for _, k := range keys {
		if len(k.Groups) != 1 || k.Ceiling.After(k.Created.AddDate(0, 0, 30)) || k.Until.After(k.Ceiling) {
			t.Fatalf("not repaired: %+v", k)
		}
	}
	// Idempotent: a second repair changes nothing.
	if rep, err := s.Repair(ctx); err != nil || rep != (RepairReport{}) {
		t.Fatalf("second repair %+v %v", rep, err)
	}
}

func TestRepairNeverExtendsACurrentKey(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, k, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	s.Settings.InactivityDays = 60 // a raised inactivity limit is not a sign-in
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	if len(keys) != 1 || !keys[0].Until.Equal(k.Until) {
		t.Fatalf("repair extended a key: %+v", keys)
	}
}

func TestRepairCompletesAnInterruptedRefresh(t *testing.T) {
	var failKeyUpdates atomic.Bool
	s, clk := newKeySvcWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if failKeyUpdates.Load() && isKeySecret(obj) {
			return errors.New("connection lost")
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, _, err := s.Create(ctx, alice("eng", "admins"), "k", ScopeLLM, 90); err != nil {
			t.Fatal(err)
		}
	}
	now := t0.AddDate(0, 0, 10)
	clk.Set(now)
	failKeyUpdates.Store(true)
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: now.Unix()}); err == nil {
		t.Fatal("refresh hid a failed key write")
	}
	if o, _ := s.owner(ctx, "alice@acme.com"); len(o.Groups) != 1 || !o.SignedIn.Equal(now) {
		t.Fatalf("owner record not written first: %+v", o)
	}
	failKeyUpdates.Store(false)
	rep, err := s.Repair(ctx)
	if err != nil || rep.Updated != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	for _, k := range keys {
		if len(k.Groups) != 1 || !k.SignedIn.Equal(now) || !k.Until.Equal(now.AddDate(0, 0, 30)) {
			t.Fatalf("refresh not completed by repair: %+v", k)
		}
	}
}

func TestKeyUpdateRetriesOnConflict(t *testing.T) {
	var conflicts atomic.Int32
	s, _ := newKeySvcWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if isKeySecret(obj) && conflicts.Add(1) == 1 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("changed"))
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	if _, _, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"ops"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 1 || keys[0].Groups[0] != "ops" || conflicts.Load() < 2 {
		t.Fatalf("conflict not retried: %+v (%d updates)", keys, conflicts.Load())
	}
}

func TestStaleResourceVersionIsRefused(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, k, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := s.List(ctx, "alice@acme.com")
	fresh := stale[0]
	fresh.Name = "renamed outside the console"
	if err := s.putKey(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteKey(ctx, stale[0], "revoke", "test"); !apierrors.IsConflict(err) {
		t.Fatal("delete with a stale resourceVersion was not refused:", err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 1 || keys[0].ID != k.ID {
		t.Fatal("key deleted despite the stale version")
	}
	// Revoke re-reads and succeeds.
	if err := s.Revoke(ctx, alice(), k.ID, false); err != nil {
		t.Fatal(err)
	}
}

func TestRepairDeletesOrphansOffboardedAndUnreadable(t *testing.T) {
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-bad", Namespace: "ambit-system", Labels: map[string]string{keyLabel: "true"}},
		Data: map[string][]byte{"key": []byte("{")}}
	s, _ := newKeySvc(t)
	ctx := context.Background()
	if err := s.Client.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.Create(ctx, alice(), "k", ScopeLLM, 90)
	_, _, _ = s.Create(ctx, Session{Email: "bob@acme.com", SignedIn: t0.Unix()}, "k", ScopeLLM, 90)
	_, _, _ = s.Create(ctx, Session{Email: "carol@acme.com", SignedIn: t0.Unix()}, "k", ScopeLLM, 90)
	_ = s.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system"},
		Data: map[string]string{"emails": `["bob@acme.com"]`}})
	o, _ := s.owner(ctx, "alice@acme.com")
	_ = s.Client.Delete(ctx, o.Secret("ambit-system"))
	rep, err := s.Repair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListAll(ctx)
	if len(all) != 1 || all[0].Email != "carol@acme.com" || rep.Deleted != 3 {
		t.Fatalf("left %+v, report %+v", all, rep)
	}
}

func TestOffboardKeysDeletesAndMarksOwner(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, _, _ = s.Create(ctx, alice(), "a", ScopeLLM, 90)
	_, _, _ = s.Create(ctx, alice(), "b", ScopeMCP, 90)
	n, err := s.OffboardKeys(ctx, "ALICE@acme.com", "scim PATCH active=false")
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if o, _ := s.owner(ctx, "alice@acme.com"); !o.Offboarded {
		t.Fatal("owner not marked")
	}
	if _, _, err := s.Create(ctx, alice(), "c", ScopeLLM, 90); !errors.Is(err, ErrOffboarded) {
		t.Fatal("created after offboarding")
	}
	ev, _ := s.Events(ctx)
	if ev[0].Action != "offboard-delete" || ev[0].Actor != "scim PATCH active=false" {
		t.Fatalf("%+v", ev)
	}
	// Idempotent, and works for someone who never had keys (owner record created, marked).
	if n, err := s.OffboardKeys(ctx, "alice@acme.com", "retry"); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := s.OffboardKeys(ctx, "dave@acme.com", "scim"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.owner(ctx, "dave@acme.com"); !o.Offboarded {
		t.Fatal("owner without keys not marked")
	}
	s.EndTermForTest()
	if _, err := s.OffboardKeys(ctx, "alice@acme.com", "scim"); !errors.Is(err, ErrNotLeader) {
		t.Fatal("offboarded without the lease:", err)
	}
}

func TestReactivationAllowsNewKeys(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	if _, err := s.OffboardKeys(ctx, "alice@acme.com", "scim"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, alice(), "c", ScopeLLM, 90); !errors.Is(err, ErrOffboarded) {
		t.Fatal("created while offboarded")
	}
	if err := s.Reactivate(ctx, "Alice@Acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, alice(), "c", ScopeLLM, 90); err != nil {
		t.Fatal("refused after reactivation:", err)
	}
	if err := s.Reactivate(ctx, "erin@acme.com"); err != nil { // no owner record: nothing to do
		t.Fatal(err)
	}
	if _, err := s.owner(ctx, "erin@acme.com"); !apierrors.IsNotFound(err) {
		t.Fatal("reactivation created an owner record")
	}
}

func TestRevokeOnlyOwnOrAdmin(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, k, _ := s.Create(ctx, alice(), "a", ScopeLLM, 90)
	if err := s.Revoke(ctx, Session{Email: "bob@acme.com"}, k.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob revoked alice's key")
	}
	if err := s.Revoke(ctx, Session{Email: "bob@acme.com"}, "../../etc", false); !errors.Is(err, ErrNotFound) {
		t.Fatal("bad id accepted")
	}
	if err := s.Revoke(ctx, Session{Email: "root@acme.com", Role: RoleAdmin}, k.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, alice(), k.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked twice:", err)
	}
	ev, _ := s.Events(ctx)
	if ev[0].Action != "revoke" || ev[0].Actor != "root@acme.com" || ev[0].KeyID != k.ID {
		t.Fatalf("%+v", ev)
	}
}

func TestEventsOlderThan400DaysAreDeleted(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	_, _, _ = s.Create(ctx, alice(), "a", ScopeLLM, 30)
	clk.Set(t0.AddDate(0, 0, 401))
	_, _ = s.Repair(ctx)
	if ev, _ := keyChanges(s); len(ev) != 1 || ev[0].Action != "cleanup-delete" {
		t.Fatalf("%+v", ev) // the create event is gone; the key's cleanup deletion is recorded
	}
}

func TestEventsHoldNoSecrets(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	plain, k, _ := s.Create(ctx, alice(), "a", ScopeLLM, 30)
	var cms corev1.ConfigMapList
	if err := s.Client.List(ctx, &cms, client.HasLabels{keyEventLabel}); err != nil || len(cms.Items) == 0 {
		t.Fatal(err, len(cms.Items))
	}
	for _, v := range cms.Items[0].Data {
		if strings.Contains(v, plain) || strings.Contains(v, strings.TrimPrefix(k.Hash, "sha256:")) {
			t.Fatal("event holds the key or its hash")
		}
	}
}

func TestExpireForTest(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, k, _ := s.Create(ctx, alice(), "a", ScopeLLM, 30)
	soon := t0.Add(72 * time.Hour)
	if err := s.ExpireForTest(ctx, k.ID, soon); err != nil {
		t.Fatal(err)
	}
	later := t0.AddDate(0, 0, 100)
	if err := s.ExpireForTest(ctx, k.ID, later); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	if len(keys) != 1 || !keys[0].Until.Equal(later) || !keys[0].Ceiling.Equal(later) {
		t.Fatalf("%+v", keys)
	}
	if err := s.ExpireForTest(ctx, "aaaaaaaaaaaa", soon); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestWritableFollowsTheTerm(t *testing.T) {
	s, _ := newKeySvc(t)
	if !s.Writable() {
		t.Fatal("not writable after StartTerm")
	}
	s.EndTermForTest()
	if s.Writable() {
		t.Fatal("writable after the term ended")
	}
	for name, err := range map[string]error{
		"revoke":     s.Revoke(context.Background(), alice(), "aaaaaaaaaaaa", false),
		"refresh":    s.Refresh(context.Background(), alice()),
		"reactivate": s.Reactivate(context.Background(), "alice@acme.com"),
		"repair":     func() error { _, err := s.Repair(context.Background()); return err }(),
		"expire":     s.ExpireForTest(context.Background(), "aaaaaaaaaaaa", t0),
	} {
		if !errors.Is(err, ErrNotLeader) {
			t.Errorf("%s without a term: %v", name, err)
		}
	}
}

func TestStartTermRetriesRepair(t *testing.T) {
	old := termBackoff
	termBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2, Cap: 10 * time.Millisecond, Steps: 1 << 30}
	t.Cleanup(func() { termBackoff = old })
	var lists atomic.Int32
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if lists.Add(1) <= 2 {
			return errors.New("api server unavailable")
		}
		return c.List(ctx, l, opts...)
	}}).Build()
	s := &KeyService{Client: c, Namespace: "ambit-system", Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}, Now: time.Now}
	if err := s.StartTerm(context.Background()); err != nil || !s.Writable() {
		t.Fatal(err, s.Writable())
	}
}

func TestStartTermPublishesNothingWhenTheTermEnded(t *testing.T) {
	lead, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		cancel() // the lease is lost while the repair runs
		return c.List(context.Background(), l, opts...)
	}}).Build()
	s := &KeyService{Client: c, Namespace: "ambit-system", Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}, Now: time.Now}
	if err := s.StartTerm(lead); err == nil || s.Writable() {
		t.Fatal("a term was published after it ended:", err)
	}
}

func TestEndingTheTermCancelsAnOperation(t *testing.T) {
	entered := make(chan struct{})
	s, _ := newKeySvcWith(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if isKeySecret(obj) {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return c.Create(ctx, obj, opts...)
	}})
	lead, cancel := context.WithCancel(context.Background())
	if err := s.StartTerm(lead); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, _, err := s.Create(context.Background(), alice(), "a", ScopeLLM, 30)
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation not cancelled when the term ended")
	}
}

// The console's Secret permission covers every Secret in the namespace; the code must only ever read key objects.
func TestKeyReadsAreLabelFiltered(t *testing.T) {
	check := func(t *testing.T, l client.ObjectList, opts []client.ListOption) {
		switch l.(type) {
		case *corev1.SecretList, *corev1.ConfigMapList:
		default:
			return
		}
		o := (&client.ListOptions{}).ApplyOptions(opts)
		if o.LabelSelector == nil || !strings.Contains(o.LabelSelector.String(), keyLabel) {
			t.Errorf("unfiltered %T list (selector %v)", l, o.LabelSelector)
		}
	}
	s, _ := newKeySvcWith(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			check(t, l, opts)
			return c.List(ctx, l, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && !strings.HasPrefix(key.Name, "ambit-key-") {
				t.Errorf("read Secret %s", key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	ctx := context.Background()
	_, k, _ := s.Create(ctx, alice("eng"), "a", ScopeLLM, 90)
	_, _ = s.List(ctx, "alice@acme.com")
	_, _ = s.ListAll(ctx)
	_ = s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"ops"}, SignedIn: t0.Unix() + 1})
	_ = s.ExpireForTest(ctx, k.ID, t0.Add(time.Hour))
	_, _ = s.Repair(ctx)
	_, _ = s.Events(ctx)
	_ = s.Revoke(ctx, alice(), k.ID, false)
	_, _ = s.OffboardKeys(ctx, "alice@acme.com", "scim")
	_ = s.Reactivate(ctx, "alice@acme.com")
}

// keyChanges returns the recorded events except the start-of-term settings record.
func keyChanges(s *KeyService) ([]KeyEvent, error) {
	ev, err := s.Events(context.Background())
	return slices.DeleteFunc(ev, func(e KeyEvent) bool { return e.Action == "settings-changed" }), err
}

func fastTermBackoff(t *testing.T) {
	old := termBackoff
	termBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2, Cap: 4 * time.Millisecond, Steps: 1 << 30}
	t.Cleanup(func() { termBackoff = old })
}

func captureLogs(t *testing.T) *lockedBuffer {
	b := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStartTermKeepsRetryingPastTheCap(t *testing.T) {
	fastTermBackoff(t) // 1, 2, 4 ms, then 4 ms forever
	var lists atomic.Int32
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if lists.Add(1) <= 12 {
			return errors.New("api server unavailable")
		}
		return c.List(ctx, l, opts...)
	}}).Build()
	s := &KeyService{Client: c, Namespace: "ambit-system", Settings: KeySettings{Enabled: true, InactivityDays: 30, MaxDays: 365}, Now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.StartTerm(ctx); err != nil || !s.Writable() {
		t.Fatalf("gave up after %d listings: %v", lists.Load(), err)
	}
}

func TestUnreadableOwnerRecordDeletesItsKeysAndTermStarts(t *testing.T) {
	fastTermBackoff(t)
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, ka, _ := s.Create(ctx, alice("eng"), "a", ScopeLLM, 90)
	_, kb, _ := s.Create(ctx, Session{Email: "bob@acme.com", SignedIn: t0.Unix()}, "b", ScopeLLM, 90)
	var o corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash("alice@acme.com")}, &o); err != nil {
		t.Fatal(err)
	}
	o.Data = map[string][]byte{"owner.json": []byte("{")}
	if err := s.Client.Update(ctx, &o); err != nil {
		t.Fatal(err)
	}
	lead, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.StartTerm(lead); err != nil || !s.Writable() {
		t.Fatal("term did not start:", err)
	}
	all, _ := s.ListAll(ctx)
	if len(all) != 1 || all[0].ID != kb.ID {
		t.Fatalf("left %+v", all)
	}
	ev, _ := keyChanges(s)
	if ev[0].Action != "repair-delete" || ev[0].KeyID != ka.ID {
		t.Fatalf("%+v", ev)
	}
}

func TestStartTermSurvivesAKeyThatCannotBeWritten(t *testing.T) {
	fastTermBackoff(t)
	logs := captureLogs(t)
	var stuck atomic.Value
	stuck.Store("")
	s, _ := newKeySvcWith(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == stuck.Load().(string) {
			return errors.New("admission refused")
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	_, k1, _ := s.Create(ctx, alice("eng"), "a", ScopeLLM, 365)
	_, k2, _ := s.Create(ctx, alice("eng"), "b", ScopeLLM, 365)
	stuck.Store("ambit-key-" + k1.ID)
	s.Settings.MaxDays = 30 // both keys need a lower ceiling
	lead, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.StartTerm(lead); err != nil || !s.Writable() {
		t.Fatal("one stuck key blocked the term:", err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	for _, k := range keys {
		lowered := !k.Ceiling.After(k.Created.AddDate(0, 0, 30))
		if (k.ID == k2.ID) != lowered {
			t.Fatalf("key %s lowered=%v", k.ID, lowered)
		}
	}
	if !strings.Contains(logs.String(), "admission refused") {
		t.Fatalf("error not logged: %s", logs.String())
	}
	if _, err := s.Repair(ctx); err == nil {
		t.Fatal("the hourly repair hid the stuck key")
	}
}

func TestStartTermRecordsSettingsChanges(t *testing.T) {
	s, _ := newKeySvc(t) // first start records
	ctx := context.Background()
	settings := func() []KeyEvent {
		ev, _ := s.Events(ctx)
		return slices.DeleteFunc(ev, func(e KeyEvent) bool { return e.Action != "settings-changed" })
	}
	if ev := settings(); len(ev) != 1 || ev[0].Detail != "enabled=true inactivityDays=30 maxDays=365" {
		t.Fatalf("%+v", ev)
	}
	if err := s.StartTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := settings(); len(ev) != 1 {
		t.Fatalf("same settings recorded again: %+v", ev)
	}
	s.Settings.MaxDays = 90
	if err := s.StartTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := settings(); len(ev) != 2 || ev[0].Detail != "enabled=true inactivityDays=30 maxDays=90" {
		t.Fatalf("%+v", ev)
	}
}

func TestListDoesNotWaitForTheLock(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, _, _ = s.Create(ctx, alice(), "a", ScopeLLM, 90)
	s.mu.Lock() // as a long repair would
	defer s.mu.Unlock()
	done := make(chan int)
	go func() {
		keys, _ := s.List(ctx, "alice@acme.com")
		all, _ := s.ListAll(ctx)
		done <- len(keys) + len(all)
	}()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatal(n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("List waited for the lock")
	}
}

func TestOffboardKeysReplacesAnUnreadableOwnerRecord(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	_, _, _ = s.Create(ctx, alice("eng"), "a", ScopeLLM, 90)
	_, _, _ = s.Create(ctx, alice("eng"), "b", ScopeMCP, 90)
	var raw corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash("alice@acme.com")}, &raw); err != nil {
		t.Fatal(err)
	}
	raw.Data = map[string][]byte{"owner.json": []byte("{")}
	if err := s.Client.Update(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	n, err := s.OffboardKeys(ctx, "Alice@Acme.com", "scim PATCH active=false")
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 0 {
		t.Fatalf("keys left: %+v", keys)
	}
	o, err := s.owner(ctx, "alice@acme.com")
	if err != nil || !o.Offboarded || o.Email != "alice@acme.com" {
		t.Fatalf("owner %+v %v", o, err)
	}
	if ev, _ := keyChanges(s); ev[0].Action != "offboard-delete" || ev[2].Action != "offboard" {
		t.Fatalf("%+v", ev)
	}
	if _, _, err := s.Create(ctx, alice(), "c", ScopeLLM, 90); !errors.Is(err, ErrOffboarded) {
		t.Fatal("created after offboarding:", err)
	}
}

func TestStaleStartTermDoesNotEndALiveTerm(t *testing.T) {
	s, _ := newKeySvc(t) // publishes a live term (lead2)
	lead1, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.StartTerm(lead1); err == nil || !s.Writable() {
		t.Fatal("a stale start ended the live term:", err, s.Writable())
	}
}

// A component that may create Secrets (but not pods) plants an unlabelled owner record before alice's first key.
func TestForgedOwnerRecordIsIgnored(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	forged, _ := json.Marshal(Owner{Email: "alice@acme.com", Groups: []string{"admins"}, SignedIn: t0.AddDate(5, 0, 0)})
	if err := s.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-owner-" + ownerHash("alice@acme.com"),
		Namespace: "ambit-system"}, Data: map[string][]byte{"owner.json": forged}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.owner(ctx, "alice@acme.com"); !errors.Is(err, errBadOwner) {
		t.Fatal("unlabelled owner record accepted:", err)
	}
	_, k, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 365)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.Groups, []string{"eng"}) || !k.Until.Equal(t0.AddDate(0, 0, 30)) || !k.SignedIn.Equal(t0) {
		t.Fatalf("key took the forged record: %+v", k)
	}
	o, err := s.owner(ctx, "alice@acme.com")
	if err != nil || !slices.Equal(o.Groups, []string{"eng"}) {
		t.Fatalf("owner record not replaced: %+v %v", o, err)
	}
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 1 || !slices.Equal(keys[0].Groups, []string{"eng"}) || !keys[0].Until.Equal(k.Until) {
		t.Fatalf("repair copied forged groups: %+v", keys)
	}
}

func TestOwnerRecordForAnotherEmailIsBad(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	o := Owner{Email: "mallory@acme.com", Groups: []string{"admins"}, SignedIn: t0}
	sec := o.Secret("ambit-system")
	sec.Name = "ambit-key-owner-" + ownerHash("alice@acme.com")
	if err := s.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.owner(ctx, "alice@acme.com"); !errors.Is(err, errBadOwner) {
		t.Fatal("owner record of another email accepted:", err)
	}
}

func TestOffboardAndReactivateLabelAPlantedOwnerRecord(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	for _, email := range []string{"alice@acme.com", "bob@acme.com"} {
		raw, _ := json.Marshal(Owner{Email: email, Groups: []string{"admins"}, SignedIn: t0})
		if err := s.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-owner-" + ownerHash(email),
			Namespace: "ambit-system"}, Data: map[string][]byte{"owner.json": raw}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OffboardKeys(ctx, "alice@acme.com", "scim"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reactivate(ctx, "bob@acme.com"); err != nil {
		t.Fatal(err)
	}
	if o, err := s.owner(ctx, "alice@acme.com"); err != nil || !o.Offboarded || len(o.Groups) != 0 {
		t.Fatalf("offboarding did not replace the planted record: %+v %v", o, err)
	}
	if o, err := s.owner(ctx, "bob@acme.com"); err != nil || o.Offboarded || len(o.Groups) != 0 {
		t.Fatalf("reactivation did not replace the planted record: %+v %v", o, err)
	}
}

func TestCreateWithANewerSignInUpdatesOtherKeys(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	_, old, err := s.Create(ctx, alice("eng", "admins"), "old", ScopeLLM, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.AddDate(0, 0, 10)
	clk.Set(now)
	if _, _, err := s.Create(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: now.Unix()}, "new", ScopeLLM, 365); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List(ctx, "alice@acme.com")
	if len(keys) != 2 || keys[0].ID != old.ID {
		t.Fatalf("%+v", keys)
	}
	if k := keys[0]; !slices.Equal(k.Groups, []string{"eng"}) || !k.SignedIn.Equal(now) || !k.Until.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("the older key kept the older sign-in: %+v", k)
	}
}
