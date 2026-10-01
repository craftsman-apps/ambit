package console

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTLSConfigFromEnv(t *testing.T) {
	if c, err := TLSConfig(false, ""); c != nil || err != nil {
		t.Fatalf("off: %v %v", c, err)
	}
	if c, err := TLSConfig(true, ""); c == nil || c.RootCAs != nil || err != nil {
		t.Fatalf("system roots: %+v %v", c, err)
	}
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	if c, err := TLSConfig(true, ca); c == nil || c.RootCAs == nil || err != nil {
		t.Fatalf("private CA: %+v %v", c, err)
	}
	os.WriteFile(ca, []byte("not a certificate"), 0o600)
	if _, err := TLSConfig(true, ca); err == nil {
		t.Fatal("a CA file without certificates was accepted")
	}
	if _, err := TLSConfig(true, ca+".missing"); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
}

func TestClickHouseOptions(t *testing.T) {
	tlsc, _ := TLSConfig(true, "")
	o := clickhouseOptions(ClickHouse{Addr: "abc.clickhouse.cloud:8443", User: "ambit_ro", Database: "audit", Password: "pw", TLS: tlsc})
	if o.Addr[0] != "abc.clickhouse.cloud:8443" || o.Auth.Username != "ambit_ro" || o.Auth.Database != "audit" || o.Auth.Password != "pw" || o.TLS == nil {
		t.Fatalf("%+v", o)
	}
	o = clickhouseOptions(ClickHouse{Addr: "ambit-clickhouse:8123", Password: "pw"})
	if o.Auth.Username != "ambit" || o.Auth.Database != "otel" || o.TLS != nil {
		t.Fatalf("defaults %+v", o)
	}
}
