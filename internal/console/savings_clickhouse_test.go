package console

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/auditsql"
)

// These tests run the summary views on a real ClickHouse (the image the chart pins). They are skipped unless
// AMBIT_TEST_CLICKHOUSE_ADDR is set; test/stepsavings.sh runs them inside the ClickHouse pod of the kind cluster (the
// password stays in the pod), in scratch databases. AMBIT_TEST_OTEL_LOGS_DDL is a file with the collector's CREATE TABLE of
// otel_logs, so the tests use the real column types.

func clickhouseTestDB(t *testing.T, name string) *Audit {
	t.Helper()
	addr := os.Getenv("AMBIT_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("AMBIT_TEST_CLICKHOUSE_ADDR is not set")
	}
	user := os.Getenv("AMBIT_TEST_CLICKHOUSE_USER")
	a := OpenAudit(ClickHouse{Addr: addr, User: user, Password: os.Getenv("AMBIT_TEST_CLICKHOUSE_PASSWORD"), Database: name}) // the scratch database exists
	ctx := context.Background()
	rows, err := a.DB.QueryContext(ctx, "SELECT name FROM system.tables WHERE database = currentDatabase() ORDER BY engine = 'MaterializedView' DESC")
	if err != nil {
		t.Fatal(err)
	}
	var old []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		old = append(old, n)
	}
	rows.Close()
	for _, n := range old { // views first (the query orders them first)
		if _, err := a.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+n+" SYNC"); err != nil {
			t.Fatal(err)
		}
	}
	ddl, err := os.ReadFile(os.Getenv("AMBIT_TEST_OTEL_LOGS_DDL"))
	if err != nil {
		t.Fatal(err)
	}
	q := strings.NewReplacer("otel.otel_logs", "otel_logs", "CREATE TABLE otel_logs", "CREATE TABLE IF NOT EXISTS otel_logs").Replace(string(ddl))
	if _, err := a.DB.ExecContext(ctx, q); err != nil {
		t.Fatalf("otel_logs: %v", err)
	}
	t.Cleanup(func() { a.DB.Close() })
	return a
}

func sq(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }

// insertLog writes one record the way the collector does (time and attributes).
func insertLog(t *testing.T, a *Audit, ts time.Time, attrs map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kv []string
	for _, k := range keys {
		kv = append(kv, sq(k)+":"+sq(attrs[k]))
	}
	q := fmt.Sprintf("INSERT INTO otel_logs (Timestamp, LogAttributes) VALUES ('%s', {%s})", ts.UTC().Format("2006-01-02 15:04:05.000000000"), strings.Join(kv, ","))
	if _, err := a.DB.ExecContext(context.Background(), q); err != nil {
		t.Fatal(err)
	}
}

func llm(user, groups, model, router string, in, out int) map[string]string {
	m := map[string]string{"protocol": "llm", "jwt.email": user, "gen_ai.request.model": model, "gen_ai.usage.input_tokens": fmt.Sprint(in),
		"gen_ai.usage.output_tokens": fmt.Sprint(out), "http.status": "200", "ambit.accessMode": "enforce"}
	if groups != "" {
		m["jwt.groups"] = groups
	}
	if router != "" {
		m["ambit.router"] = router
	}
	return m
}

func guardRecord(server, typ, groups string, in, out int) map[string]string {
	m := map[string]string{"ambit.kind": "compression", "mcp.target": server, "ambit.compress.type": typ, "ambit.compress.mode": "lossless",
		"ambit.compress.bytes_in": fmt.Sprint(in), "ambit.compress.bytes_out": fmt.Sprint(out), "ambit.compress.omitted": "0"}
	if groups != "" {
		m["jwt.groups"] = groups
	}
	return m
}

