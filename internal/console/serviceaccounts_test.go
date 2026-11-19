package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var (
	adminA = Session{Email: "Admin@Example.com", Role: RoleAdmin, SignedIn: t0.Unix()}
	adminB = Session{Email: "admin-b@example.com", Role: RoleAdmin, SignedIn: t0.Unix()}
)

func blockList(emails string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system"}, Data: map[string]string{"emails": emails}}
}

// mustAccount creates account name in group and one key issued by by; it returns the key.
func mustAccount(t *testing.T, s *KeyService, by Session, name, group string) Key {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, by, name, group, "GitHub Actions, repo web"); err != nil {
		t.Fatal(err)
	}
	_, k, err := s.CreateAccountKey(ctx, by, name, "gha", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func secretBytes(t *testing.T, s *KeyService, name string) []byte {
	t.Helper()
	var sec corev1.Secret
	if err := s.Client.Get(context.Background(), client.ObjectKey{Namespace: "ambit-system", Name: name}, &sec); err != nil {
		t.Fatal(err)
	}
	return append(append([]byte(sec.ResourceVersion), '|'), sec.Data["owner.json"]...)
}

func keyOf(t *testing.T, s *KeyService, id string) Key {
	t.Helper()
	k, err := s.getKey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Section 4: the name rule; the identity passes CanonicalEmail unchanged and the AccessRule users pattern.
func TestAccountNamesAndIdentity(t *testing.T) {
	users := regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`) // AccessRuleSpec.Users items
	for name, ok := range map[string]bool{
		"ci": true, "ci-deploy": true, "a": true, "0": true, strings.Repeat("a", 40): true, "nightly-eval-2": true,
		strings.Repeat("a", 41): false, "": false, "-ci": false, "ci-": false, "CI": false, "ci_deploy": false,
		"ci.deploy": false, "ci deploy": false, "ci@x": false, "ü": false,
	} {
		if accountNamePattern.MatchString(name) != ok {
			t.Errorf("name %q: valid = %v, want %v", name, !ok, ok)
		}
		if !ok {
			continue
		}
		id := AccountIdentity(name)
		if c, good := identity.CanonicalEmail(id); !good || c != id || !users.MatchString(id) || !identity.IsService(id) {
			t.Errorf("identity %q: canonical %q %v, users pattern %v", id, c, good, users.MatchString(id))
		}
	}
}

// Section 5.2: every refusal of account and key creation.
func TestAccountRefusals(t *testing.T) {
	s, clk := newKeySvc(t, blockList(`["blocked@example.com"]`))
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, adminA, "ci", "eng", "x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, group, desc string
		want              error
	}{
		{"Bad_Name", "eng", "", ErrBadInput},
		{"ci-2", "ambit-admins", "", ErrBadInput},
		{"ci-2", "ambit-approvers", "", ErrBadInput},
		{"ci-2", "-eng", "", ErrBadInput},
		{"ci-2", "", "", ErrBadInput},
		{"ci-2", strings.Repeat("g", 129), "", ErrBadInput},
		{"ci-2", "eng", strings.Repeat("d", 201), ErrBadInput},
		{"ci", "eng", "", ErrNameTaken},
	} {
		if _, err := s.CreateAccount(ctx, adminA, c.name, c.group, c.desc); !errors.Is(err, c.want) {
			t.Errorf("create %q %q: %v, want %v", c.name, c.group, err, c.want)
		}
	}
	if _, err := s.CreateAccount(ctx, adminA, "ci-2", "eng", strings.Repeat("d", 200)); err != nil {
		t.Errorf("200 characters of description: %v", err)
	}
	// keys
	for _, c := range []struct {
		by      Session
		account string
		days    int
		want    error
	}{
		{Session{Email: "blocked@example.com", Role: RoleAdmin}, "ci", 90, ErrOffboarded},
		{adminA, "nope", 90, ErrNoAccount},
		{adminA, "ci", 365, ErrBadInput}, // maxDays 90
		{adminA, "ci", 7, ErrBadInput},
	} {
		if _, _, err := s.CreateAccountKey(ctx, c.by, c.account, "k", ScopeLLM, c.days); !errors.Is(err, c.want) {
			t.Errorf("key %s on %q, %d days: %v, want %v", c.by.Email, c.account, c.days, err, c.want)
		}
	}
	for range 2 {
		if _, _, err := s.CreateAccountKey(ctx, adminA, "ci", "k", ScopeLLM, 30); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateAccountKey(ctx, adminA, "ci", "k3", ScopeLLM, 30); !errors.Is(err, ErrTooManyAccountKeys) {
		t.Errorf("third key: %v", err)
	}
	// the admin's 10 creations per hour count personal and service account keys together
	clk.Set(t0.Add(2 * time.Hour))
	s.created = nil
	for i := range 10 {
		if _, err := s.CreateAccount(ctx, adminA, "rate-"+string(rune('a'+i)), "eng", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		if _, _, err := s.Create(ctx, Session{Email: adminA.Email, SignedIn: clk.Now().Unix()}, "laptop", ScopeLLM, 30); err != nil {
			t.Fatal(i, err)
		}
	}
	for i := range 6 {
		if _, _, err := s.CreateAccountKey(ctx, adminA, "rate-"+string(rune('a'+i)), "k", ScopeLLM, 30); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := s.CreateAccountKey(ctx, adminA, "rate-j", "k", ScopeLLM, 30); !errors.Is(err, ErrTooFast) {
		t.Errorf("11th key in an hour: %v", err)
	}
	// switched off
	s.Services.Enabled = false
	if _, err := s.CreateAccount(ctx, adminA, "off", "eng", ""); !errors.Is(err, ErrServicesOff) {
		t.Errorf("accounts off: %v", err)
	}
	if _, _, err := s.CreateAccountKey(ctx, adminB, "ci-2", "k", ScopeLLM, 30); !errors.Is(err, ErrServicesOff) {
		t.Errorf("accounts off, key: %v", err)
	}
	if err := s.DeleteAccount(ctx, adminB, "ci-2"); err != nil {
		t.Errorf("delete must work while switched off: %v", err)
	}
	s.Services.Enabled, s.Settings.Enabled = true, false
	if _, err := s.CreateAccount(ctx, adminA, "off", "eng", ""); !errors.Is(err, ErrKeysOff) {
		t.Errorf("keys off: %v", err)
	}
}

func TestTheTwoHundredFirstAccountIsRefused(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	for i := range maxAccounts {
		if _, err := s.CreateAccount(ctx, adminA, fmt.Sprintf("a%03d", i), "eng", ""); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.CreateAccount(ctx, adminA, "one-more", "eng", ""); !errors.Is(err, ErrTooManyAccounts) {
		t.Fatalf("201st: %v", err)
	}
	if err := s.DeleteAccount(ctx, adminA, "a000"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, adminA, "one-more", "eng", ""); err != nil {
		t.Fatalf("a tombstone does not count as live: %v", err)
	}
}

// Section 5.2: names are never reused. Any object at the record's name keeps the name: a tombstone, an unreadable
// record, an unlabelled (planted) one.
func TestAccountNameStaysTaken(t *testing.T) {
	id := AccountIdentity("ci")
	name := "ambit-key-owner-" + ownerHash(id)
	for label, obj := range map[string]*corev1.Secret{
		"unreadable": {ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", Labels: map[string]string{ownerRecordLabel: "true"}}, Data: map[string][]byte{"owner.json": []byte("{")}},
		"unlabelled": {ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"}, Data: map[string][]byte{"owner.json": []byte(`{"email":"` + id + `","kind":"service-account","groups":["eng"]}`)}},
	} {
		s, _ := newKeySvc(t, obj)
		if _, err := s.CreateAccount(context.Background(), adminA, "ci", "eng", ""); !errors.Is(err, ErrNameTaken) {
			t.Errorf("%s record: %v", label, err)
		}
	}
	s, _ := newKeySvc(t)
	ctx := context.Background()
	mustAccount(t, s, adminA, "ci", "eng")
	if err := s.DeleteAccount(ctx, adminA, "ci"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, adminA, "ci"); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	if _, err := s.CreateAccount(ctx, adminA, "ci", "eng", ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("after delete: %v", err)
	}
	if _, _, err := s.CreateAccountKey(ctx, adminA, "ci", "k", ScopeLLM, 30); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("key on a deleted account: %v", err)
	}
	if a, _ := s.Accounts(ctx); len(a) != 0 {
		t.Fatalf("a tombstone is listed: %+v", a)
	}
}

// Section 6: a service account key's until is its end date (no inactivity rule); lowering maxDays lowers the end
// dates for good.
func TestAccountKeyUntil(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	k := mustAccount(t, s, adminA, "ci", "eng")
	if !k.Until.Equal(t0.AddDate(0, 0, 90)) || !k.Ceiling.Equal(k.Until) || k.IssuedBy != "admin@example.com" || k.Groups[0] != "eng" {
		t.Fatalf("key %+v", k)
	}
	clk.Set(t0.AddDate(0, 0, 45)) // long past inactivityDays (30): nobody signs in for a service account
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keyOf(t, s, k.ID); !got.Until.Equal(t0.AddDate(0, 0, 90)) {
		t.Fatalf("until moved: %v", got.Until)
	}
	s.Services.MaxDays = 30
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keyOf(t, s, k.ID); !got.Until.Equal(t0.AddDate(0, 0, 30)) { // in the past: the gateway refuses it
		t.Fatalf("maxDays 30: until %v", got.Until)
	}
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
		t.Fatalf("the next repair must delete the ended key: %v", err)
	}
	clk.Set(t0)
	s.Services.MaxDays = 90
	k = mustAccount(t, s, adminA, "ci-2", "eng")
	s.Services.MaxDays = 30
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	s.Services.MaxDays = 90
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keyOf(t, s, k.ID); !got.Until.Equal(t0.AddDate(0, 0, 30)) {
		t.Fatalf("raising maxDays raised the end date: %v", got.Until)
	}
}

// Section 6: repair keeps a live account's keys and deletes the keys of a tombstoned or unreadable account; it
// never deletes the record, so the name stays taken.
func TestRepairOfAccountKeys(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	live := mustAccount(t, s, adminA, "live", "eng")
	tomb := mustAccount(t, s, adminA, "tomb", "eng")
	bad := mustAccount(t, s, adminA, "bad", "eng")
	unl := mustAccount(t, s, adminA, "unlabelled", "eng")
	// tombstone without deleting the keys (as after a crash mid-delete)
	if _, _, err := s.changeOwnerKind(ctx, KindServiceAccount, tomb.Email, nil, func(o *Owner, _ bool) (bool, error) { o.Deleted = true; return true, nil }); err != nil {
		t.Fatal(err)
	}
	for email, change := range map[string]func(*corev1.Secret){
		bad.Email: func(sec *corev1.Secret) { sec.Data["owner.json"] = []byte("{") },
		unl.Email: func(sec *corev1.Secret) { delete(sec.Labels, ownerRecordLabel) },
	} {
		var sec corev1.Secret
		_ = s.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash(email)}, &sec)
		change(&sec)
		if err := s.Client.Update(ctx, &sec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getKey(ctx, live.ID); err != nil {
		t.Errorf("live account's key: %v", err)
	}
	for _, k := range []Key{tomb, bad, unl} {
		if _, err := s.getKey(ctx, k.ID); !apierrors.IsNotFound(err) {
			t.Errorf("key of %s survived: %v", k.Email, err)
		}
		var sec corev1.Secret
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash(k.Email)}, &sec); err != nil {
			t.Errorf("record of %s deleted: %v", k.Email, err)
		}
		if _, err := s.CreateAccount(ctx, adminA, strings.TrimSuffix(k.Email, identity.ServiceDomain), "eng", ""); !errors.Is(err, ErrNameTaken) {
			t.Errorf("name %s free again: %v", k.Email, err)
		}
	}
}

// Section 6 delete: a failing key delete still writes the tombstone, answers ErrDeleteIncomplete, and the next
// repair deletes the key.
func TestDeleteAccountWithAFailingKeyDelete(t *testing.T) {
	var fail sync.Map
	s, _ := newKeySvcWith(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := fail.Load(obj.GetName()); ok {
			return apierrors.NewServiceUnavailable("etcd hiccup")
		}
		return c.Delete(ctx, obj, opts...)
	}})
	ctx := context.Background()
	k1 := mustAccount(t, s, adminA, "ci", "eng")
	_, k2, err := s.CreateAccountKey(ctx, adminA, "ci", "second", ScopeMCP, 30)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store("ambit-key-"+k2.ID, true)
	if err := s.DeleteAccount(ctx, adminA, "ci"); !errors.Is(err, ErrDeleteIncomplete) {
		t.Fatalf("delete: %v", err)
	}
	if o, err := s.owner(ctx, k1.Email); err != nil || !o.Deleted {
		t.Fatalf("no tombstone: %+v %v", o, err)
	}
	if _, err := s.getKey(ctx, k1.ID); !apierrors.IsNotFound(err) {
		t.Fatalf("first key survived: %v", err)
	}
	fail.Delete("ambit-key-" + k2.ID)
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getKey(ctx, k2.ID); !apierrors.IsNotFound(err) {
		t.Fatalf("repair left the second key: %v", err)
	}
	ev, _ := s.Events(ctx)
	acts := map[string]int{}
	for _, e := range ev {
		acts[e.Action]++
	}
	if acts["sa-create"] != 1 || acts["sa-delete"] != 1 || acts["sa-delete-key"] != 1 || acts["repair-delete"] != 1 || acts["create"] != 2 {
		t.Fatalf("History: %v", acts)
	}
}

// Guard 3: the person paths never write an owner record in the service account domain, nor over a record of kind
// service-account (live or tombstone, even one planted at a person's name); the service account record stays
// byte-for-byte the same. The service account methods never write outside the domain.
func TestPersonPathsNeverTouchServiceAccountRecords(t *testing.T) {
	s, _ := newKeySvc(t)
	ctx := context.Background()
	mustAccount(t, s, adminA, "ci", "eng")
	mustAccount(t, s, adminA, "gone", "eng")
	if err := s.DeleteAccount(ctx, adminA, "gone"); err != nil {
		t.Fatal(err)
	}
	// a person's record replaced by a service account record (planted through the API by someone with Secret access)
	person := "user05@acme.com"
	if _, _, err := s.Create(ctx, Session{Email: person, SignedIn: t0.Unix()}, "laptop", ScopeLLM, 30); err != nil {
		t.Fatal(err)
	}
	var sec corev1.Secret
	_ = s.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "ambit-key-owner-" + ownerHash(person)}, &sec)
	sec.Data["owner.json"] = []byte(`{"email":"` + person + `","kind":"service-account","groups":["eng"]}`)
	if err := s.Client.Update(ctx, &sec); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"ci@svc.ambit.internal", " CI@SVC.ambit.internal ", "gone@svc.ambit.internal", person} {
		c, _ := identity.CanonicalEmail(email)
		rec := "ambit-key-owner-" + ownerHash(c)
		before := secretBytes(t, s, rec)
		sess := Session{Email: email, Groups: []string{"ambit-admins"}, SignedIn: t0.Add(time.Hour).Unix()}
		if _, _, err := s.Create(ctx, sess, "x", ScopeLLM, 30); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: Create %v", email, err)
		}
		if err := s.Refresh(ctx, sess); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: Refresh %v", email, err)
		}
		if err := s.RefreshAtSignIn(ctx, sess); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: RefreshAtSignIn %v", email, err)
		}
		if _, err := s.OffboardKeys(ctx, email, "scim"); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: OffboardKeys %v", email, err)
		}
		_ = s.Reactivate(ctx, email)
		if after := secretBytes(t, s, rec); !bytes.Equal(before, after) {
			t.Errorf("%s: record changed\n%s\n%s", email, before, after)
		}
	}
	if _, _, err := s.changeOwnerKind(ctx, KindServiceAccount, "user06@acme.com", nil, func(*Owner, bool) (bool, error) { return true, nil }); !errors.Is(err, ErrBadInput) {
		t.Errorf("a service account write outside the domain: %v", err)
	}
	if _, err := s.owner(ctx, "user06@acme.com"); !apierrors.IsNotFound(err) {
		t.Errorf("record written: %v", err)
	}
}

// Section 7.3: the exposure rule.
func TestExposureRule(t *testing.T) {
	s, clk := newKeySvc(t)
	ctx := context.Background()
	k1 := mustAccount(t, s, adminA, "ci", "eng") // issued by Admin@Example.com (canonical admin@example.com)
	_, k2, err := s.CreateAccountKey(ctx, adminB, "ci", "by-b", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	other := mustAccount(t, s, adminB, "other", "ml")
	clk.Set(t0.Add(time.Hour))
	stop := t0.Add(time.Hour).AddDate(0, 0, 7)
	if n, err := s.ExposeIssuedBy(ctx, "admin@example.com", "scim PATCH active=false"); err != nil || n != 1 {
		t.Fatalf("expose: %d %v", n, err)
	}
	if got := keyOf(t, s, k1.ID); !got.Until.Equal(stop) || !got.Ceiling.Equal(stop) || got.Exposed.IsZero() {
		t.Fatalf("exposed key %+v", got)
	}
	for _, k := range []Key{k2, other} {
		if got := keyOf(t, s, k.ID); got.ResourceVersion != k.ResourceVersion {
			t.Fatalf("key of another issuer changed: %+v", got)
		}
	}
	// idempotent, and later calls never raise it
	before := keyOf(t, s, k1.ID).ResourceVersion
	clk.Set(t0.Add(2 * time.Hour))
	if n, err := s.ExposeIssuedBy(ctx, "ADMIN@example.com", "scim"); err != nil || n != 0 || keyOf(t, s, k1.ID).ResourceVersion != before {
		t.Fatalf("second call: %d %v", n, err)
	}
	// survives a repair and a new term
	if _, err := s.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	s.EndTerm()
	if err := s.StartTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keyOf(t, s, k1.ID); !got.Until.Equal(stop) {
		t.Fatalf("after repair and a new term: %v", got.Until)
	}
	// rotation: B offboarded too, now key 2 as well
	if n, err := s.ExposeIssuedBy(ctx, "admin-b@example.com", "scim"); err != nil || n != 2 {
		t.Fatalf("expose B: %d %v", n, err)
	}
	ev, _ := s.Events(ctx)
	if exposed := len(slicesFilter(ev, func(e KeyEvent) bool { return e.Action == "exposed" })); exposed != 3 {
		t.Fatalf("exposed events: %d", exposed)
	}
	// exposureDays 0: stops at once
	s.Services.ExposureDays = 0
	k3 := mustAccount(t, s, adminA, "zero", "eng")
	if _, err := s.ExposeIssuedBy(ctx, "admin@example.com", "scim"); err != nil {
		t.Fatal(err)
	}
	if got := keyOf(t, s, k3.ID); !got.Until.Equal(clk.Now()) {
		t.Fatalf("exposureDays 0: until %v, want now", got.Until)
	}
}

func slicesFilter[T any](xs []T, keep func(T) bool) []T {
	var out []T
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// Section 7.3 race: key creation by an admin and that admin's offboarding (block list, then exposure) in parallel:
// every key created is exposed, or the creation was refused.
func TestExposureRaceWithKeyCreation(t *testing.T) {
	for run := range 50 {
		s, _ := newKeySvc(t)
		ctx := context.Background()
		if _, err := s.CreateAccount(ctx, adminA, "ci", "eng", ""); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var created Key
		var createErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, created, createErr = s.CreateAccountKey(ctx, adminA, "ci", "k", ScopeLLM, 90)
		}()
		go func() {
			defer wg.Done()
			if err := s.Client.Create(ctx, blockList(`["admin@example.com"]`)); err != nil { // SCIM: block first
				t.Error(err)
			}
			if _, err := s.ExposeIssuedBy(ctx, "admin@example.com", "scim"); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		switch {
		case errors.Is(createErr, ErrOffboarded):
		case createErr != nil:
			t.Fatalf("run %d: %v", run, createErr)
		default:
			if got := keyOf(t, s, created.ID); !got.Until.Equal(t0.AddDate(0, 0, 7)) {
				t.Fatalf("run %d: a key created during offboarding is not exposed: %v", run, got.Until)
			}
		}
	}
}

// Concurrency: key creation and account deletion at the same time never leave a live key without a live record.
func TestCreateKeyAndDeleteAccountConcurrently(t *testing.T) {
	for run := range 20 {
		s, _ := newKeySvc(t)
		ctx := context.Background()
		if _, err := s.CreateAccount(ctx, adminA, "ci", "eng", ""); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _, _ = s.CreateAccountKey(ctx, adminA, "ci", "k", ScopeLLM, 90) }()
		go func() { defer wg.Done(); _ = s.DeleteAccount(ctx, adminB, "ci") }()
		wg.Wait()
		if _, err := s.Repair(ctx); err != nil {
			t.Fatal(err)
		}
		if keys, _ := s.List(ctx, AccountIdentity("ci")); len(keys) != 0 {
			t.Fatalf("run %d: a key outlived its account: %+v", run, keys)
		}
	}
}

// Section 14: an older console (0.4.x) reads a service account record as a person who never signed in; its rule
// gives every key an until in the past, so the repair deletes them (fail closed).
func TestDowngradeEndsServiceAccountKeys(t *testing.T) {
	s, _ := newKeySvc(t)
	k := mustAccount(t, s, adminA, "ci", "eng")
	o, err := s.owner(context.Background(), k.Email)
	if err != nil {
		t.Fatal(err)
	}
	old := Owner{Email: o.Email, Groups: o.Groups, SignedIn: o.SignedIn, Offboarded: o.Offboarded} // what 0.4.x decodes
	s.recompute(&k, old, false)
	if k.Until.After(s.now()) {
		t.Fatalf("0.4.x rule keeps the key until %v", k.Until)
	}
}
