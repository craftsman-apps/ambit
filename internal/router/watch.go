package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Watch loads ambit-router's configuration and keys through the API (a mounted file can lag by more than a
// minute, spec fact 13) and keeps them current until ctx ends. It returns after the first load.
func Watch(ctx context.Context, cs kubernetes.Interface, ns string, s *Store) error {
	// One factory per object: each list and watch carries its own name, the only one RBAC allows.
	named := func(name string) informers.SharedInformerFactory {
		return informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = "metadata.name=" + name }))
	}
	cmF, secF := named(routerconfig.ConfigMapName), named(routerconfig.SecretName)
	cms, secs := cmF.Core().V1().ConfigMaps(), secF.Core().V1().Secrets()
	var mu sync.Mutex
	reload := func() {
		mu.Lock()
		defer mu.Unlock()
		keys := map[string]string{}
		if sec, err := secs.Lister().Secrets(ns).Get(routerconfig.SecretName); err == nil {
			for k, v := range sec.Data {
				keys[k] = string(v)
			}
		}
		cm, err := cms.Lister().ConfigMaps(ns).Get(routerconfig.ConfigMapName)
		if apierrors.IsNotFound(err) {
			s.Set(routerconfig.Config{}, keys) // no configuration: every request is stale
			return
		}
		var cfg routerconfig.Config
		if err != nil || json.Unmarshal([]byte(cm.Data[routerconfig.ConfigKey]), &cfg) != nil {
			slog.Warn(routerconfig.ConfigMapName + " does not parse; keeping the last good configuration")
			if cur := s.Snapshot(); cur != nil {
				s.Set(cur.Config, keys) // new keys still apply
			}
			return
		}
		s.Set(cfg, keys)
	}
	h := cache.ResourceEventHandlerFuncs{AddFunc: func(any) { reload() }, UpdateFunc: func(any, any) { reload() }, DeleteFunc: func(any) { reload() }}
	if _, err := cms.Informer().AddEventHandler(h); err != nil {
		return err
	}
	if _, err := secs.Informer().AddEventHandler(h); err != nil {
		return err
	}
	cmF.Start(ctx.Done())
	secF.Start(ctx.Done())
	for _, f := range []informers.SharedInformerFactory{cmF, secF} {
		for typ, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				return fmt.Errorf("watch %v did not sync", typ)
			}
		}
	}
	reload()
	return nil
}
