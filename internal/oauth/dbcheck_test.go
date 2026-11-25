package oauth

import (
	"strings"
	"testing"
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
		"sslmode repeated":       {goodDSN + "&sslmode=disable", `repeats the parameter "sslmode"`},
		"no CA":                  {strings.Replace(goodDSN, "&sslrootcert=/ca/ca.crt", "", 1), "sslrootcert"},
		"no max_conns":           {strings.Replace(goodDSN, "&max_conns=8", "", 1), "max_conns"},
		"max_conns zero":         {strings.Replace(goodDSN, "max_conns=8", "max_conns=0", 1), "max_conns"},
		"max_conns huge":         {strings.Replace(goodDSN, "max_conns=8", "max_conns=500", 1), "max_conns"},
		"key=value form":         {"host=db user=hydra password=s3cr3t-pw sslmode=verify-full", "postgres://"},
		"other scheme":           {"mysql://hydra:s3cr3t-pw@db/hydra", "postgres://"},
		"no host":                {"postgres://hydra:s3cr3t-pw@/hydra?sslmode=verify-full", "no host"},
		"no database":            {"postgres://hydra:s3cr3t-pw@db:5432/?sslmode=verify-full", "no database"},
		"broken URL with secret": {"postgres://hydra:s3cr3t-pw@db:port/hydra", "postgres://"},
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
	d := DSN{MaxConns: 8}
	ok := Facts{VersionNum: 170012, TLS: true, ConnLimit: -1, Role: "hydra"}
	if p := ok.Problems(d, 2); len(p) != 0 {
		t.Fatalf("a good database has problems: %v", p)
	}
	for _, v := range []int{150000, 160009, 170012} {
		if p := (Facts{VersionNum: v, TLS: true, ConnLimit: -1}).Problems(d, 2); len(p) != 0 {
			t.Errorf("version %d is supported but has problems: %v", v, p)
		}
	}
	for name, tc := range map[string]struct {
		f    Facts
		want string
	}{
		"14":        {Facts{VersionNum: 140012, TLS: true, ConnLimit: -1}, "PostgreSQL 14 is not supported"},
		"18":        {Facts{VersionNum: 180001, TLS: true, ConnLimit: -1}, "PostgreSQL 18 is not supported"},
		"no TLS":    {Facts{VersionNum: 170012, ConnLimit: -1}, "does not use TLS"},
		"superuser": {Facts{VersionNum: 170012, TLS: true, Superuser: true, ConnLimit: -1, Role: "postgres"}, `"postgres" is a superuser`},
		"limit":     {Facts{VersionNum: 170012, TLS: true, ConnLimit: 16, Role: "hydra"}, "need 17"},
	} {
		p := tc.f.Problems(d, 2)
		if len(p) != 1 || !strings.Contains(p[0], tc.want) {
			t.Errorf("%s: %v, want one problem containing %q", name, p, tc.want)
		}
	}
	// exactly enough connections: replicas x max_conns + 1 for the check itself
	if p := (Facts{VersionNum: 170012, TLS: true, ConnLimit: 17}).Problems(d, 2); len(p) != 0 {
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
