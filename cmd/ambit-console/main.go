// ambit-console serves the Ambit admin console.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/console"
	"github.com/craftsman-apps/ambit/internal/pricing"
	"github.com/craftsman-apps/ambit/web"
	"github.com/dexidp/dex/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// gitopsFromEnv reads GitOps mode settings rendered by the chart; nil means Direct mode.
func gitopsFromEnv() (*console.GitOps, error) {
	repo := os.Getenv("AMBIT_GITOPS_REPOSITORY")
	if repo == "" {
		return nil, nil
	}
	keyPEM, err := os.ReadFile(env("AMBIT_GITHUB_APP_KEY_FILE", "/etc/ambit/github/private-key.pem"))
	if err != nil {
		return nil, fmt.Errorf("GitOps mode needs the GitHub App private key: %w", err)
	}
	key, err := console.ParseAppKey(keyPEM)
	if err != nil {
		return nil, err
	}
	branch := env("AMBIT_GITOPS_BRANCH", "main")
	gh := &console.GitHub{API: env("AMBIT_GITHUB_API", "https://api.github.com"), Repository: repo, Branch: branch,
		AppID: os.Getenv("AMBIT_GITHUB_APP_ID"), Key: key}
	if gh.AppID == "" {
		return nil, errors.New("GitOps mode needs AMBIT_GITHUB_APP_ID")
	}
	return &console.GitOps{Repo: gh, Repository: repo, Branch: branch, Dir: env("AMBIT_GITOPS_PATH", "ambit")}, nil
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
	hours, err := strconv.Atoi(env("AMBIT_SESSION_HOURS", "1"))
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
	scheme := runtime.NewScheme()
	// Core types for the Secrets the Connect SSO page creates; Ambit types for the catalog.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1.AddToScheme(scheme); err != nil {
		return err
	}
	objects, err := client.New(cfg, client.Options{Scheme: scheme})
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

	var conns *console.Connections
	if gns := os.Getenv("AMBIT_GRANTS_NAMESPACE"); gns != "" {
		conns = &console.Connections{Client: objects, Namespace: ns, PublicURL: publicURL, Codec: console.Codec{Key: keys.SessionKey},
			Store: &console.GrantStore{Client: objects, Namespace: gns, Key: keys.GrantKey}}
		go conns.RunChecks(ctx)
		{
			broker := &console.Broker{Conns: conns}
			bsrv := &http.Server{Addr: ":8082", Handler: broker.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := bsrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					slog.Error("token broker stopped", "err", err)
				}
			}()
			go func() { <-ctx.Done(); bsrv.Close() }()
			slog.Info("token broker ready", "grants", gns)
		}
	}
	gitops, err := gitopsFromEnv()
	if err != nil {
		return err
	}
	if gitops != nil {
		slog.Info("GitOps mode: changes become pull requests", "repository", gitops.Repository, "branch", gitops.Branch, "path", gitops.Dir)
	}
	var audit *console.Audit
	if addr := os.Getenv("AMBIT_CLICKHOUSE_ADDR"); addr != "" {
		// An external ClickHouse brings its own password (AMBIT_CLICKHOUSE_PASSWORD, from the customer's
		// Secret); the chart's own gets one the console creates once.
		pw, external := os.LookupEnv("AMBIT_CLICKHOUSE_PASSWORD") // set (even empty) only for an external one
		if !external {
			var err error
			if pw, err = console.EnsureClickHousePassword(ctx, kube, ns); err != nil {
				return err
			}
		}
		tlsc, err := console.TLSConfig(os.Getenv("AMBIT_CLICKHOUSE_TLS") == "true", os.Getenv("AMBIT_CLICKHOUSE_CA"))
		if err != nil {
			return fmt.Errorf("ClickHouse TLS: %w", err)
		}
		audit = console.OpenAudit(console.ClickHouse{Addr: addr, User: os.Getenv("AMBIT_CLICKHOUSE_USER"),
			Database: os.Getenv("AMBIT_CLICKHOUSE_DATABASE"), Password: pw, TLS: tlsc})
		audit.Prices = func(ctx context.Context) map[string]v1.ModelPrice { // by the model name clients send
			var l v1.ModelList
			if objects.List(ctx, &l, client.InNamespace(ns)) != nil {
				return nil
			}
			return pricing.ByModel(l.Items)
		}
	}
	var budgets *console.Budgets
	if addr := os.Getenv("AMBIT_VALKEY_ADDR"); addr != "" {
		tlsc, err := console.TLSConfig(os.Getenv("AMBIT_VALKEY_TLS") == "true", os.Getenv("AMBIT_VALKEY_CA"))
		if err != nil {
			return fmt.Errorf("Valkey TLS: %w", err)
		}
		budgets = &console.Budgets{Client: objects, Namespace: ns,
			Valkey: console.Valkey{Addr: addr, Password: os.Getenv("AMBIT_VALKEY_PASSWORD"), TLS: tlsc}}
	}
	var scim *console.SCIM
	if conns != nil {
		scim = &console.SCIM{Client: objects, Namespace: ns, Token: keys.SCIMToken, Conns: conns, Sessions: auth.Sessions}
	}
	prices := &console.PriceSource{URL: os.Getenv("AMBIT_PRICES_URL")}
	go prices.Run(ctx)
	deps := &console.Dependencies{Client: objects, Namespace: ns, Audit: audit, Budgets: budgets,
		RatelimitMode: os.Getenv("AMBIT_RATELIMIT_FAILURE_MODE"), Protection: os.Getenv("AMBIT_PROTECTION") == "true",
		XDSAddr: os.Getenv("AMBIT_XDS_ADDR")}
	srv := &http.Server{Addr: ":8080", Handler: console.NewServer(auth, web.FS(), console.LoginOptions{Bootstrap: bootstrapAdmin, SSO: sso},
		&console.Catalog{Client: objects, Namespace: ns, GitOps: gitops, ArgoApplication: os.Getenv("AMBIT_ARGOCD_APPLICATION"), PublicURL: publicURL, Connectors: sso, Conns: conns, SCIM: scim, Audit: audit, Budgets: budgets, Dependencies: deps, Prices: prices}), ReadHeaderTimeout: 10 * time.Second}
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
