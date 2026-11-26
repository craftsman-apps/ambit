package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/craftsman-apps/ambit/internal/oauth"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func env(k string) string { return strings.TrimSpace(os.Getenv(k)) }

func envInt(k string, def int) (int, error) {
	if env(k) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(env(k))
	if err != nil {
		return 0, fmt.Errorf("%s is not a number", k)
	}
	return n, nil
}

// serve runs the sign-in host (port 8080), Hydra's token hook (8081) and health and metrics (8082).
func serve(ctx context.Context) error {
	cfg := oauth.Config{PublicURL: env("AMBIT_AUTH_PUBLIC_URL"), Resource: env("AMBIT_AUTH_RESOURCE"), IdPIssuer: env("AMBIT_AUTH_IDP_ISSUER"),
		IdPClientID: env("AMBIT_AUTH_IDP_CLIENT_ID"), IdPClientSecret: env("AMBIT_AUTH_IDP_CLIENT_SECRET"), GroupsClaim: env("AMBIT_AUTH_GROUPS_CLAIM")}
	for _, h := range strings.Split(env("AMBIT_AUTH_REGISTRATION_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			cfg.RegistrationHosts = append(cfg.RegistrationHosts, h)
		}
	}
	cfg.NoRegistration = env("AMBIT_AUTH_REGISTRATION") == "off"
	var err error
	if cfg.MaxClients, err = envInt("AMBIT_AUTH_MAX_CLIENTS", 500); err != nil {
		return err
	}
	if cfg.UnusedClientDays, err = envInt("AMBIT_AUTH_UNUSED_CLIENT_DAYS", 30); err != nil {
		return err
	}
	if cfg.NeverUsedHours, err = envInt("AMBIT_AUTH_NEVER_USED_CLIENT_HOURS", 24); err != nil {
		return err
	}
	if cfg.MaxGrantDays, err = envInt("AMBIT_AUTH_MAX_GRANT_DAYS", 30); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	sealer, err := oauth.NewSealer([]byte(env("AMBIT_AUTH_COOKIE_SECRET")))
	if err != nil {
		return err
	}
	if env("AMBIT_AUTH_HYDRA_ADMIN") == "" || env("AMBIT_AUTH_HYDRA_PUBLIC") == "" || env("AMBIT_AUTH_CONSOLE_URL") == "" {
		return errors.New("AMBIT_AUTH_HYDRA_ADMIN, AMBIT_AUTH_HYDRA_PUBLIC and AMBIT_AUTH_CONSOLE_URL are needed")
	}

	vopt := &redis.Options{Addr: env("AMBIT_AUTH_VALKEY_ADDRESS"), Password: env("AMBIT_AUTH_VALKEY_PASSWORD"),
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	if env("AMBIT_AUTH_VALKEY_TLS") == "true" {
		vopt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if ca := env("AMBIT_AUTH_VALKEY_CA"); ca != "" {
			pem, err := os.ReadFile(ca)
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			vopt.TLSConfig.RootCAs = pool
		}
	}
	vc := redis.NewClient(vopt)
	defer vc.Close()

	rc, err := rest.InClusterConfig()
	if err != nil { // outside a cluster (a developer's machine): the usual KUBECONFIG
		if rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig(); err != nil {
			return fmt.Errorf("no Kubernetes configuration: %w", err)
		}
	}
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	ns := env("AMBIT_NAMESPACE")
	deny := &oauth.ConfigDeny{MaxAge: 45 * time.Second, Get: func(ctx context.Context, name string) (map[string]string, error) {
		cm, err := kc.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil // no list is an empty list
		}
		if err != nil {
			return nil, err
		}
		return cm.Data, nil
	}}
	go deny.Run(ctx, 3*time.Second, func(msg string, err error) { slog.Warn(msg, "err", err) })

	idpCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	var idp *oauth.OIDC
	for { // the identity provider may be reachable a little after the pod starts
		if idp, err = oauth.NewOIDC(idpCtx, cfg); err == nil {
			break
		}
		slog.Warn("waiting for the identity provider", "err", err)
		select {
		case <-idpCtx.Done():
			cancel()
			return err
		case <-time.After(5 * time.Second):
		}
	}
	cancel()

	rates := oauth.DefaultRates()
	perHour, err := envInt("AMBIT_AUTH_REGISTER_PER_HOUR", 10)
	if err != nil || perHour < 1 {
		return errors.New("AMBIT_AUTH_REGISTER_PER_HOUR must be 1 or more")
	}
	rates[oauth.BucketRegister] = oauth.Rate{Limit: perHour, Window: time.Hour}
	local := &oauth.MemLimiter{}
	s := &oauth.Server{Cfg: cfg, Sealer: sealer, Hydra: &oauth.HydraAdmin{Base: env("AMBIT_AUTH_HYDRA_ADMIN")}, HydraPub: env("AMBIT_AUTH_HYDRA_PUBLIC"),
		Backend: &oauth.ConsoleBackend{Base: env("AMBIT_AUTH_CONSOLE_URL"), TokenFile: env("AMBIT_AUTH_CONSOLE_TOKEN_FILE")},
		Deny:    deny, IdP: idp, Clients: &oauth.ValkeyRegistry{C: vc, Prefix: "ambit:oauth:"},
		Limits: &oauth.Limits{Shared: &oauth.ValkeyLimiter{C: vc, Prefix: "ambit:oauth:"}, Local: local, Rates: rates}}
	go s.RunJanitor(ctx, 10*time.Minute)

	var ready atomic.Bool
	go func() {
		for ctx.Err() == nil {
			pctx, c := context.WithTimeout(ctx, 3*time.Second)
			ok := deny.Fresh() == nil && vc.Ping(pctx).Err() == nil
			if ok {
				req, _ := http.NewRequestWithContext(pctx, "GET", env("AMBIT_AUTH_HYDRA_ADMIN")+"/health/ready", nil)
				if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
					ok = false
				} else {
					resp.Body.Close()
				}
			}
			c()
			ready.Store(ok)
			time.Sleep(3 * time.Second)
		}
	}()
	ops := http.NewServeMux()
	ops.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	ops.Handle("GET /readyz", oauth.ReadyHandler(&ready))
	ops.Handle("GET /metrics", promhttp.Handler())

	errc := make(chan error, 3)
	addr := func(k, def string) string {
		if env(k) != "" {
			return env(k)
		}
		return def
	}
	for addr, h := range map[string]http.Handler{addr("AMBIT_AUTH_ADDR_PUBLIC", ":8080"): s.Public(), addr("AMBIT_AUTH_ADDR_HOOK", ":8081"): s.Internal(), addr("AMBIT_AUTH_ADDR_OPS", ":8082"): ops} {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
		go func() {
			<-ctx.Done()
			sc, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = srv.Shutdown(sc)
		}()
		go func() { errc <- srv.ListenAndServe() }()
	}
	slog.Info("ambit-auth ready", "issuer", cfg.PublicURL, "resource", cfg.Resource, "registrationHosts", cfg.RegistrationHosts)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}
