// Package oauth holds the parts of Ambit's OAuth sign-in (experimental in 0.6.x, docs/oauth.md) that run beside Ory
// Hydra: the check of the customer's PostgreSQL database, and the check that Hydra's signing key sets each hold exactly
// one key.
package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq" // the PostgreSQL driver of the database check
)

// Supported PostgreSQL major versions (docs/oauth.md, "PostgreSQL"). CI runs the first and the last.
const (
	MinPostgres = 15
	MaxPostgres = 17
)

// DSN is the part of Hydra's database address the check needs. The password is never kept or printed.
type DSN struct {
	URL      string // the address for the driver (without Hydra's pool parameters): never log it
	Host     string
	Database string
	MaxConns int
}

// ParseDSN checks the shape of the address and the TLS rule. Every error is written without the address: a
// parse error of net/url would quote it, password included.
func ParseDSN(raw string) (DSN, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return DSN{}, errors.New("the DSN must be a URL that starts with postgres:// (key=value form is not supported)")
	}
	if u.Hostname() == "" {
		return DSN{}, errors.New("the DSN names no host")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return DSN{}, errors.New("the DSN names no database")
	}
	q := u.Query()
	// The driver reads the last value of a repeated parameter, a reader may read the first: one value only.
	for k, v := range q {
		if len(v) != 1 {
			return DSN{}, fmt.Errorf("the DSN repeats the parameter %q", k)
		}
	}
	if m := q.Get("sslmode"); m != "verify-full" {
		if m == "" {
			m = "(not set)"
		}
		return DSN{}, fmt.Errorf("the DSN must say sslmode=verify-full (it says %s): the connection must use TLS and prove the server's name", m)
	}
	if q.Get("sslrootcert") == "" {
		return DSN{}, errors.New("the DSN must name the CA file with sslrootcert=/ca/ca.crt (the chart mounts oauth.database.caSecret there)")
	}
	n, err := strconv.Atoi(q.Get("max_conns"))
	if err != nil || n < 1 || n > 64 {
		return DSN{}, errors.New("the DSN must limit the connections of each Hydra pod with max_conns=<1 to 64>, for example max_conns=8&max_idle_conns=4")
	}
	// Hydra's own pool parameters are not the driver's: it refuses them.
	for _, k := range []string{"max_conns", "max_idle_conns", "max_conn_lifetime", "max_idle_conn_time"} {
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	return DSN{URL: u.String(), Host: u.Host, Database: db, MaxConns: n}, nil
}

// Facts of the server and of the role that connected, read in one session.
type Facts struct {
	VersionNum int  // server_version_num, for example 170012
	TLS        bool // this session uses TLS
	Superuser  bool
	ConnLimit  int // the role's connection limit; -1: none
	Role       string
}

// Major is the major version: 170012 -> 17.
func (f Facts) Major() int { return f.VersionNum / 10000 }

// Problems lists what is wrong with the database for Ambit: every item names the fix. replicas is the number of Hydra
// pods; every pod may hold MaxConns connections, and one more is kept for this check.
func (f Facts) Problems(d DSN, replicas int) []string {
	var p []string
	if f.Major() < MinPostgres || f.Major() > MaxPostgres {
		p = append(p, fmt.Sprintf("PostgreSQL %d is not supported: use %d to %d", f.Major(), MinPostgres, MaxPostgres))
	}
	if !f.TLS {
		p = append(p, "the session does not use TLS: the server must require TLS (hostssl in pg_hba.conf)")
	}
	if f.Superuser {
		p = append(p, fmt.Sprintf("the role %q is a superuser: use an owner role of the Hydra database only, without CREATEROLE or SUPERUSER", f.Role))
	}
	if need := replicas*d.MaxConns + 1; f.ConnLimit >= 0 && f.ConnLimit < need {
		p = append(p, fmt.Sprintf("the role %q may open %d connections, but %d Hydra pods with max_conns=%d need %d (CONNECTION LIMIT)", f.Role, f.ConnLimit, replicas, d.MaxConns, need))
	}
	return p
}

// ReadFacts asks the server about itself over an open connection.
func ReadFacts(ctx context.Context, db *sql.DB) (Facts, error) {
	var f Facts
	var ver string
	if err := db.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&ver); err != nil {
		return f, fmt.Errorf("read the server version: %w", err)
	}
	var err error
	if f.VersionNum, err = strconv.Atoi(ver); err != nil {
		return f, fmt.Errorf("the server version %q is not a number", ver)
	}
	// A session's own row in pg_stat_ssl is readable by any role.
	if err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&f.TLS); err != nil {
		return f, fmt.Errorf("read the TLS state: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT rolname, rolsuper, rolconnlimit FROM pg_roles WHERE rolname = current_user`).Scan(&f.Role, &f.Superuser, &f.ConnLimit); err != nil {
		return f, fmt.Errorf("read the role: %w", err)
	}
	return f, nil
}

// CheckDatabase connects with the address as written (so a wrong CA or name fails here, as it would for Hydra) and
// returns every problem found. The connection is closed before it returns.
func CheckDatabase(ctx context.Context, raw string, replicas int) ([]string, Facts, error) {
	d, err := ParseDSN(raw)
	if err != nil {
		return []string{err.Error()}, Facts{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", d.URL)
	if err != nil {
		return nil, Facts{}, errors.New("the driver refused the DSN")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		// lib/pq's errors name the host and the reason, never the password.
		return nil, Facts{}, fmt.Errorf("connect to %s: %w", d.Host, err)
	}
	f, err := ReadFacts(ctx, db)
	if err != nil {
		return nil, f, err
	}
	return f.Problems(d, replicas), f, nil
}
