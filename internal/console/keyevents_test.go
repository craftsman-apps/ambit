package console

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// pagedEvents makes the fake client page lists the way the API server does (the fake ignores Limit). It fails
// the test when key events are listed without a limit, and counts full ConfigMap lists of key events.
func pagedEvents(t *testing.T, fullLists *int) interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		o := (&client.ListOptions{}).ApplyOptions(opts)
		events := o.LabelSelector != nil && strings.Contains(o.LabelSelector.String(), keyEventLabel)
		if !events {
			return c.List(ctx, l, opts...)
		}
		if o.Limit == 0 {
			t.Errorf("key events listed without a limit (%T)", l)
		}
		if _, ok := l.(*corev1.ConfigMapList); ok {
			*fullLists++
		}
		if err := c.List(ctx, l, opts...); err != nil || o.Limit == 0 {
			return err
		}
		items, err := meta.ExtractList(l)
		if err != nil {
			return err
		}
		sort.Slice(items, func(i, j int) bool {
			a, _ := meta.Accessor(items[i])
			b, _ := meta.Accessor(items[j])
			return a.GetName() < b.GetName()
		})
		start, _ := strconv.Atoi(o.Continue)
		end := min(start+int(o.Limit), len(items))
		if err := meta.SetList(l, slices.Clone(items[start:end])); err != nil {
			return err
		}
		next := ""
		if end < len(items) {
			next = strconv.Itoa(end)
		}
		l.(metav1.ListInterface).SetContinue(next)
		return nil
	}}
}

func smallEventPages(t *testing.T, page, history int) {
	oldPage, oldHistory := eventPage, historyMax
	eventPage, historyMax = int64(page), history
	t.Cleanup(func() { eventPage, historyMax = oldPage, oldHistory })
}

func TestRefreshRecordsAnEventOnlyWhenGroupsChange(t *testing.T) {
	logs := captureLogs(t)
	s, _ := newKeySvc(t)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, alice("eng"), "k", ScopeLLM, 90); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"eng"}, SignedIn: t0.Unix() + 10}); err != nil {
		t.Fatal(err)
	}
	if ev, _ := keyChanges(s); len(ev) != 1 || ev[0].Action != "create" {
		t.Fatalf("a refresh with the same groups was recorded: %+v", ev)
	}
	if !strings.Contains(logs.String(), "action=refresh") {
		t.Fatalf("refresh not logged: %s", logs.String())
	}
	if keys, _ := s.List(ctx, "alice@acme.com"); len(keys) != 1 || !keys[0].SignedIn.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("keys not refreshed: %+v", keys)
	}
	if err := s.Refresh(ctx, Session{Email: "alice@acme.com", Groups: []string{"ops"}, SignedIn: t0.Unix() + 20}); err != nil {
		t.Fatal(err)
	}
	if ev, _ := keyChanges(s); len(ev) != 2 || ev[0].Action != "refresh" || ev[0].Detail != "keys=1" {
		t.Fatalf("a refresh that changed groups was not recorded: %+v", ev)
	}
}

func TestEventListsArePaged(t *testing.T) {
	smallEventPages(t, 3, 5)
	full := 0
	s, clk := newKeySvcWith(t, pagedEvents(t, &full)) // records settings-changed first
	ctx := context.Background()
	for i := 0; i < 8; i++ { // 8 create events after the settings record: it is on the first page of three
		clk.Set(t0.Add(time.Duration(i+1) * time.Minute))
		if _, _, err := s.Create(ctx, Session{Email: "u" + strconv.Itoa(i) + "@acme.com", SignedIn: t0.Unix()}, "k", ScopeLLM, 30); err != nil {
			t.Fatal(err)
		}
	}
	full = 0
	if err := s.StartTerm(ctx); err != nil { // same settings: found across pages, not recorded again
		t.Fatal(err)
	}
	if full != 0 {
		t.Fatalf("StartTerm or its repair listed whole event objects (%d lists)", full)
	}
	ev, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 5 || ev[0].Email != "u7@acme.com" || ev[4].Email != "u3@acme.com" {
		t.Fatalf("history is not the newest 5, newest first: %+v", ev)
	}
	var all corev1.ConfigMapList
	_ = s.Client.List(ctx, &all, client.MatchingLabels{keyEventLabel: "true"}, client.Limit(1000))
	settings := 0
	for _, cm := range all.Items {
		if cm.Data["action"] == "settings-changed" {
			settings++
		}
	}
	if len(all.Items) != 9 || settings != 1 {
		t.Fatalf("settings recorded again: %d events, %d settings records", len(all.Items), settings)
	}
	// Retention across pages: everything but the newest event is past 400 days.
	clk.Set(t0.Add(7*time.Minute + 30*time.Second + eventRetention))
	if err := s.cleanupEvents(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Client.List(ctx, &all, client.MatchingLabels{keyEventLabel: "true"}, client.Limit(1000))
	if len(all.Items) != 1 || all.Items[0].Data["email"] != "u7@acme.com" {
		t.Fatalf("cleanup did not reach every page: %d left", len(all.Items))
	}
}
