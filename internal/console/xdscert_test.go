package console

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// tlsServing serves TLS with a self-signed certificate valid until notAfter, and returns its address.
func tlsServing(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: notAfter.Add(-24 * time.Hour), NotAfter: notAfter, DNSNames: []string{"agentgateway"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// agentgateway renews its xDS certificate 12 hours before it expires (24-hour lifetime). Under 11 hours
// left means the renewal did not happen; at expiry, new gateway pods cannot get their configuration.
func TestXDSCertificateCheck(t *testing.T) {
	now := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		left    time.Duration
		healthy bool
		detail  string
	}{
		{20 * time.Hour, true, "valid until 2026-10-03 21:00 UTC"},
		{11*time.Hour + time.Minute, true, "valid until 2026-10-03 12:01 UTC"},
		{10 * time.Hour, false, "not renewed: expires 2026-10-03 11:00 UTC"},
		{-time.Hour, false, "expired 2026-10-03 00:00 UTC"},
	} {
		d := &Dependencies{XDSAddr: tlsServing(t, now.Add(c.left)), Now: func() time.Time { return now }}
		ok, detail := d.xdsCertificate(t.Context())
		if ok != c.healthy || !strings.Contains(detail, c.detail) {
			t.Errorf("%v left: %v %q, want %v containing %q", c.left, ok, detail, c.healthy, c.detail)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if ok, detail := (&Dependencies{XDSAddr: addr}).xdsCertificate(t.Context()); ok || !strings.Contains(detail, "no answer") {
		t.Errorf("nothing listening: %v %q", ok, detail)
	}
}
