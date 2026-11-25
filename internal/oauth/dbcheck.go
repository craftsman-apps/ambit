// Package oauth holds the parts of Ambit's OAuth sign-in (experimental in 0.6.x, docs/oauth.md) that run beside Ory
// Hydra: the check of the customer's PostgreSQL database, and the check that Hydra's signing key sets each hold exactly
// one key.
package oauth

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq" // the PostgreSQL driver of the database check
)

// Supported PostgreSQL major versions (docs/oauth.md, "PostgreSQL"). CI runs the first and the last.
const (
	MinPostgres = 15
	MaxPostgres = 17
)

// The query parameters of the DSN that the check accepts: the TLS rule, Hydra's own pool limits, and the connect timeout.
// Anything else (host, options, a startup parameter such as session_authorization, a secret in the wrong place) is refused.
var (
	dsnParamList = []string{"sslmode", "sslrootcert", "max_conns", "max_idle_conns", "max_conn_lifetime", "max_idle_conn_time", "connect_timeout"}
	dsnParams    = func() map[string]bool {
		m := map[string]bool{}
		for _, k := range dsnParamList {
			m[k] = true
		}
		return m
	}()
)

// DSN is the part of Hydra's database address the check needs. The password is never kept or printed.
type DSN struct {
	URL      string // the address for the driver (without Hydra's pool parameters): never log it
	Host     string
	User     string // the login name the DSN gives (the role that must log in)
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
	// u.Query() drops a malformed pair without a word, and the driver writes the decoded names back without escaping them: a
	// name such as "x=y&host" could then change the server the check connects to. So: a strict parse, and only names that this
	// check knows. A name that is not known is not repeated in the message: it may be a secret in the wrong place.
	// A NUL byte ends a startup field of the PostgreSQL protocol: one in a decoded value could add fields of its own.
	pw, _ := u.User.Password()
	if strings.ContainsRune(u.User.Username()+pw+u.Host+u.Path, 0) || strings.Contains(u.RawQuery, "%00") {
		return DSN{}, errors.New("the DSN holds a NUL character")
	}
	user := u.User.Username()
	if user == "" {
		return DSN{}, errors.New("the DSN names no user")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return DSN{}, errors.New("the DSN has a malformed parameter list")
	}
	for k, v := range q {
		if !dsnParams[k] {
			return DSN{}, errors.New("the DSN has a parameter that Ambit does not accept (accepted: " + strings.Join(dsnParamList, ", ") + ")")
		}
		// The driver reads the last value of a repeated parameter, a reader may read the first: one value only.
		if len(v) != 1 {
			return DSN{}, errors.New("the DSN repeats a parameter (each may appear once)")
		}
	}
	if strings.Contains(u.Host, ",") {
		return DSN{}, errors.New("the DSN must name one server in its address (no host list)")
	}
	if m := q.Get("sslmode"); m != "verify-full" {
		switch m { // only a known mode is repeated
		case "":
			m = "(not set)"
		case "disable", "allow", "prefer", "require", "verify-ca":
		default:
			m = "(not a PostgreSQL mode)"
		}
		return DSN{}, fmt.Errorf("the DSN must say sslmode=verify-full (it says %s): the connection must use TLS and prove the server's name", m)
	}
	if q.Get("sslrootcert") != "/ca/ca.crt" {
		return DSN{}, errors.New("the DSN must name the CA file with sslrootcert=/ca/ca.crt (the chart mounts oauth.database.caSecret there)")
	}
	n, err := strconv.Atoi(q.Get("max_conns"))
	if err != nil || n < 1 || n > 64 {
		return DSN{}, errors.New("the DSN must limit the connections of each Hydra pod with max_conns=<1 to 64>, for example max_conns=8&max_idle_conns=4")
	}
	// Hydra's own pool parameters are not the driver's: it refuses them.
	for _, k := range []string{"max_conns", "max_idle_conns", "max_conn_lifetime", "max_idle_conn_time"} { // (all in the allowlist)
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	return DSN{URL: u.String(), Host: u.Host, User: user, Database: db, MaxConns: n}, nil
}

// Facts of the server and of the role that connected, read in one session.
type Facts struct {
	VersionNum int  // server_version_num, for example 170012
	TLS        bool // this session uses TLS
	Superuser  bool
	ConnLimit  int // the role's connection limit; -1: none
	Role       string
	Effective  string // the role in effect (current_user); it differs from Role after SET SESSION AUTHORIZATION or SET ROLE
}

var sqlstate = regexp.MustCompile(`^[0-9A-Z]{5}$`)

var plainName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// say returns a role name for a message only when it is plain: a name that comes from the server is not repeated as it is.
func say(n string) string {
	if plainName.MatchString(n) {
		return n
	}
	return "(a name with unusual characters)"
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
	if f.Effective != f.Role || f.Role != d.User {
		p = append(p, fmt.Sprintf("the session runs as %q and logged in as %q, but the DSN names %q: all three must be one role (no SET SESSION AUTHORIZATION, no role in effect other than the login)", say(f.Effective), say(f.Role), say(d.User)))
	}
	if f.Superuser {
		p = append(p, fmt.Sprintf("the role %q is a superuser: use an owner role of the Hydra database only, without CREATEROLE or SUPERUSER", say(d.User)))
	}
	if need := replicas*d.MaxConns + 1; f.ConnLimit >= 0 && f.ConnLimit < need {
		p = append(p, fmt.Sprintf("the role %q may open %d connections, but %d Hydra pods with max_conns=%d need %d (CONNECTION LIMIT)", say(d.User), f.ConnLimit, replicas, d.MaxConns, need))
	}
	return p
}

// ReadFacts asks the server about itself over an open connection.
func ReadFacts(ctx context.Context, db *sql.DB, user string) (Facts, error) {
	var f Facts
	var ver string
	if err := db.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&ver); err != nil {
		return f, errors.New("read the server version: the server's answer could not be read")
	}
	var err error
	if f.VersionNum, err = strconv.Atoi(ver); err != nil {
		return f, errors.New("the server version is not a number")
	}
	// A session's own row in pg_stat_ssl is readable by any role.
	if err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).Scan(&f.TLS); err != nil {
		return f, errors.New("read the TLS state: the server's answer could not be read")
	}
	// The role the DSN names (its privileges and connection limit are the ones that count), the role that logged in (session_user)
	// and the role in effect (current_user): Problems requires all three to be one.
	if err := db.QueryRowContext(ctx, `SELECT session_user::text, current_user::text, r.rolsuper, r.rolconnlimit
		FROM pg_catalog.pg_roles r WHERE r.rolname = $1`, user).Scan(&f.Role, &f.Effective, &f.Superuser, &f.ConnLimit); err != nil {
		return f, errors.New("read the role: the server has no login role of the name that the DSN gives, or its answer could not be read")
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
		return nil, Facts{}, ConnectError(err)
	}
	f, err := ReadFacts(ctx, db, d.User)
	if err != nil {
		return nil, f, err
	}
	return f.Problems(d, replicas), f, nil
}

// ConnectError turns a failure to connect into a fixed message. The server's own text is never used: PostgreSQL can echo the name
// of an unknown startup parameter, and a secret may have been written there. The message names no host either.
func ConnectError(err error) error {
	var pe *pq.Error
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var ne net.Error
	switch {
	case errors.As(err, &pe):
		code := string(pe.Code)
		if !sqlstate.MatchString(code) {
			code = "unknown"
		}
		return fmt.Errorf("connect to the database: the server refused the connection (SQLSTATE %s): check the user, the password and the database name", code)
	case errors.Is(err, pq.ErrSSLNotSupported):
		return errors.New("connect to the database: the server does not offer TLS")
	case errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ci):
		return errors.New("connect to the database: the server's certificate was not accepted (not signed by the CA file, wrong name, or expired)")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return errors.New("connect to the database: no answer in time")
	case errors.As(err, &ne):
		return errors.New("connect to the database: the server could not be reached (name, address or port)")
	}
	return errors.New("connect to the database: the connection failed")
}
