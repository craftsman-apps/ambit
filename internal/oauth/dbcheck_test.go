package oauth

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/lib/pq"
)

const goodDSN = "postgres://hydra:s3cr3t-pw@postgres.example.svc:5432/hydra?sslmode=verify-full&sslrootcert=/ca/ca.crt&max_conns=8&max_idle_conns=4"

func TestParseDSN(t *testing.T) {
	d, err := ParseDSN(goodDSN)
	if err != nil || d.Host != "postgres.example.svc:5432" || d.Database != "hydra" || d.MaxConns != 8 {
		t.Fatalf("good DSN: %+v, %v", d, err)
	}
	for name, tc := range map[string]struct{ dsn, want string }{
		"sslmode disable":        {strings.Replace(goodDSN, "verify-full", "disable", 1), "sslmode=verify-full (it says disable)"},
		"sslmode allow":          {strings.Replace(goodDSN, "verify-full", "allow", 1), "(it says allow)"},
		"sslmode prefer":         {strings.Replace(goodDSN, "verify-full", "prefer", 1), "(it says prefer)"},
		"sslmode require":        {strings.Replace(goodDSN, "verify-full", "require", 1), "(it says require)"},
		"sslmode verify-ca":      {strings.Replace(goodDSN, "verify-full", "verify-ca", 1), "(it says verify-ca)"},
		"no sslmode":             {strings.Replace(goodDSN, "sslmode=verify-full&", "", 1), "(not set)"},
		"sslmode repeated":       {goodDSN + "&sslmode=disable", "repeats a parameter"},
		"no CA":                  {strings.Replace(goodDSN, "&sslrootcert=/ca/ca.crt", "", 1), "sslrootcert"},
		"no max_conns":           {strings.Replace(goodDSN, "&max_conns=8", "", 1), "max_conns"},
		"max_conns zero":         {strings.Replace(goodDSN, "max_conns=8", "max_conns=0", 1), "max_conns"},
		"max_conns huge":         {strings.Replace(goodDSN, "max_conns=8", "max_conns=500", 1), "max_conns"},
		"key=value form":         {"host=db user=hydra password=s3cr3t-pw sslmode=verify-full", "postgres://"},
		"other scheme":           {"mysql://hydra:s3cr3t-pw@db/hydra", "postgres://"},
		"no host":                {"postgres://hydra:s3cr3t-pw@/hydra?sslmode=verify-full", "no host"},
		"no database":            {"postgres://hydra:s3cr3t-pw@db:5432/?sslmode=verify-full", "no database"},
		"broken URL with secret": {"postgres://hydra:s3cr3t-pw@db:port/hydra", "postgres://"},
		// names that would change what the check connects to, or that may hold a secret
		"host parameter":            {goodDSN + "&host=evil.example", "does not accept"},
		"hostaddr parameter":        {goodDSN + "&hostaddr=10.0.0.1", "does not accept"},
		"port parameter":            {goodDSN + "&port=1", "does not accept"},
		"options parameter":         {goodDSN + "&options=-c%20role%3Dpostgres", "does not accept"},
		"session_authorization":     {goodDSN + "&session_authorization=postgres", "does not accept"},
		"sslcert":                   {goodDSN + "&sslcert=/x", "does not accept"},
		"service":                   {goodDSN + "&service=x", "does not accept"},
		"encoded name that repacks": {goodDSN + "&x%3Dy%26host=evil.example", "does not accept"},
		"malformed pair":            {goodDSN + "&a=%zz", "malformed"},
		"semicolon":                 {goodDSN + ";host=evil.example", "malformed"},
		"secret as a name":          {goodDSN + "&s3cr3t-in-a-name=1", "does not accept"},
	} {
		_, err := ParseDSN(tc.dsn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want it to contain %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t-pw") {
			t.Errorf("%s: the error repeats the password: %v", name, err)
		}
	}
}

