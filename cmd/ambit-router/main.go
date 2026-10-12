// ambit-router labels chat prompts for model routers of type LLM. agentgateway calls it as an ExtProc (gRPC, port
// 9000) for chat completions while a router needs it; it answers with a category header, never an error.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/craftsman-apps/ambit/internal/router"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	concurrency := flag.Int("concurrency", 16, "classifications at once; over it requests get no label (busy)")
	drain := flag.Duration("drain", 25*time.Second, "on SIGTERM, how long held requests may finish")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(*concurrency, *drain); err != nil {
		slog.Error("ambit-router", "err", err)
		os.Exit(1)
	}
}

func run(concurrency int, drain time.Duration) error {
	ns := os.Getenv("AMBIT_NAMESPACE")
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	store := &router.Store{}
	if err := router.Watch(ctx, cs, ns, store); err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	h := &router.Handler{Store: store, Classifier: &router.LLM{HTTP: &http.Client{}}, Concurrency: concurrency, Metrics: router.NewMetrics(reg)}
	srv := router.NewServer(h, drain)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !srv.Ready(store) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) { // the loaded hash, for support
		if snap := store.Snapshot(); snap != nil {
			fmt.Fprintln(w, snap.Config.Hash)
		}
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	go func() { _ = http.ListenAndServe(":8081", mux) }()
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		return err
	}
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		slog.Info("SIGTERM: finishing held requests", "drain", drain.String())
		srv.Shutdown()
		close(stopped)
	}()
	slog.Info("ambit-router ready", "namespace", ns)
	if err := srv.Serve(lis); err != nil {
		return err
	}
	<-stopped // Serve returns when the listener closes; held requests may still be finishing
	return nil
}
