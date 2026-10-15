package console

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The calls query and the summary query share one definition of the outcome (spec 4.2), so the list and the
// summary cannot disagree.
func TestAuditQueriesShareTheOutcomeDefinition(t *testing.T) {
	for _, q := range []string{callsSQL, wouldRefuseTotalsSQL, wouldRefuseGroupsSQL} {
		for _, s := range []string{"AS c_outcome", "'mark-missing'", "'would-refuse'", "'refused'", "LogAttributes['ambit.accessMode']",
			"ambit.wouldRefuseTool", "mapKeys(LogAttributes)", "'401', '403', '429'"} {
			if !strings.Contains(q, s) {
				t.Errorf("a query does not contain %q:\n%s", s, q)
			}
		}
		if strings.Contains(q, "{status}") || strings.Contains(q, "{classify}") {
			t.Errorf("unreplaced placeholder:\n%s", q)
		}
	}
	for _, a := range []string{"ambit.accessMode", "ambit.rulesVersion"} {
		if !strings.Contains(callsSQL, "LogAttributes['"+a+"']") {
			t.Errorf("calls query does not read %s", a)
		}
	}
}

// Every input reaches ClickHouse as a parameter, never as SQL text.
func TestWouldRefuseSQLUsesParameters(t *testing.T) {
	for _, q := range []string{wouldRefuseTotalsSQL, wouldRefuseGroupsSQL} {
		for _, s := range []string{"{from:DateTime64(9)}", "{to:DateTime64(9)}", "{rules:String}", "{all:UInt8}"} {
			if !strings.Contains(q, s) {
				t.Errorf("missing %s in %s", s, q)
			}
		}
	}
}

// The endpoint is for admins. An audit store that does not answer is reported as such; a bad rules value is
// the caller's mistake; rules=current needs a published rules version.
func TestWouldRefuseEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	build := func(data map[string]string) (http.Handler, *Auth) {
		b := fake.NewClientBuilder().WithScheme(scheme)
		if data != nil {
			b = b.WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ambit-access-mode", Namespace: testNS}, Data: data})
		}
		auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
		audit := OpenAudit(ClickHouse{Addr: "127.0.0.1:1", Password: "x"}) // nothing listens here
		return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: b.Build(), Namespace: testNS, Audit: audit}), auth
	}
	h, a := build(map[string]string{"mode": "audit", "rulesVersion": "3f9a1c2b7d10"})
	admin := user(a, "priya@acme.test", RoleAdmin)
	for u, want := range map[string]int{
		"/api/audit/would-refuse":                               http.StatusBadGateway,
		"/api/audit/would-refuse?rules=all":                     http.StatusBadGateway,
		"/api/audit/would-refuse?rules=current":                 http.StatusBadGateway,
		"/api/audit/would-refuse?rules=bogus":                   http.StatusBadRequest,
		"/api/audit/would-refuse?from=2026-04-01&to=2026-03-01": http.StatusBadRequest,
	} {
		if rec := do(t, h, "GET", u, admin, nil); rec.Code != want || (want == http.StatusBadGateway && !strings.Contains(rec.Body.String(), "ClickHouse")) {
			t.Errorf("%s: %d %s, want %d", u, rec.Code, rec.Body.String(), want)
		}
	}
	for _, role := range []string{RoleViewer, RoleApprover} {
		if rec := do(t, h, "GET", "/api/audit/would-refuse", user(a, "x@acme.test", role), nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", role, rec.Code)
		}
	}
	h, a = build(nil) // no ambit-access-mode: there is no current version
	if rec := do(t, h, "GET", "/api/audit/would-refuse?rules=current", user(a, "priya@acme.test", RoleAdmin), nil); rec.Code != http.StatusConflict {
		t.Errorf("no published version: %d %s", rec.Code, rec.Body.String())
	}
}

