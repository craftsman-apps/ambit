package console

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// TLSConfig is the client TLS setting for an external database: nil when off; the system roots, or
// only the CA in caFile (a PEM file) when one is given, for databases behind a private CA.
func TLSConfig(enabled bool, caFile string) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("CA file: %w", err)
	}
	c.RootCAs = x509.NewCertPool()
	if !c.RootCAs.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file " + caFile + " holds no PEM certificate")
	}
	return c, nil
}