func TestProblems(t *testing.T) {
	// fix fills the roles of a fact: the role the DSN names is the one that logged in and is in effect, unless the case says otherwise
	fix := func(f Facts) (Facts, DSN) {
		if f.Role == "" {
			f.Role = "hydra"
		}
		if f.Effective == "" {
			f.Effective = f.Role
		}
		return f, DSN{MaxConns: 8, User: f.Role}
	}
	check := func(f Facts, replicas int) []string { f, d := fix(f); return f.Problems(d, replicas) }
	if p := check(Facts{VersionNum: 170012, TLS: true, ConnLimit: -1}, 2); len(p) != 0 {
		t.Fatalf("a good database has problems: %v", p)
	}
	for _, v := range []int{150000, 160009, 170012} {
		if p := check(Facts{VersionNum: v, TLS: true, ConnLimit: -1}, 2); len(p) != 0 {
			t.Errorf("version %d is supported but has problems: %v", v, p)
		}
	}
	for name, tc := range map[string]struct {
		f    Facts
		want string
	}{
		"14":                    {Facts{VersionNum: 140012, TLS: true, ConnLimit: -1}, "PostgreSQL 14 is not supported"},
		"18":                    {Facts{VersionNum: 180001, TLS: true, ConnLimit: -1}, "PostgreSQL 18 is not supported"},
		"no TLS":                {Facts{VersionNum: 170012, ConnLimit: -1}, "does not use TLS"},
		"superuser":             {Facts{VersionNum: 170012, TLS: true, Superuser: true, ConnLimit: -1, Role: "postgres"}, `"postgres" is a superuser`},
		"limit":                 {Facts{VersionNum: 170012, TLS: true, ConnLimit: 16, Role: "hydra"}, "need 17"},
		"session authorization": {Facts{VersionNum: 170012, TLS: true, ConnLimit: -1, Role: "hydra", Effective: "other"}, `not the role that the DSN names`},
	} {
		p := check(tc.f, 2)
		if len(p) != 1 || !strings.Contains(p[0], tc.want) {
			t.Errorf("%s: %v, want one problem containing %q", name, p, tc.want)
		}
	}
	// the role that logged in is not the one the DSN names (a server-side default of SESSION AUTHORIZATION), both sessions alike
	f := Facts{VersionNum: 170012, TLS: true, ConnLimit: -1, Role: "other", Effective: "other"}
	if p := f.Problems(DSN{MaxConns: 8, User: "hydra"}, 2); len(p) != 1 || !strings.Contains(p[0], `not the role that the DSN names`) {
		t.Errorf("another login than the DSN's: %v", p)
	}
	// a name from the server with odd characters is not repeated
	f = Facts{VersionNum: 170012, TLS: true, ConnLimit: -1, Role: "x\ny s3cr3t", Effective: "x\ny s3cr3t"}
	if p := f.Problems(DSN{MaxConns: 8, User: "hydra"}, 2); len(p) != 1 || strings.Contains(p[0], "s3cr3t") {
		t.Errorf("an odd role name is repeated: %v", p)
	}
	// exactly enough connections: replicas x max_conns + 1 for the check itself
	if p := check(Facts{VersionNum: 170012, TLS: true, ConnLimit: 17}, 2); len(p) != 0 {
		t.Errorf("a limit of 17 is enough for 2 pods of 8: %v", p)
	}
}

// A DSN that fails the parse never reaches the network, and nothing of it is printed.
func TestCheckDatabaseRefusesBeforeConnecting(t *testing.T) {
	p, _, err := CheckDatabase(t.Context(), strings.Replace(goodDSN, "verify-full", "require", 1), 2)
	if err != nil || len(p) != 1 || strings.Contains(p[0], "s3cr3t-pw") {
		t.Fatalf("got %v, %v", p, err)
	}
}

func TestConnectErrorIsFixedText(t *testing.T) {
	const secret = "s3cr3t-in-a-name"
	for name, err := range map[string]error{
		"server error":  &pq.Error{Code: "08P01", Message: `unrecognized configuration parameter "` + secret + `"`},
		"wrapped":       fmt.Errorf("x: %w", &pq.Error{Code: "28P01", Message: "password " + secret}),
		"no tls":        pq.ErrSSLNotSupported,
		"unknown CA":    x509.UnknownAuthorityError{},
		"wrong name":    x509.HostnameError{Host: secret + ".example"},
		"network":       &net.OpError{Op: "dial", Err: errors.New("refused " + secret)},
		"anything else": errors.New("boom " + secret),
	} {
		got := ConnectError(err).Error()
		if strings.Contains(got, secret) || strings.Contains(got, "example") {
			t.Errorf("%s: the message repeats the input: %s", name, got)
		}
		if !strings.HasPrefix(got, "connect to the database:") {
			t.Errorf("%s: unexpected message %q", name, got)
		}
	}
	if got := ConnectError(&pq.Error{Code: "28P01"}).Error(); !strings.Contains(got, "SQLSTATE 28P01") {
		t.Errorf("the SQLSTATE is the one useful fact: %s", got)
	}
	if got := ConnectError(&pq.Error{Code: pq.ErrorCode("XX000")}).Error(); !strings.Contains(got, "SQLSTATE other") || strings.Contains(got, "XX000") {
		t.Errorf("a malformed SQLSTATE is repeated: %s", got)
	}
}
