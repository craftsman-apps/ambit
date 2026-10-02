package console

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Dependency is one component Ambit relies on: what happens when it is down (decided and tested in
// acceptance step 8, or in the guard's tests) and whether it answers now.
type Dependency struct {
	Name     string `json:"name"`
	Purpose  string `json:"purpose"`
	Mode     string `json:"mode"` // fail-open, fail-closed, partial, or required (everything stops)
	WhenDown string `json:"whenDown"`
	Healthy  bool   `json:"healthy"`
	Detail   string `json:"detail"`
}

// Dependencies reports Ambit's dependencies for the Dependencies page. Fields mirror what is installed.
type Dependencies struct {
	Client        client.Client
	Namespace     string
	Audit         *Audit   // nil: evidence off
	Budgets       *Budgets // nil: limits off
	RatelimitMode string   // FailOpen or FailClosed, from limits.failureMode
	Protection    bool     // ambit-guard installed
	XDSAddr       string   // agentgateway's control plane (xDS), host:port; empty skips the certificate check
	Now           func() time.Time
}

// xdsCertificate reads the expiry of the certificate agentgateway's control plane serves to gateway pods.
// It renews it 12 hours before expiry (24-hour lifetime); after expiry new gateway pods cannot get their
// configuration (seen 2026-10-02: the renewal did not happen, and new pods stayed not ready).
func (d *Dependencies) xdsCertificate(ctx context.Context) (bool, string) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	// Only the dates are read, so the chain is not verified; nothing is sent over the connection.
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	conn, err := dialer.DialContext(ctx, "tcp", d.XDSAddr)
	if err != nil {
		return answered(err, "")
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return false, "no certificate from " + d.XDSAddr
	}
	end := certs[0].NotAfter.UTC()
	at := end.Format("2006-01-02 15:04 UTC")
	switch left := end.Sub(now); {
	case left <= 0:
		return false, "expired " + at + ". Restart the control plane: kubectl rollout restart deploy/ambit-agentgateway"
	case left < 11*time.Hour:
		return false, "not renewed: expires " + at + ". Restart the control plane before then: kubectl rollout restart deploy/ambit-agentgateway"
	}
	return true, "valid until " + at
}

type depCheck struct {
	dep   Dependency
	check func(context.Context) (bool, string)
}

// workload reports whether a Deployment or StatefulSet of the chart has a ready pod.
func (d *Dependencies) workload(obj client.Object, name string) func(context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		if err := d.Client.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: name}, obj); err != nil {
			return false, "cannot read " + name + ": " + err.Error()
		}
		var ready, want int32
		switch o := obj.(type) {
		case *appsv1.Deployment:
			ready, want = o.Status.ReadyReplicas, ptrOr(o.Spec.Replicas, 1)
		case *appsv1.StatefulSet:
			ready, want = o.Status.ReadyReplicas, ptrOr(o.Spec.Replicas, 1)
		}
		return ready > 0, fmt.Sprintf("%d of %d pods ready", ready, want)
	}
}

func ptrOr(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func answered(err error, ok string) (bool, string) {
	if err != nil {
		return false, "no answer: " + err.Error()
	}
	return true, ok
}

func (d *Dependencies) checks() []depCheck {
	out := []depCheck{
		{Dependency{Name: "Gateway", Purpose: "Carries every MCP and LLM request", Mode: "required",
			WhenDown: "All MCP and LLM requests fail. Run two or more replicas (gateway.replicas)."},
			d.workload(&appsv1.Deployment{}, "ambit")},
		{Dependency{Name: "Dex", Purpose: "Signs people in through your identity provider", Mode: "partial",
			WhenDown: "New sign-ins fail. Tokens already issued keep working until they expire."},
			d.workload(&appsv1.Deployment{}, "ambit-dex")},
	}
	if d.XDSAddr != "" {
		out = append(out, depCheck{Dependency{Name: "Gateway control plane", Purpose: "Sends configuration to gateway pods over TLS",
			Mode: "partial", WhenDown: "Running gateway pods keep their last configuration. New or restarted gateway pods cannot start, and changes do not reach the gateway."},
			d.xdsCertificate})
	}
	if d.Budgets != nil {
		mode, when := "fail-open", "LLM requests pass without being counted against budgets (limits.failureMode: FailOpen)."
		if d.RatelimitMode == "FailClosed" {
			mode, when = "fail-closed", "While any Budget exists, LLM requests are refused (limits.failureMode: FailClosed)."
		}
		v := d.Budgets.Valkey
		out = append(out,
			depCheck{Dependency{Name: "Rate limit service", Purpose: "Counts LLM requests against budgets", Mode: mode, WhenDown: when},
				d.workload(&appsv1.Deployment{}, "ambit-ratelimit")},
			depCheck{Dependency{Name: "Valkey", Purpose: "Stores budget counters for every gateway replica", Mode: mode,
				WhenDown: when + " The Budgets page cannot show today's use."},
				func(ctx context.Context) (bool, string) { _, err := v.do(ctx, "PING"); return answered(err, "answers PING at "+v.Addr) }})
	}
	if d.Protection {
		out = append(out, depCheck{Dependency{Name: "ambit-guard", Purpose: "Tool pinning and masking of personal data",
			Mode: "fail-closed", WhenDown: "MCP tool calls fail rather than run unchecked."},
			d.workload(&appsv1.Deployment{}, "ambit-guard")})
	}
	if d.Audit != nil {
		db := d.Audit.DB
		out = append(out,
			depCheck{Dependency{Name: "OpenTelemetry Collector", Purpose: "Carries access logs to ClickHouse", Mode: "fail-open",
				WhenDown: "MCP and LLM requests continue. Records of calls made during the outage can be missing from Audit."},
				d.workload(&appsv1.Deployment{}, "ambit-otel-collector")},
			depCheck{Dependency{Name: "ClickHouse", Purpose: "Stores the audit trail and token usage", Mode: "fail-open",
				WhenDown: "MCP and LLM requests continue. The Audit and Usage pages show an error."},
				func(ctx context.Context) (bool, string) { return answered(db.PingContext(ctx), "answers queries") }})
	}
	return out
}

// Report runs every check at once, each limited to a few seconds.
func (d *Dependencies) Report(ctx context.Context) []Dependency {
	cs := d.checks()
	out := make([]Dependency, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			out[i] = c.dep
			out[i].Healthy, out[i].Detail = c.check(cctx)
		}()
	}
	wg.Wait()
	return out
}

func (d *Dependencies) register(mux *http.ServeMux, admin func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc) {
	mux.HandleFunc("GET /api/dependencies", admin(func(w http.ResponseWriter, r *http.Request, _ Session) {
		writeJSON(w, http.StatusOK, d.Report(r.Context()))
	}))
}
