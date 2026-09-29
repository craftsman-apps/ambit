// ambit-console serves the Ambit admin console.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/craftsman-apps/ambit/internal/console"
	"github.com/craftsman-apps/ambit/web"
	"github.com/dexidp/dex/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// retry runs f until it succeeds or ctx ends. Dex may still be starting when the console starts.
func retry(ctx context.Context, what string, f func() error) error {
	for delay := time.Second; ; delay = min(delay*2, 15*time.Second) {
		err := f()
		if err == nil {
			return nil
		}
		slog.Warn("waiting", "for", what, "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("console stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	publicURL := env("AMBIT_PUBLIC_URL", "http://console.localtest.me:8080")
	ns := env("AMBIT_NAMESPACE", "ambit-system")
	hours, err := strconv.Atoi(env("AMBIT_SESSION_HOURS", "8"))
	if err != nil || hours < 1 {
		return errors.New("AMBIT_SESSION_HOURS must be a positive whole number")
	}

	bootstrapAdmin := env("AMBIT_BOOTSTRAP_ENABLED", "true") == "true"
	sso, err := console.ParseConnectors(os.Getenv("AMBIT_SSO_CONNECTORS"))
	if err != nil {
		return err
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	// ponytail: plaintext gRPC inside the cluster, limited by NetworkPolicy; add mTLS for EKS.
	conn, err := grpc.NewClient(env("AMBIT_DEX_GRPC", "ambit-dex:5557"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	var keys console.Keys
	if err := retry(ctx, "bootstrap", func() (e error) {
		keys, e = console.Bootstrap(ctx, kube, api.NewDexClient(conn), ns, publicURL, bootstrapAdmin)
		return e
	}); err != nil {
		return err
	}
	var auth *console.Auth
	if err := retry(ctx, "dex discovery", func() (e error) {
		auth, e = console.NewAuth(ctx, console.AuthConfig{
			Issuer: publicURL + "/dex", ClientID: console.ClientID, ClientSecret: keys.ClientSecret,
			PublicURL: publicURL, SessionKey: keys.SessionKey, SessionTTL: time.Duration(hours) * time.Hour,
		})
		return e
	}); err != nil {
		return err
	}

	srv := &http.Server{Addr: ":8080", Handler: console.NewServer(auth, web.FS(), console.LoginOptions{Bootstrap: bootstrapAdmin, SSO: sso}), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	slog.Info("console ready", "url", publicURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
