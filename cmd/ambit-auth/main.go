// ambit-auth is Ambit's OAuth sign-in component (experimental in 0.6.x, docs/oauth.md). It runs the sign-in host, and two commands
// that run beside Ory Hydra:
//
//	ambit-auth check-database   an init container of Hydra's pods: the DSN, the server and the role (exit 1: not usable)
//	ambit-auth serve            the sign-in host (docs/oauth.md)
//	ambit-auth hydra-keys       a container of Hydra's pods: keeps the signing key sets at one key, and is the pod's readiness
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/craftsman-apps/ambit/internal/oauth"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ambit-auth serve | check-database | hydra-keys")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "check-database":
		err = checkDatabase(ctx, os.Args[2:])
	case "serve":
		err = serve(ctx)
	case "hydra-keys":
		err = hydraKeys(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		slog.Error("ambit-auth", "command", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func checkDatabase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check-database", flag.ExitOnError)
	replicas := fs.Int("replicas", 2, "Hydra pods (each may hold max_conns connections)")
	env := fs.String("dsn-env", "DSN", "environment variable with the DSN")
	_ = fs.Parse(args)
	problems, f, err := oauth.CheckDatabase(ctx, os.Getenv(*env), *replicas)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		for _, p := range problems {
			slog.Error("the database is not usable for Ambit's OAuth sign-in (docs/oauth.md, \"PostgreSQL\")", "problem", p)
		}
		return fmt.Errorf("%d problem(s) with the database", len(problems))
	}
	slog.Info("database ok", "postgresMajor", f.Major(), "tls", f.TLS, "role", f.Role)
	return nil
}

func hydraKeys(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hydra-keys", flag.ExitOnError)
	admin := fs.String("admin", "http://127.0.0.1:4445", "Hydra's admin API in this pod")
	listen := fs.String("listen", ":8082", "address of the readiness endpoint /readyz")
	_ = fs.Parse(args)
	var ready atomic.Bool
	k := &oauth.KeyChecker{Admin: *admin, Client: &http.Client{Timeout: 10 * time.Second}, Sets: oauth.SigningSets, Jitter: 2 * time.Second}
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", oauth.ReadyHandler(&ready))
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go k.Watch(ctx, 5*time.Second, &ready, func(msg string, err error) {
		if err != nil {
			slog.Warn(msg, "ready", false, "err", err)
		} else {
			slog.Info(msg, "ready", true)
		}
	})
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
