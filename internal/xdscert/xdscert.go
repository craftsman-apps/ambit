// Package xdscert watches the certificate agentgateway's control plane serves to gateway pods (xDS).
// agentgateway renews it 12 hours before it expires (24-hour lifetime). Twice (2026-10-01, 2026-10-03) the
// renewal did not happen, with no log line and no error; the cause is unknown. After expiry, new gateway pods
// cannot get their configuration. A change to its Secret made it renew (2026-10-03).
package xdscert

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Threshold is how little time left means the renewal at 12 hours did not happen.
const Threshold = 11 * time.Hour

// Annotation is what the Nudger writes on agentgateway's Secret; the value is the time of the nudge.
const Annotation = "ambitproject.dev/renew-requested"

// Expiry reads the expiry of the certificate served at addr (host:port). Only the dates are read, so the
// chain is not verified; nothing is sent over the connection.
func Expiry(ctx context.Context, addr string) (time.Time, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, errors.New("no certificate from " + addr)
	}
	return certs[0].NotAfter.UTC(), nil
}

// Nudger changes agentgateway's Secret when the served certificate was not renewed, at most once an hour.
// It runs in the controller, on the leader only.
type Nudger struct {
	Client    client.Client // needs only patch on the Secret
	Namespace string
	Secret    string // kgateway-xds-cert
	Addr      string
	Every     time.Duration                                    // 0: 10 minutes
	Now       func() time.Time                                 // nil: time.Now
	Expiry    func(context.Context, string) (time.Time, error) // nil: Expiry
	last      time.Time                                        // last successful nudge; kept in memory only
}

func (n *Nudger) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Check reads the certificate once and nudges when needed.
func (n *Nudger) Check(ctx context.Context) error {
	expiry := n.Expiry
	if expiry == nil {
		expiry = Expiry
	}
	end, err := expiry(ctx, n.Addr)
	if err != nil {
		return err
	}
	now := n.now()
	if end.Sub(now) >= Threshold || (!n.last.IsZero() && now.Sub(n.last) < time.Hour) {
		return nil
	}
	s := &corev1.Secret{}
	s.Namespace, s.Name = n.Namespace, n.Secret
	patch := []byte(`{"metadata":{"annotations":{"` + Annotation + `":"` + now.UTC().Format(time.RFC3339) + `"}}}`)
	if err := n.Client.Patch(ctx, s, client.RawPatch("application/merge-patch+json", patch)); err != nil {
		return err
	}
	n.last = now
	log.FromContext(ctx).Info("xds certificate not renewed; nudged its Secret", "expires", end.Format(time.RFC3339), "secret", n.Secret)
	return nil
}

// Start checks every n.Every until ctx ends; errors are logged and the next check tries again.
func (n *Nudger) Start(ctx context.Context) error {
	every := n.Every
	if every == 0 {
		every = 10 * time.Minute
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithName("xdscert"))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := n.Check(ctx); err != nil {
			log.FromContext(ctx).Error(err, "xds certificate check failed", "addr", n.Addr)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection: only the leader writes.
func (n *Nudger) NeedLeaderElection() bool { return true }