// T1, T2, T7, T10 on a real ClickHouse.
func TestSummariesOnClickHouse(t *testing.T) {
	a := clickhouseTestDB(t, "savings_test")
	ctx := context.Background()
	if err := EnsureSummaries(ctx, a.DB, 365); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSummaries(ctx, a.DB, 400); err != nil { // idempotent, and a changed retention is applied
		t.Fatalf("second run: %v", err)
	}
	d1, d2 := time.Date(2026, 10, 30, 23, 59, 59, 0, time.UTC), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	// Before any guard record: the Audit page's answer for user1 and the mark-missing count.
	audit := func() (int, int) {
		calls, err := a.Calls(ctx, "user1@example.com", d1.Add(-48*time.Hour), d2.Add(48*time.Hour), 100, CallFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var mm int
		if err := a.DB.QueryRowContext(ctx, strings.Replace(auditsql.MarkMissingLast24h, "now() - INTERVAL 24 HOUR", "'2026-10-01 00:00:00'", 1)).Scan(&mm); err != nil {
			t.Fatal(err)
		}
		return len(calls), mm
	}
	// A person in two groups, two requests across midnight, a router request, an empty group list, a missing key.
	insertLog(t, a, d1, llm("user1@example.com", `["eng","ml-research"]`, "claude-sonnet", "", 1000, 200))
	insertLog(t, a, d2, llm("user1@example.com", `["eng","ml-research"]`, "claude-sonnet", "claude-sonnet", 2000, 300)) // router equals model: direct
	insertLog(t, a, d2, llm("user1@example.com", `["eng"]`, "gemma", "auto", 500, 50))                                  // routed
	insertLog(t, a, d2, llm("user2@example.com", `[]`, "gemma", "", 70, 7))                                             // no group
	insertLog(t, a, d2, llm("user3@example.com", "", "gemma", "", 9999, 9999))                                          // no jwt.groups key: in neither
	insertLog(t, a, d2, map[string]string{"mcp.method.name": "tools/call", "mcp.target": "crm", "jwt.email": "user1@example.com", "http.status": "200", "jwt.groups": `["eng"]`, "ambit.key_id": "k1"})
	callsBefore, mmBefore := audit()
	used := func() time.Time {
		got, err := a.KeysLastUsed(ctx, []string{"k1"}, d1.Add(-48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return got["k1"]
	}
	usedBefore := used()

	// Guard records name the caller like the gateway's record of the same call (#203): email, subject, key ID.
	person := func(m map[string]string) map[string]string {
		m["jwt.email"], m["jwt.sub"], m["ambit.key_id"] = "user1@example.com", "sub-of-user1", "k1"
		return m
	}
	insertLog(t, a, d1, guardRecord("crm", "json", `["eng","ml-research"]`, 29000, 10000))
	insertLog(t, a, d2, person(guardRecord("crm", "json", `["eng"]`, 29000, 10000)))
	insertLog(t, a, d2, guardRecord("crm", "logs", `[]`, 24000, 1000))
	insertLog(t, a, d2.Add(time.Hour), person(guardRecord("crm", "logs", "", 24000, 1000))) // a record without groups is not a savings row
	// Any other record kind that names the person (none does today) is not a call either.
	insertLog(t, a, d2.Add(time.Hour), map[string]string{"ambit.kind": AppRecordKind, "ambit.app.action": "approved", "jwt.email": "user1@example.com", "jwt.sub": "sub-of-user1", "ambit.key_id": "k1"})

	// T10: Audit, key last use and the mark-missing check give the same answer with guard and console records present.
	if callsAfter, mmAfter := audit(); callsAfter != callsBefore || mmAfter != mmBefore {
		t.Errorf("guard records changed the audit: calls %d -> %d, mark-missing %d -> %d", callsBefore, callsAfter, mmBefore, mmAfter)
	}
	if usedAfter := used(); !usedAfter.Equal(usedBefore) || usedBefore.IsZero() {
		t.Errorf("guard records changed the key's last use: %v -> %v", usedBefore, usedAfter)
	}

	// T1: the LLM summary equals the Usage query for requests and tokens, per day, group and model.
	prices := testPrices
	usage, err := a.usageScan(ctx, d1.Add(-time.Hour), d2.Add(time.Hour), prices)
	if err != nil {
		t.Fatal(err)
	}
	// Issue #215: the Usage page reads the summary and gives the same answer as the scan of otel_logs. The records are
	// older than the views, so the summary counts from the oldest row and answers every day.
	fromSummary, err := a.Usage(ctx, d1.Add(-time.Hour), d2.Add(time.Hour), prices)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromSummary, usage) || len(usage) == 0 {
		t.Errorf("Usage from the summary and from the scan differ:\n summary %+v\n scan    %+v", fromSummary, usage)
	}
	if cov, err := a.summaryCover(ctx, llmTable, llmView); err != nil || cov.Since.Format("2006-01-02") != "2026-10-30" || cov.Full.Format("2006-01-02") != "2026-10-30" {
		t.Errorf("cover %+v %v: want the oldest row, 2026-10-30", cov, err)
	}
	llmRows, compRows, since, err := a.SummaryRows(ctx, d1.Add(-24*time.Hour), d2.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if since != "2026-10-30" { // the oldest summary row, which is older than the views
		t.Errorf("since %q, want 2026-10-30", since)
	}
	type k struct{ day, group string }
	fromView, fromUsage := map[k][3]int64{}, map[k][3]int64{}
	for _, r := range llmRows {
		if r.Group == "" {
			continue
		}
		e := fromView[k{r.Day, r.Group}]
		fromView[k{r.Day, r.Group}] = [3]int64{e[0] + int64(r.Requests), e[1] + r.In, e[2] + r.Out}
	}
	for _, u := range usage {
		fromUsage[k{u.Day, u.Group}] = [3]int64{int64(u.Requests), u.InputTokens, u.OutputTokens}
	}
	if fmt.Sprint(fromView) != fmt.Sprint(fromUsage) || len(fromUsage) == 0 {
		t.Errorf("summary and Usage differ:\n view  %v\n usage %v", fromView, fromUsage)
	}
	// T2: a person in two groups counts in both group rows and once in the all-groups row.
	var all, eng, ml int64
	for _, r := range llmRows {
		if r.Router != "" {
			continue
		}
		switch {
		case r.Group == "" && r.Model == "claude-sonnet":
			all += int64(r.Requests)
		case r.Group == "eng" && r.Model == "claude-sonnet":
			eng += int64(r.Requests)
		case r.Group == "ml-research" && r.Model == "claude-sonnet":
			ml += int64(r.Requests)
		}
	}
	if all != 2 || eng != 2 || ml != 2 {
		t.Errorf("claude-sonnet requests: all %d eng %d ml-research %d, want 2 2 2", all, eng, ml)
	}
	s := aggregateSavings(llmRows, compRows, prices, []RouterInfo{router("auto", "small", "sonnet")}, SavingsSettings{})
	if s.Totals.Compression.Results != 3 || len(s.Groups) < 3 {
		t.Errorf("compression totals %+v groups %+v", s.Totals.Compression, s.Groups)
	}
	// T7: only the request with a router name that differs from the model is a router row; direct requests add nothing.
	if s.Totals.Routing.Requests != 1 || len(s.Routers) != 1 || s.Routers[0].Router != "auto" {
		t.Errorf("routing %+v routers %+v", s.Totals.Routing, s.Routers)
	}
	// Day boundary: 23:59:59 and 00:00:00 are different UTC days.
	days := map[string]bool{}
	for _, r := range llmRows {
		days[r.Day] = true
	}
	if !days["2026-10-30"] || !days["2026-10-31"] || len(days) != 2 {
		t.Errorf("days %v", days)
	}
	// The empty group list is "(no group)" in both summaries; the record without jwt.groups is in neither.
	var noGroup, noKey int
	for _, r := range llmRows {
		if r.Group == "(no group)" {
			noGroup++
		}
	}
	for _, r := range compRows {
		if r.Group == "(no group)" {
			noKey += int(r.Results)
		}
	}
	if noGroup != 1 || noKey != 1 {
		t.Errorf("(no group) rows: llm %d, compression results %d", noGroup, noKey)
	}
	var ttl string
	if err := a.DB.QueryRowContext(ctx, "SELECT create_table_query FROM system.tables WHERE database = currentDatabase() AND name = '"+llmTable+"'").Scan(&ttl); err != nil || !strings.Contains(ttl, "toIntervalDay(400)") {
		t.Errorf("the changed retention was not applied: %v %s", err, ttl)
	}
	_ = sql.ErrNoRows

	// #216: a routed record with the classifier's tokens is in the classifier summary, once in the all-groups row and
	// once per group; a record without them is not.
	cl := llm("user1@example.com", `["eng","ml-research"]`, "gemma", "auto", 500, 50)
	cl["ambit.classifier.input_tokens"], cl["ambit.classifier.output_tokens"] = "300", "2"
	insertLog(t, a, d2, cl)
	rowsC, csince, err := a.ClassifierRows(ctx, d1, d2)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SavingsClassifierRow{}
	for _, r := range rowsC {
		got[r.Group] = r
	}
	if len(got) != 3 || got[""] != (SavingsClassifierRow{Day: "2026-10-31", Router: "auto", Calls: 1, In: 300, Out: 2}) || got["eng"].Calls != 1 || got["ml-research"].Calls != 1 || csince == "" {
		t.Errorf("classifier rows %+v since %q", rowsC, csince)
	}
}

// With records of today only, the summary counts from the day the views were created, and that day is partial: the
// Usage page reads it from otel_logs, so a record written before the views existed would still show (issue #215).
func TestSinceIsTheViewCreationDay(t *testing.T) {
	a := clickhouseTestDB(t, "savings_since")
	ctx := context.Background()
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	before := now.Add(-time.Hour)
	if before.Before(today) { // between 00:00 and 01:00 UTC: stay on the same UTC day (#258)
		before = today
	}
	insertLog(t, a, before, llm("user1@example.com", `["eng"]`, "gemma", "", 100, 10)) // before the views exist
	if err := EnsureSummaries(ctx, a.DB, 365); err != nil {
		t.Fatal(err)
	}
	insertLog(t, a, now, llm("user2@example.com", `["eng"]`, "gemma", "", 200, 20))
	cov, err := a.summaryCover(ctx, llmTable, llmView, compressTable, compressView)
	if err != nil || !cov.Since.Equal(today) || !cov.Full.Equal(today.AddDate(0, 0, 1)) {
		t.Fatalf("cover %+v %v, want since %s and full %s", cov, err, today, today.AddDate(0, 0, 1))
	}
	u, err := a.Usage(ctx, now.AddDate(0, 0, -3), now.Add(time.Minute), nil)
	if err != nil || len(u) != 1 || u[0].Requests != 2 || u[0].InputTokens != 300 {
		t.Fatalf("Usage %+v %v: want both records, the one before the views too", u, err)
	}
}

// #203 on a real ClickHouse: compression records annotate the Audit row of their call, by call ID and person, and are
// never rows themselves. Two parallel calls of one tool get their own records; a duplicate is shown once; a record
// written after the period's end still annotates its call; records without an email (compressionAudit.caller false),
// of another person, or for a call not shown, annotate nothing; calls from before 0.5.0 (no call ID) get nothing.
func TestAuditCompressionOnClickHouse(t *testing.T) {
	a := clickhouseTestDB(t, "audit_compress")
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	call := func(id string) map[string]string {
		m := map[string]string{"mcp.method.name": "tools/call", "mcp.target": "crm", "gen_ai.tool.name": "get_log", "jwt.email": "user1@example.com",
			"http.response.status_code": "200", "jwt.groups": `["eng"]`}
		if id != "" {
			m["ambit.call_id"] = id
		}
		return m
	}
	rec := func(id, email, part, typ string, in, out, omitted int) map[string]string {
		m := guardRecord("crm", typ, `["eng"]`, in, out)
		m["ambit.compress.mode"], m["ambit.compress.part"], m["ambit.compress.omitted"], m["ambit.call_id"], m["gen_ai.tool.name"] = "shorten", part, fmt.Sprint(omitted), id, "get_log"
		if email != "" {
			m["jwt.email"] = email
		}
		return m
	}
	insertLog(t, a, t0, call("c_a"))
	insertLog(t, a, t0, call("c_b")) // the same tool at the same moment
	insertLog(t, a, t0.Add(time.Second), call("c_c"))
	insertLog(t, a, t0.Add(2*time.Second), call(""))
	insertLog(t, a, t0.Add(3*time.Second), call("c_e"))
	insertLog(t, a, t0.Add(59*time.Second), call("c_late"))
	insertLog(t, a, t0.Add(time.Second), rec("c_a", "user1@example.com", "0", "logs", 38093, 10035, 14))
	insertLog(t, a, t0.Add(time.Second), rec("c_a", "user1@example.com", "1", "json", 500, 400, 0))
	insertLog(t, a, t0.Add(2*time.Second), rec("c_a", "user1@example.com", "0", "logs", 38093, 10035, 14)) // delivered twice
	insertLog(t, a, t0.Add(time.Second), rec("c_b", "user1@example.com", "0", "logs", 20000, 5000, 3))
	insertLog(t, a, t0.Add(4*time.Second), rec("c_e", "", "0", "logs", 20000, 5000, 3))                  // caller false
	insertLog(t, a, t0.Add(4*time.Second), rec("c_c", "user2@example.com", "0", "logs", 20000, 5000, 3)) // another person
	insertLog(t, a, t0.Add(4*time.Second), rec("c_none", "user1@example.com", "0", "logs", 1, 1, 0))     // gateway record lost
	insertLog(t, a, t0.Add(61*time.Second), rec("c_late", "user1@example.com", "0", "json", 900, 300, 0))

	calls, err := a.Calls(ctx, "user1@example.com", t0.Add(-time.Hour), t0.Add(time.Minute), 100, CallFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 {
		t.Fatalf("%d rows, want the 6 gateway calls: %+v", len(calls), calls)
	}
	got := map[string]*Compression{}
	for _, c := range calls {
		got[c.CallID] = c.Compression
	}
	want := map[string]*Compression{
		"c_a": {Mode: "shorten", BytesIn: 38593, BytesOut: 10435, Match: "call-id", Parts: []CompressionPart{
			{Part: 0, Type: "logs", BytesIn: 38093, BytesOut: 10035, Omitted: 14}, {Part: 1, Type: "json", BytesIn: 500, BytesOut: 400}}},
		"c_b":    {Mode: "shorten", BytesIn: 20000, BytesOut: 5000, Match: "call-id", Parts: []CompressionPart{{Part: 0, Type: "logs", BytesIn: 20000, BytesOut: 5000, Omitted: 3}}},
		"c_c":    nil,
		"":       nil,
		"c_e":    nil,
		"c_late": {Mode: "shorten", BytesIn: 900, BytesOut: 300, Match: "call-id", Parts: []CompressionPart{{Part: 0, Type: "json", BytesIn: 900, BytesOut: 300}}},
	}
	if !reflect.DeepEqual(got, want) {
		for id := range want {
			t.Errorf("%s: got %+v, want %+v", id, got[id], want[id])
		}
	}
}
