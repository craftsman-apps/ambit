// ambit-console serves the Ambit admin console.
package main

import (
	"cmp"
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
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/oauth"
	"github.com/craftsman-apps/ambit/internal/presets"
	"github.com/craftsman-apps/ambit/internal/pricing"
	"github.com/craftsman-apps/ambit/web"
	"github.com/dexidp/dex/api/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

// envInt reads an integer setting; an unset or invalid value gives def.
func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

// keySettingsFromEnv reads the key settings; values the service cannot work with stop the start.
func keySettingsFromEnv() (console.KeySettings, error) {
	k := console.KeySettings{Enabled: os.Getenv("AMBIT_KEYS_ENABLED") != "false",
		InactivityDays: envInt("AMBIT_KEYS_INACTIVITY_DAYS", 30), MaxDays: envInt("AMBIT_KEYS_MAX_DAYS", 365)}
	if k.InactivityDays < 1 || k.InactivityDays > 365 {
		return k, fmt.Errorf("AMBIT_KEYS_INACTIVITY_DAYS is %d; it must be 1 to 365", k.InactivityDays)
	}
	if k.MaxDays != 30 && k.MaxDays != 90 && k.MaxDays != 365 {
		return k, fmt.Errorf("AMBIT_KEYS_MAX_DAYS is %d; it must be 30, 90 or 365", k.MaxDays)
	}
	return k, nil
}

// serviceSettingsFromEnv reads the serviceAccounts.* values; values the service cannot work with stop the start.
func serviceSettingsFromEnv() (console.ServiceSettings, error) {
	s := console.ServiceSettings{Enabled: os.Getenv("AMBIT_SA_ENABLED") != "false",
		MaxDays: envInt("AMBIT_SA_MAX_DAYS", 90), ExposureDays: envInt("AMBIT_SA_EXPOSURE_DAYS", 7)}
	if s.MaxDays != 30 && s.MaxDays != 90 && s.MaxDays != 365 {
		return s, fmt.Errorf("AMBIT_SA_MAX_DAYS is %d; it must be 30, 90 or 365", s.MaxDays)
	}
	if s.ExposureDays < 0 || s.ExposureDays > 30 {
		return s, fmt.Errorf("AMBIT_SA_EXPOSURE_DAYS is %d; it must be 0 to 30", s.ExposureDays)
	}
	return s, nil
}

// agentSettingsFromEnv reads the agents.* values; values the service cannot work with stop the start.
func agentSettingsFromEnv() (console.AgentSettings, error) {
	a := console.AgentSettings{Enabled: os.Getenv("AMBIT_AGENTS_ENABLED") != "false", MaxTotal: envInt("AMBIT_AGENTS_MAX_TOTAL", 2000),
		MaxPerPerson: envInt("AMBIT_AGENTS_MAX_PER_PERSON", 10), KeyMaxDays: envInt("AMBIT_AGENTS_KEY_MAX_DAYS", 90)}
	if a.MaxTotal < 1 || a.MaxTotal > 2000 {
		return a, fmt.Errorf("AMBIT_AGENTS_MAX_TOTAL is %d; it must be 1 to 2000", a.MaxTotal)
	}
	if a.MaxPerPerson < 1 || a.MaxPerPerson > 10 {
		return a, fmt.Errorf("AMBIT_AGENTS_MAX_PER_PERSON is %d; it must be 1 to 10", a.MaxPerPerson)
	}
	if a.KeyMaxDays != 30 && a.KeyMaxDays != 90 && a.KeyMaxDays != 365 {
		return a, fmt.Errorf("AMBIT_AGENTS_KEY_MAX_DAYS is %d; it must be 30, 90 or 365", a.KeyMaxDays)
	}
	return a, nil
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
	podName := os.Getenv("AMBIT_POD_NAME")
	if podName == "" {
		return errors.New("AMBIT_POD_NAME is required: it names this console in the key Lease")
	}
	keySettings, err := keySettingsFromEnv()
	if err != nil {
		return err
	}
	services, err := serviceSettingsFromEnv()
	if err != nil {
		return err
	}
	agents, err := agentSettingsFromEnv()
	if err != nil {
		return err
	}
	var hydra *oauth.HydraAdmin // set with OAuth sign-in
	keySvc := &console.KeyService{Client: objects, Namespace: ns, Now: time.Now, Settings: keySettings, Services: services, Agents: agents}
	keySvc.RecordKey = console.AgentRecordKey(keys.SessionKey)
	if addr := os.Getenv("AMBIT_OAUTH_ADDR"); addr != "" {
		// The internal API of OAuth sign-in (docs/oauth.md): only ambit-auth's own service account token is accepted.
		api := &console.OAuthAPI{Keys: keySvc, Client: objects, Review: objects, Namespace: ns, Audience: "ambit-console", Subject: console.OAuthSubject(ns)}
		osrv := &http.Server{Addr: addr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := osrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				slog.Error("oauth internal API stopped", "err", err)
			}
		}()
		go func() { <-ctx.Done(); osrv.Close() }()
		slog.Info("OAuth internal API ready", "addr", addr)
		if h := os.Getenv("AMBIT_HYDRA_ADMIN"); h != "" {
			hydra = &oauth.HydraAdmin{Base: h}
		}
	}
	go console.RunLeader(ctx, kube, ns, podName, keySvc.StartTerm, keySvc.EndTerm)
	go func() { // the hourly full repair bounds any stale write or interrupted refresh to one hour
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				switch rep, err := keySvc.Repair(ctx); {
				case errors.Is(err, console.ErrNotLeader):
					slog.Debug("key repair skipped: not the key writer")
				case err != nil:
					slog.Error("key repair", "err", err)
				default:
					slog.Info("key repair", "deleted", rep.Deleted, "updated", rep.Updated)
				}
			}
		}
	}()
	groupStore := &console.GroupStore{Client: objects, Namespace: ns}
	var auth *console.Auth
	if err := retry(ctx, "dex discovery", func() (e error) {
		auth, e = console.NewAuth(ctx, console.AuthConfig{
			Issuer: publicURL + "/dex", InternalIssuer: os.Getenv("AMBIT_DEX_URL"), ClientID: console.ClientID, ClientSecret: keys.ClientSecret,
			PublicURL: publicURL, SessionKey: keys.SessionKey, SessionTTL: time.Duration(hours) * time.Hour,
			OnSignIn: func(c context.Context, sess console.Session) {
				groupStore.RecordAsync(c, sess.Groups)
				go func() { // never make sign-in wait for the key lock; Refresh keeps the newest sign-in, so order does not matter
					switch err := keySvc.RefreshAtSignIn(c, sess); {
					case errors.Is(err, console.ErrNotLeader):
						slog.Info("key refresh skipped: no key writer for 60 s; the next sign-in refreshes the keys", "email", sess.Email)
					case err != nil:
						slog.Error("key refresh at sign-in", "email", sess.Email, "err", err)
					}
				}()
			},
		})
		return e
	}); err != nil {
		return err
	}

	var conns *console.Connections
	var reader *console.ToolReader
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
			reader = &console.ToolReader{Conns: conns, Namespace: ns, List: controller.ListToolsAt, TTL: 10 * time.Minute}
			tsrv := &http.Server{Addr: ":8083", Handler: reader.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := tsrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					slog.Error("tools listener stopped", "err", err)
				}
			}()
			go func() { <-ctx.Done(); tsrv.Close() }()
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
		audit.AgentOwners = keySvc.AgentOwners
		audit.Prices = func(ctx context.Context) map[string]v1.ModelPrice { // by the model name clients send
			var l v1.ModelList
			if objects.List(ctx, &l, client.InNamespace(ns)) != nil {
				return nil
			}
			return pricing.ByModel(l.Items)
		}
		audit.Routers = func(ctx context.Context) []console.RouterInfo { // as they are now, for the Savings baseline
			var rl v1.ModelRouterList
			var ml v1.ModelList
			if err := objects.List(ctx, &rl, client.InNamespace(ns)); err != nil {
				slog.Warn("savings: cannot list ModelRouters; routing is not priced", "err", err)
				return nil
			}
			if err := objects.List(ctx, &ml, client.InNamespace(ns)); err != nil {
				slog.Warn("savings: cannot list Models; routing is not priced", "err", err)
				return nil
			}
			return console.RouterInfos(rl.Items, ml.Items)
		}
		if v := os.Getenv("AMBIT_SAVINGS_REFERENCE_PRICE"); v != "" {
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || p < 0 {
				return fmt.Errorf("AMBIT_SAVINGS_REFERENCE_PRICE %q is not a price in US dollars per million tokens", v)
			}
			audit.ReferenceInputPrice = &p
		}
		days, err := console.SummaryDays(cmp.Or(os.Getenv("AMBIT_SUMMARY_RETENTION"), "26280h"))
		if err != nil {
			return err
		}
		go audit.RunSummaries(ctx, days, 15*time.Second) // retries until the collector has created otel_logs
	}
	var appAudit *console.AppRecorder
	if ep := os.Getenv("AMBIT_OTLP_ENDPOINT"); ep != "" {
		if appAudit, err = console.NewAppRecorder(ep); err != nil {
			return fmt.Errorf("AMBIT_OTLP_ENDPOINT: %w", err)
		}
		defer appAudit.Close() // sends what is queued, within 5 seconds
		// ambit_console_audit_send_failures_total, for the AmbitConsoleAuditSendFailed alert.
		msrv := &http.Server{Addr: ":8084", Handler: promhttp.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := msrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics listener stopped", "err", err)
			}
		}()
		go func() { <-ctx.Done(); msrv.Close() }()
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
		scim = &console.SCIM{Client: objects, Namespace: ns, Token: keys.SCIMToken, Conns: conns, Sessions: auth.Sessions, Keys: keySvc}
	}
	catalogPresets, err := presets.All()
	if err != nil {
		return fmt.Errorf("app presets: %w", err)
	}
	if os.Getenv("AMBIT_TEST_PRESETS") == "1" {
		test, err := presets.LoadTesting()
		if err != nil {
			return fmt.Errorf("test presets: %w", err)
		}
		catalogPresets = append(catalogPresets, test...)
		slog.Warn("test presets enabled")
	}
	prices := &console.PriceSource{URL: os.Getenv("AMBIT_PRICES_URL")}
	go prices.Run(ctx)
	deps := &console.Dependencies{Client: objects, Namespace: ns, Audit: audit, Budgets: budgets,
		RatelimitMode: os.Getenv("AMBIT_RATELIMIT_FAILURE_MODE"), Protection: os.Getenv("AMBIT_PROTECTION") == "true", ModelRouting: os.Getenv("AMBIT_MODEL_ROUTING") == "true",
		XDSAddr: os.Getenv("AMBIT_XDS_ADDR")}
	loginOpts := console.LoginOptions{Bootstrap: bootstrapAdmin, SSO: sso}
	if bootstrapAdmin {
		loginOpts.Namespace = ns
	}
	var evidence *console.EvidenceExports
	if os.Getenv("AMBIT_EVIDENCE_EXPORTS") == "true" {
		maxRows, err := strconv.ParseInt(env("AMBIT_EVIDENCE_MAX_ROWS", "5000000"), 10, 64)
		if err != nil || maxRows < 1 {
			return fmt.Errorf("AMBIT_EVIDENCE_MAX_ROWS %q is not a positive number", os.Getenv("AMBIT_EVIDENCE_MAX_ROWS"))
		}
		maxDays, err := strconv.Atoi(env("AMBIT_EVIDENCE_MAX_DAYS", "366"))
		if err != nil || maxDays < 1 {
			return fmt.Errorf("AMBIT_EVIDENCE_MAX_DAYS %q is not a positive number", os.Getenv("AMBIT_EVIDENCE_MAX_DAYS"))
		}
		evidence = &console.EvidenceExports{MaxRows: maxRows, MaxDays: maxDays, Retention: env("AMBIT_EVIDENCE_RETENTION", "8760h"), Version: os.Getenv("AMBIT_VERSION")}
	}
	srv := &http.Server{Addr: ":8080", Handler: console.NewServer(auth, web.FS(), loginOpts,
		&console.Catalog{Hydra: hydra, Client: objects, Namespace: ns, GitOps: gitops, ArgoApplication: os.Getenv("AMBIT_ARGOCD_APPLICATION"), PublicURL: publicURL, Connectors: sso, Conns: conns, SCIM: scim, Audit: audit, AppAudit: appAudit, Evidence: evidence, Budgets: budgets, Dependencies: deps, AccessMode: os.Getenv("AMBIT_ACCESS_MODE"), Prices: prices, Tools: reader, Presets: catalogPresets,
			Groups: groupStore, Keys: keySvc, LLMURL: os.Getenv("AMBIT_LLM_URL"), MCPURL: os.Getenv("AMBIT_MCP_URL")}), ReadHeaderTimeout: 10 * time.Second}
	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	slog.Info("console ready", "url", publicURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutDone // ListenAndServe returns when Shutdown starts; wait for running requests, so their audit records are queued before the recorder closes
	return nil
}
