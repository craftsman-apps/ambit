// ambit-guard is the MCP guardrail processor: tool pinning (A6) and masking of personal data in tool
// results (U6). agentgateway calls it over gRPC (ExtMcp) for tools/list and tools/call.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/extmcp"
	"github.com/craftsman-apps/ambit/internal/guard"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ns := os.Getenv("AMBIT_NAMESPACE")
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme,
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		slog.Error("manager", "err", err)
		os.Exit(1)
	}
	var masker guard.Masker = guard.Builtin{}
	if a := os.Getenv("AMBIT_PRESIDIO_ANALYZER_URL"); a != "" {
		timeout, err := time.ParseDuration(os.Getenv("AMBIT_PRESIDIO_TIMEOUT")) // empty: the default
		if err != nil && os.Getenv("AMBIT_PRESIDIO_TIMEOUT") != "" {
			slog.Error("AMBIT_PRESIDIO_TIMEOUT", "err", err)
			os.Exit(1)
		}
		masker = guard.BuiltinThenPresidio{Presidio: &guard.Presidio{AnalyzerURL: a, AnonymizerURL: os.Getenv("AMBIT_PRESIDIO_ANONYMIZER_URL"),
			Language: os.Getenv("AMBIT_PRESIDIO_LANGUAGE"), Timeout: timeout}}
		slog.Info("masking with Presidio", "analyzer", a)
	}
	srv := grpc.NewServer(guard.ServerOptions()...)
	extmcp.RegisterExtMcpServer(srv, &guard.Guard{Pins: guard.KubePins{Client: mgr.GetClient(), Namespace: ns}, Masker: masker,
		Rules: guard.KubeRules{Client: mgr.GetClient(), Namespace: ns}})
	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	go func() { _ = mgr.Start(ctx) }()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	go func() {
		_ = http.ListenAndServe(":8081", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mgr.GetCache().WaitForCacheSync(r.Context()) {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	}()
	slog.Info("ambit-guard ready", "namespace", ns)
	if err := srv.Serve(lis); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
