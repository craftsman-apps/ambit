package router

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func cmObj(cfg routerconfig.Config) *corev1.ConfigMap {
	b, _ := json.Marshal(cfg)
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: routerconfig.ConfigMapName, Namespace: "ambit-system"}, Data: map[string]string{routerconfig.ConfigKey: string(b)}}
}

func secObj(keys map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range keys {
		d[k] = []byte(v)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: routerconfig.SecretName, Namespace: "ambit-system"}, Data: d}
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in 3 s")
}

// countWatches counts the watches a fake client has opened. Watch returns when the lists are cached, before the
// informers open their watches, and the fake replays nothing for that gap (a real API server replays from the
// list's resourceVersion). A test that changes an object right after Watch must wait for the watches first.
func countWatches(cs *fake.Clientset) *atomic.Int32 {
	var n atomic.Int32
	cs.PrependWatchReactor("*", func(k8stesting.Action) (bool, watch.Interface, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

func TestWatchLoadsUpdatesAndSurvivesBadConfig(t *testing.T) {
	cfg := cfgOf(auto)
	cs := fake.NewSimpleClientset(cmObj(cfg), secObj(map[string]string{"local": "sk-1"}))
	watches := countWatches(cs)
	s := &Store{}
	if err := Watch(t.Context(), cs, "ambit-system", s); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return watches.Load() >= 2 }) // the ConfigMap and the Secret
	if s.Snapshot().Config.Hash != cfg.Hash || s.Snapshot().keys["local"] != "sk-1" {
		t.Fatal("initial load")
	}
	if _, err := cs.CoreV1().Secrets("ambit-system").Update(t.Context(), secObj(map[string]string{"local": "sk-2"}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Snapshot().keys["local"] == "sk-2" }) // rotated key, no restart
	bad := cmObj(cfg)
	bad.Data[routerconfig.ConfigKey] = "{not json"
	if _, err := cs.CoreV1().ConfigMaps("ambit-system").Update(t.Context(), bad, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if s.Snapshot().Config.Hash != cfg.Hash {
		t.Fatal("a configuration that does not parse keeps the last good one")
	}
	if err := cs.CoreV1().ConfigMaps("ambit-system").Delete(t.Context(), routerconfig.ConfigMapName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Snapshot().Config.Hash == "" }) // deleted: every request is stale
	// The fake does not filter by field selector, so check that every list and watch names its object: RBAC
	// refuses an unfiltered one.
	for _, a := range cs.Actions() {
		var fs fields.Selector
		switch x := a.(type) {
		case k8stesting.ListAction:
			fs = x.GetListRestrictions().Fields
		case k8stesting.WatchAction:
			fs = x.GetWatchRestrictions().Fields
		default:
			continue
		}
		name := routerconfig.ConfigMapName
		if a.GetResource().Resource == "secrets" {
			name = routerconfig.SecretName
		}
		if v, ok := fs.RequiresExactMatch("metadata.name"); !ok || v != name {
			t.Fatalf("%s %s without metadata.name=%s", a.GetVerb(), a.GetResource().Resource, name)
		}
	}
}

func TestWatchRelistsAfterServerClosesWatch(t *testing.T) {
	cfg := cfgOf(auto)
	cs := fake.NewSimpleClientset(cmObj(cfg), secObj(nil))
	var first atomic.Pointer[watch.FakeWatcher]
	cs.PrependWatchReactor("configmaps", func(k8stesting.Action) (bool, watch.Interface, error) {
		if first.Load() == nil {
			w := watch.NewFake()
			first.Store(w)
			return true, w, nil
		}
		return false, nil, nil // later watches go to the normal fake tracker
	})
	s := &Store{}
	if err := Watch(t.Context(), cs, "ambit-system", s); err != nil {
		t.Fatal(err)
	}
	// Watch returns before the informer opens its watch.
	eventually(t, func() bool { return first.Load() != nil })
	first.Load().Stop() // the API server closed the watch
	changed := auto
	changed.TimeoutMillis = 999
	cfg2 := cfgOf(changed)
	eventually(t, func() bool { // the informer relists and watches again; then the change arrives
		_, _ = cs.CoreV1().ConfigMaps("ambit-system").Update(t.Context(), cmObj(cfg2), metav1.UpdateOptions{})
		return s.Snapshot().Config.Hash == cfg2.Hash
	})
}
