package console

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func groupScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newGroupStore(t *testing.T, day string, fns ...interceptor.Funcs) (*GroupStore, client.Client) {
	b := fake.NewClientBuilder().WithScheme(groupScheme(t))
	for _, f := range fns {
		b = b.WithInterceptorFuncs(f)
	}
	c := b.Build()
	d, _ := time.Parse(groupDay, day)
	return &GroupStore{Client: c, Namespace: testNS, Now: func() time.Time { return d.Add(23 * time.Hour) }}, c
}

func storedGroups(t *testing.T, c client.Client) []knownGroup {
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: groupsConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	return parseKnownGroups(cm.Data["groups"])
}

func TestSignInRecordsNewGroupsOnce(t *testing.T) {
	writes := 0
	g, c := newGroupStore(t, "2026-10-18", interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			writes++
			return cl.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			writes++
			return cl.Update(ctx, o, opts...)
		},
	})
	for range 2 {
		if err := g.Record(t.Context(), []string{"eng", "ml-research", "bad name;"}); err != nil {
			t.Fatal(err)
		}
	}
	got := storedGroups(t, c)
	if writes != 1 || len(got) != 2 || got[0] != (knownGroup{"eng", "2026-10-18"}) || got[1].Name != "ml-research" {
		t.Fatalf("writes=%d groups=%+v", writes, got)
	}
	// A new UTC day updates the date; an empty claim writes nothing.
	g.Now = func() time.Time { return time.Date(2026, 10, 19, 0, 0, 1, 0, time.UTC) }
	g.Record(t.Context(), []string{"eng"})
	g.Record(t.Context(), nil)
	got = storedGroups(t, c)
	if writes != 2 || got[0].LastSeen != "2026-10-19" || got[1].LastSeen != "2026-10-18" {
		t.Fatalf("writes=%d groups=%+v", writes, got)
	}
}

func TestSignInKeepsThe1000NewestGroups(t *testing.T) {
	g, c := newGroupStore(t, "2026-10-01")
	var old []string
	for i := range maxKnownGroups {
		old = append(old, fmt.Sprintf("old-%04d", i))
	}
	g.Record(t.Context(), old)
	g.Now = func() time.Time { return time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC) }
	g.Record(t.Context(), []string{"fresh"})
	got := storedGroups(t, c)
	if len(got) != maxKnownGroups {
		t.Fatalf("got %d names", len(got))
	}
	has := map[string]bool{}
	for _, k := range got {
		has[k.Name] = true
	}
	// All old names share one date; the tie is broken by name, so the last name is dropped.
	if !has["fresh"] || has["old-0999"] || !has["old-0000"] {
		t.Fatalf("wrong names kept: fresh=%v old-0999=%v old-0000=%v", has["fresh"], has["old-0999"], has["old-0000"])
	}
}

func TestSignInRetriesAConflictThreeTimes(t *testing.T) {
	for _, tc := range []struct {
		fails   int
		wantErr bool
	}{{2, false}, {3, true}} {
		fails := tc.fails
		g, c := newGroupStore(t, "2026-10-18", interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if fails > 0 {
					fails--
					return apierrors.NewConflict(corev1.Resource("configmaps"), groupsConfigMap, fmt.Errorf("busy"))
				}
				return cl.Create(ctx, o, opts...)
			},
		})
		err := g.Record(t.Context(), []string{"eng"})
		if (err != nil) != tc.wantErr {
			t.Fatalf("fails=%d: err=%v", tc.fails, err)
		}
		if !tc.wantErr && len(storedGroups(t, c)) != 1 {
			t.Fatalf("fails=%d: not stored", tc.fails)
		}
	}
}

func TestListGroupsIsAdminOnlyAndMergesSources(t *testing.T) {
	s := groupScheme(t)
	rule := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: testNS},
		Spec: v1.AccessRuleSpec{Groups: []string{"eng", "finance"}, Models: []string{"m"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rule).Build()
	store := &GroupStore{Client: c, Namespace: testNS}
	store.Record(t.Context(), []string{"eng", "ops"})
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	h := NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, Groups: store})
	if r := do(t, h, "GET", "/api/groups", nil, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", r.Code)
	}
	for _, role := range []string{RoleViewer, RoleApprover} {
		if r := do(t, h, "GET", "/api/groups", as(auth, role), nil); r.Code != http.StatusForbidden {
			t.Errorf("%s: %d", role, r.Code)
		}
	}
	r := do(t, h, "GET", "/api/groups", as(auth, RoleAdmin), nil)
	got := decodeAs[[]GroupView](t, r.Body.String())
	want := []GroupView{{"eng", "both"}, {"finance", "rules"}, {"ops", "sign-in"}}
	if r.Code != 200 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%d %+v, want %+v", r.Code, got, want)
	}
}
