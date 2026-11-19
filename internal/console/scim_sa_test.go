package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// scimWithAccounts is scimFixture with service accounts on (7-day exposure).
func scimWithAccounts(t *testing.T) (*http.ServeMux, *SCIM, client.Client) {
	t.Helper()
	mux, s, _, c := scimFixture(t)
	s.Keys.Services = ServiceSettings{Enabled: true, MaxDays: 90, ExposureDays: 7}
	return mux, s, c
}

func scimUser(email string, active bool) map[string]any {
	return map[string]any{"schemas": []string{scimUserSchema}, "userName": email, "active": active,
		"emails": []any{map[string]any{"value": email, "primary": true}}}
}

// ownerRecords returns every owner record's version and content, to prove nothing changed.
func ownerRecords(t *testing.T, c client.Client) string {
	t.Helper()
	var l corev1.SecretList
	if err := c.List(t.Context(), &l, client.InNamespace(testNS), client.MatchingLabels{ownerRecordLabel: "true"}); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, s := range l.Items {
		b.WriteString(s.Name + "@" + s.ResourceVersion + ":" + string(s.Data["owner.json"]) + "\n")
	}
	return b.String()
}

// Guard 4: SCIM cannot create a user in the service account domain, nor change one to it; a user stored with such
// an address before the upgrade (planted here) cannot offboard, block or reactivate anything.
func TestSCIMRefusesTheServiceDomain(t *testing.T) {
	mux, s, c := scimWithAccounts(t)
	ctx := context.Background()
	if _, err := s.Keys.CreateAccount(ctx, adminA, "ci-test", "eng", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Keys.CreateAccountKey(ctx, adminA, "ci-test", "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	for _, active := range []bool{true, false} {
		for _, email := range []string{"ci-test@svc.ambit.internal", " CI-Test@SVC.ambit.internal "} {
			if r := scimDo(t, mux, "POST", "/scim/v2/Users", scimToken, scimUser(email, active)); r.Code != http.StatusBadRequest {
				t.Errorf("POST %q active=%v: %d %s", email, active, r.Code, r.Body)
			}
		}
	}
	id := provision(t, mux, "user03@example.com")
	if r := scimDo(t, mux, "PUT", "/scim/v2/Users/"+id, scimToken, scimUser("ci-test@svc.ambit.internal", true)); r.Code != http.StatusBadRequest {
		t.Errorf("PUT to a reserved email: %d", r.Code)
	}
	// a planted user with a reserved address
	planted := &SCIMUser{ID: "0123456789abcdef0123456789abcdef", UserName: "ci-test", Email: "ci-test@svc.ambit.internal", Active: true}
	if err := s.save(ctx, planted); err != nil {
		t.Fatal(err)
	}
	s.Sessions.Add("svc", "ci-test@svc.ambit.internal", time.Now().Add(time.Hour).Unix())
	beforeBlock, beforeOwners := blocked(t, c), ownerRecords(t, c)
	keysBefore, _ := s.Keys.ListAll(ctx)
	for _, step := range []struct {
		method string
		body   any
	}{
		{"PATCH", map[string]any{"Operations": []any{map[string]any{"op": "replace", "path": "active", "value": false}}}},
		{"PATCH", map[string]any{"Operations": []any{map[string]any{"op": "replace", "path": "active", "value": true}}}},
		{"PUT", scimUser("user04@example.com", true)}, // email change away from the reserved address
		{"DELETE", nil},
	} {
		// the PUT moves the stored user to user04; put it back so every step starts from the reserved address
		_ = s.save(ctx, planted)
		r := scimDo(t, mux, step.method, "/scim/v2/Users/"+planted.ID, scimToken, step.body)
		if r.Code >= 500 {
			t.Errorf("%s: %d %s", step.method, r.Code, r.Body)
		}
		if b, o := blocked(t, c), ownerRecords(t, c); b != beforeBlock || o != beforeOwners {
			t.Errorf("%s changed the block list (%s) or owner records:\n%s\n%s", step.method, b, beforeOwners, o)
		}
		if !s.Sessions.Valid("svc") {
			t.Errorf("%s ended a session of the reserved address", step.method)
		}
		if keys, _ := s.Keys.ListAll(ctx); len(keys) != len(keysBefore) || keys[0].ResourceVersion != keysBefore[0].ResourceVersion {
			t.Errorf("%s changed the service account key", step.method)
		}
	}
}

// Section 7.3: deactivating a person in the account's group, or the admin who created the account but issued no
// live key, changes nothing for the account; deactivating the issuer exposes exactly the keys they issued.
func TestSCIMOffboardingAndServiceAccounts(t *testing.T) {
	mux, s, _ := scimWithAccounts(t)
	ctx := context.Background()
	creator := Session{Email: "creator@example.com", Role: RoleAdmin}
	issuer := Session{Email: "Issuer@Example.com", Role: RoleAdmin} // compared in canonical form
	if _, err := s.Keys.CreateAccount(ctx, creator, "ci", "eng", ""); err != nil {
		t.Fatal(err)
	}
	_, k1, err := s.Keys.CreateAccountKey(ctx, issuer, "ci", "k1", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	_, k2, err := s.Keys.CreateAccountKey(ctx, adminB, "ci", "k2", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	same := func(k Key) bool {
		got, err := s.Keys.getKey(ctx, k.ID)
		return err == nil && got.ResourceVersion == k.ResourceVersion
	}
	// a member of eng with a personal key
	member := provision(t, mux, "member@example.com")
	if _, _, err := s.Keys.Create(ctx, Session{Email: "member@example.com", Groups: []string{"eng"}, SignedIn: time.Now().Unix()}, "laptop", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	if r := patchActive(t, mux, member, false); r.Code != 200 {
		t.Fatalf("deactivate member: %d", r.Code)
	}
	if keys, _ := s.Keys.List(ctx, "member@example.com"); len(keys) != 0 || !same(k1) || !same(k2) {
		t.Fatalf("member offboarding: their keys %d, service account keys unchanged %v %v", len(keys), same(k1), same(k2))
	}
	// the creator issued no live key
	cid := provision(t, mux, "creator@example.com")
	if r := patchActive(t, mux, cid, false); r.Code != 200 || !same(k1) || !same(k2) {
		t.Fatalf("creator offboarding: %d, keys unchanged %v %v", r.Code, same(k1), same(k2))
	}
	if a, _ := s.Keys.Accounts(ctx); len(a) != 1 {
		t.Fatal("the account is gone")
	}
	// the issuer: key 1 stops within 7 days, key 2 is unchanged
	iid := provision(t, mux, "issuer@example.com")
	if r := patchActive(t, mux, iid, false); r.Code != 200 {
		t.Fatalf("issuer offboarding: %d %s", r.Code, r.Body)
	}
	got, _ := s.Keys.getKey(ctx, k1.ID)
	if d := time.Until(got.Until); d > 7*24*time.Hour || d < 7*24*time.Hour-time.Minute || !same(k2) {
		t.Fatalf("issuer offboarding: key 1 until %v, key 2 unchanged %v", got.Until, same(k2))
	}
	recs, _ := s.Records(ctx)
	if recs[0].Email != "issuer@example.com" || recs[0].KeysExposed != 1 {
		t.Fatalf("record %+v", recs[0])
	}
	// reactivation does not raise it
	if r := patchActive(t, mux, iid, true); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if again, _ := s.Keys.getKey(ctx, k1.ID); !again.Until.Equal(got.Until) {
		t.Fatalf("reactivation moved until to %v", again.Until)
	}
}

// Section 7.3: a SCIM email change treats the old email as offboarded for exposure.
func TestSCIMEmailChangeExposes(t *testing.T) {
	mux, s, _ := scimWithAccounts(t)
	ctx := context.Background()
	if _, err := s.Keys.CreateAccount(ctx, adminA, "ci", "eng", ""); err != nil {
		t.Fatal(err)
	}
	_, k, err := s.Keys.CreateAccountKey(ctx, Session{Email: "old@example.com"}, "ci", "k", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	id := provision(t, mux, "old@example.com")
	if r := scimDo(t, mux, "PUT", "/scim/v2/Users/"+id, scimToken, scimUser("new@example.com", true)); r.Code != 200 {
		t.Fatalf("PUT: %d %s", r.Code, r.Body)
	}
	if got, _ := s.Keys.getKey(ctx, k.ID); time.Until(got.Until) > 7*24*time.Hour {
		t.Fatalf("not exposed: %v", got.Until)
	}
}

// Section 7.3: when the exposure write fails, SCIM answers 503 with Retry-After: 30 and the retry completes it.
func TestSCIMExposureFailureIsRetried(t *testing.T) {
	failing := true
	mux, s, _, c := scimFixture(t)
	fc := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if sec, ok := obj.(*corev1.Secret); ok && failing && sec.Annotations[annPrefix+"issued-by"] != "" {
			return apierrors.NewServiceUnavailable("etcd hiccup")
		}
		return cl.Update(ctx, obj, opts...)
	}})
	s.Keys.Client = fc
	s.Keys.Services = ServiceSettings{Enabled: true, MaxDays: 90, ExposureDays: 7}
	ctx := context.Background()
	if _, err := s.Keys.CreateAccount(ctx, adminA, "ci", "eng", ""); err != nil {
		t.Fatal(err)
	}
	_, k, err := s.Keys.CreateAccountKey(ctx, Session{Email: "leaver@example.com"}, "ci", "k", ScopeLLM, 90)
	if err != nil {
		t.Fatal(err)
	}
	id := provision(t, mux, "leaver@example.com")
	r := patchActive(t, mux, id, false)
	if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") != "30" {
		t.Fatalf("failed exposure: %d %q", r.Code, r.Header().Get("Retry-After"))
	}
	if !userActive(t, s, id) {
		t.Fatal("the user was saved as inactive: the identity provider would not retry")
	}
	failing = false
	if r := patchActive(t, mux, id, false); r.Code != 200 {
		t.Fatalf("retry: %d", r.Code)
	}
	if got, _ := s.Keys.getKey(ctx, k.ID); time.Until(got.Until) > 7*24*time.Hour {
		t.Fatalf("not exposed after the retry: %v", got.Until)
	}
}
