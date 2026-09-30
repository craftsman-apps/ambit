// ambit-controller turns Ambit objects into agentgateway configuration.
package main

import (
	"flag"
	"os"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ns := os.Getenv("AMBIT_NAMESPACE")
	route := flag.String("llm-route", "ambit-llm", "HTTPRoute that every model attaches to")
	flag.Parse()
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("setup")
	if ns == "" {
		log.Info("AMBIT_NAMESPACE is required")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	// ponytail: one replica, no leader election; add it before running two controllers.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	if err := (&controller.Reconciler{Client: mgr.GetClient(), Live: mgr.GetAPIReader(), ListTools: controller.ListTools, Namespace: ns, Route: controller.RouteRef{Name: *route}}).SetupWithManager(mgr); err != nil {
		log.Error(err, "set up controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	log.Info("starting", "namespace", ns, "llmRoute", *route)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}