// audit builds the attributes of one served audit-mode LLM record.
func audit(extra map[string]string) map[string]string {
	m := map[string]string{"ambit.accessMode": "audit", "ambit.rulesVersion": "v1", "http.status": "200", "jwt.email": "u@acme.test",
		"jwt.groups": `["eng"]`, "gen_ai.request.model": "gpt-4o"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func without(m map[string]string, keys ...string) map[string]string {
	for _, k := range keys {
		delete(m, k)
	}
	return m
}

// The outcome table of spec 4.2, run against a real ClickHouse: set AMBIT_TEST_CLICKHOUSE_ADDR to the HTTP
// address of one (host:port). The test makes its own database and drops it.
func TestAuditOutcomesAgainstClickHouse(t *testing.T) {
	addr := os.Getenv("AMBIT_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("set AMBIT_TEST_CLICKHOUSE_ADDR=host:port to run the outcome table against a ClickHouse")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("ambit_test_%d", time.Now().UnixNano())
	admin := clickhouse.OpenDB(&clickhouse.Options{Protocol: clickhouse.HTTP, Addr: []string{addr}, Auth: clickhouse.Auth{Database: "default", Username: "default"}})
	exec := func(db *sql.DB, q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(admin, "CREATE DATABASE "+dbName)
	defer admin.ExecContext(ctx, "DROP DATABASE "+dbName)
	db := clickhouse.OpenDB(&clickhouse.Options{Protocol: clickhouse.HTTP, Addr: []string{addr}, Auth: clickhouse.Auth{Database: dbName, Username: "default"}})
	exec(db, "CREATE TABLE otel_logs (Timestamp DateTime64(9), LogAttributes Map(String, String)) ENGINE = MergeTree ORDER BY Timestamp")

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second) // the driver binds seconds
	type row struct {
		name  string
		attrs map[string]string
		want  string // outcome
		mark  string
	}
	mcp := func(extra map[string]string) map[string]string {
		m := audit(map[string]string{"mcp.method.name": "tools/call", "mcp.target": "github", "gen_ai.tool.name": "create_issue"})
		delete(m, "gen_ai.request.model")
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	rows := []row{
		{"model allowed", audit(map[string]string{"ambit.wouldRefuse": "allow"}), "allow", "allow"},
		{"model would be refused", audit(map[string]string{"ambit.wouldRefuse": "refuse:model"}), "would-refuse", "refuse:model"},
		{"router refused", audit(map[string]string{"ambit.wouldRefuse": "refuse:router"}), "would-refuse", "refuse:router"},
		{"other model", audit(map[string]string{"ambit.wouldRefuse": "other-model"}), "other-model", "other-model"},
		{"chunk 2 holds the mark", audit(map[string]string{"ambit.wouldRefuse": "n/a", "ambit.wouldRefuse.1": "n/a", "ambit.wouldRefuse.2": "refuse:model"}), "would-refuse", "refuse:model"},
		{"403 with refuse mark is refused for real", audit(map[string]string{"http.status": "403", "ambit.wouldRefuse": "refuse:model"}), "refused", "refuse:model"},
		{"429 with allow mark is refused for real", audit(map[string]string{"http.status": "429", "ambit.wouldRefuse": "allow"}), "refused", "allow"},
		{"401 without a mark is refused for real", audit(map[string]string{"http.status": "401"}), "refused", ""},
		{"v1.6 status name", without(audit(map[string]string{"http.response.status_code": "403"}), "http.status"), "refused", ""},
		{"LLM call without the field is mark-missing", audit(nil), "mark-missing", ""},
		{"value outside the list is mark-missing", audit(map[string]string{"ambit.wouldRefuse": "maybe"}), "mark-missing", "maybe"},
		{"all parts n/a on an LLM call is n/a", audit(map[string]string{"ambit.wouldRefuse": "n/a"}), "n/a", "n/a"},
		{"MCP tool allowed", mcp(map[string]string{"ambit.wouldRefuseTool": "allow"}), "allow", "allow"},
		{"MCP tool would be refused", mcp(map[string]string{"ambit.wouldRefuseTool": "refuse:tool"}), "would-refuse", "refuse:tool"},
		{"MCP tool call without the field is mark-missing", mcp(nil), "mark-missing", ""},
		{"MCP record reads the tool field, not the model field", mcp(map[string]string{"ambit.wouldRefuse": "refuse:model"}), "mark-missing", ""},
		{"tools/list is n/a", mcp(map[string]string{"mcp.method.name": "tools/list"}), "n/a", ""},
		{"initialize is n/a", mcp(map[string]string{"mcp.method.name": "initialize"}), "n/a", ""},
		{"LLM record without a model is n/a", without(audit(nil), "gen_ai.request.model"), "n/a", ""},
		{"5xx is not counted", audit(map[string]string{"http.status": "502", "ambit.wouldRefuse": "refuse:model"}), "n/a", "refuse:model"},
		{"enforce-mode record is n/a", audit(map[string]string{"ambit.accessMode": "enforce"}), "n/a", ""},
		{"older record without ambit.accessMode is n/a", without(audit(map[string]string{"http.status": "403"}), "ambit.accessMode"), "n/a", ""},
	}
	for i, r := range rows {
		attrs := r.attrs
		attrs["jwt.email"] = fmt.Sprintf("p%d@acme.test", i)
		exec(db, "INSERT INTO otel_logs (Timestamp, LogAttributes) VALUES (?, ?)", base.Add(time.Duration(i)*time.Second), attrs)
	}

	a := &Audit{DB: db}
	for i, r := range rows {
		email := fmt.Sprintf("p%d@acme.test", i)
		calls, err := a.Calls(ctx, email, base.Add(-time.Hour), base.Add(time.Hour), 10)
		if err != nil || len(calls) != 1 {
			t.Fatalf("%s: %v %+v", r.name, err, calls)
		}
		if got := calls[0]; got.Outcome != r.want || got.Mark != r.mark {
			t.Errorf("%s: outcome %q mark %q, want %q %q", r.name, got.Outcome, got.Mark, r.want, r.mark)
		}
	}

	// The summary: counts, people, groups, targets, newest.
	exec(db, "TRUNCATE TABLE otel_logs")
	at := func(i int, attrs map[string]string) {
		exec(db, "INSERT INTO otel_logs (Timestamp, LogAttributes) VALUES (?, ?)", base.Add(time.Duration(i)*time.Second), attrs)
	}
	at(0, audit(map[string]string{"jwt.email": "a@acme.test", "jwt.groups": `["ds","eng"]`, "ambit.wouldRefuse": "refuse:model"})) // two groups: counts in both
	at(1, audit(map[string]string{"jwt.email": "a@acme.test", "jwt.groups": `["ds","eng"]`, "ambit.wouldRefuse": "refuse:model"}))
	at(2, audit(map[string]string{"jwt.email": "b@acme.test", "jwt.groups": `["ds"]`, "ambit.wouldRefuse": "refuse:model"}))
	at(3, audit(map[string]string{"jwt.email": "c@acme.test", "jwt.groups": `[]`, "ambit.wouldRefuse": "refuse:model", "ambit.rulesVersion": "v0"})) // old rules
	at(4, mcp(map[string]string{"jwt.email": "c@acme.test", "jwt.groups": `[]`, "ambit.wouldRefuseTool": "refuse:tool"}))
	at(5, audit(map[string]string{"jwt.email": "d@acme.test", "ambit.wouldRefuse": "allow"}))
	at(6, audit(nil))                                                                                   // mark missing
	at(7, audit(map[string]string{"http.status": "403"}))                                               // refused for real
	at(8, audit(map[string]string{"ambit.accessMode": "enforce", "ambit.wouldRefuse": "refuse:model"})) // not audit mode

	res, err := a.WouldRefuse(ctx, base.Add(-time.Hour), base.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 5 || res.People != 3 || res.MarkMissing != 1 || res.Refused != 1 || res.Limited {
		t.Fatalf("all rules: %+v", res)
	}
	if res.Newest == nil || !res.Newest.Equal(base.Add(7*time.Second)) { // the newest audit-mode record; the enforce record at 8 s is not one
		t.Fatalf("newest %v", res.Newest)
	}
	want := map[string]uint64{"ds|gpt-4o": 3, "eng|gpt-4o": 2, "(no group)|gpt-4o": 1, "(no group)|github · create_issue": 1}
	got := map[string]uint64{}
	for _, g := range res.Groups {
		got[g.Group+"|"+g.Target] += g.Requests
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	cur, err := a.WouldRefuse(ctx, base.Add(-time.Hour), base.Add(time.Hour), "v1") // rules=current
	if err != nil || cur.Requests != 4 || cur.People != 3 {
		t.Fatalf("current rules: %+v %v", cur, err)
	}
	none, err := a.WouldRefuse(ctx, base.Add(time.Hour), base.Add(2*time.Hour), "")
	if err != nil || none.Requests != 0 || none.Newest != nil || len(none.Groups) != 0 {
		t.Fatalf("empty window: %+v %v", none, err)
	}
}
