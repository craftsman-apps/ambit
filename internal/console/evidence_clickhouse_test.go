package console

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The audit export on a real ClickHouse (skipped unless AMBIT_TEST_CLICKHOUSE_ADDR is set; test/stepevidence.sh runs it
// in the ClickHouse pod): only gateway records are in calls.csv (compression records, access snapshots and app events
// are not), the row count equals a count() of the same period and kinds, the daily counts add up, the one-person
// filter finds the refusal logged with the subject only, and the export's settings (readonly, time limit) are accepted.
func TestEvidenceOnClickHouse(t *testing.T) {
	a := clickhouseTestDB(t, "evidence_test")
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	gw := func(email, sub string, extra map[string]string) map[string]string {
		m := map[string]string{"protocol": "llm", "jwt.email": email, "jwt.sub": sub, "route": "ambit-system/ambit-llm", "gen_ai.request.model": "gpt-5",
			"http.status": "200", "ambit.accessMode": "enforce", "ambit.rulesVersion": "3f9a1c0e2b7d", "ambit.identity": "jwt", "url.path": "/v1/chat/completions"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	insertLog(t, a, day, gw("ana@example.com", "sub-ana", nil))
	insertLog(t, a, day.Add(time.Hour), gw("ana@example.com", "sub-ana", map[string]string{"ambit.accessMode": "audit", "ambit.rulesVersion": ""}))
	insertLog(t, a, day.Add(25*time.Hour), gw("ana@example.com", "sub-ana", map[string]string{"error": "=HYPERLINK(\"x\")", "http.status": "502"}))
	insertLog(t, a, day.Add(26*time.Hour), gw("", "sub-ana", map[string]string{"http.status": "403"})) // a refusal with the subject only
	insertLog(t, a, day.Add(27*time.Hour), map[string]string{"ambit.identity": "invalid key", "client.address": "10.0.0.7", "http.status": "403", "url.path": "/v1/chat/completions"})
	insertLog(t, a, day.Add(50*time.Hour), gw("bob@example.com", "sub-bob", nil))
	insertLog(t, a, day.AddDate(0, 0, -2), gw("ana@example.com", "sub-ana", nil)) // before the period
	// Other record kinds in the period: never in calls.csv.
	insertLog(t, a, day.Add(2*time.Hour), map[string]string{"ambit.kind": "compression", "mcp.target": "github", "jwt.groups": `["eng"]`})
	insertLog(t, a, day.Add(3*time.Hour), map[string]string{"ambit.kind": "access-snapshot", "ambit.rulesVersion": "3f9a1c0e2b7d", "ambit.snapshot.accessMode": "enforce"})
	insertLog(t, a, day.Add(4*time.Hour), map[string]string{"ambit.kind": "app-approval", "ambit.app.action": "approved", "ambit.actor": "admin@example.com",
		"ambit.app.requested_by": "ana@example.com", "mcp.target": "github"})

	if _, err := a.DB.ExecContext(exportContext(t.Context(), nil), "INSERT INTO otel_logs (Timestamp) VALUES (now64(9))"); err == nil {
		t.Error("an export's settings allowed a write")
	}
	var want uint64
	if err := a.DB.QueryRow(`SELECT count() FROM otel_logs WHERE LogAttributes['ambit.kind'] = '' AND Timestamp >= '2026-10-01' AND Timestamp < '2026-10-04'`).Scan(&want); err != nil || want != 6 {
		t.Fatalf("seeded gateway records in the period: %d (%v)", want, err)
	}
	e := newEvidenceFixture(t, nil)
	e.cat.Audit = a
	export := func(q string) (map[string][][]string, manifest) {
		t.Helper()
		resp, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?"+q, e.admin)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v %s", q, resp.StatusCode, err, body)
		}
		files := readZip(t, body)
		var m manifest
		if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
			t.Fatal(err)
		}
		out := map[string][][]string{}
		for name, b := range files {
			if name != "manifest.json" {
				out[name], _ = csv.NewReader(bytes.NewReader(b)).ReadAll()
			}
		}
		return out, m
	}
	files, m := export("from=2026-10-01&to=2026-10-03")
	if got := len(files["calls.csv"]) - 1; got != int(want) {
		t.Errorf("calls.csv has %d rows, count() says %d: %v", got, want, files["calls.csv"])
	}
	for _, r := range files["calls.csv"][1:] {
		if r[3] == "github" {
			t.Errorf("a compression record is in calls.csv: %v", r)
		}
	}
	var sum int
	for _, d := range files["daily-counts.csv"][1:] {
		var n int
		fmt.Sscan(d[1], &n)
		sum += n
	}
	if sum != int(want) || len(files["daily-counts.csv"]) != 4 {
		t.Errorf("daily counts %v", files["daily-counts.csv"])
	}
	if len(files["app-requests.csv"]) != 2 || files["app-requests.csv"][1][1] != "approved" {
		t.Errorf("app requests %v", files["app-requests.csv"])
	}
	if m.Sources["oldestRecordKept"] == nil || m.Sources["periodStartsInsideRetention"] != true {
		t.Errorf("sources %v", m.Sources)
	}
	t.Logf("retention %v from %v", m.Sources["auditRetention"], m.Sources["auditRetentionFrom"])

	files, _ = export("from=2026-10-01&to=2026-10-03&email=ana%40example.com")
	if got := len(files["calls.csv"]) - 1; got != 4 {
		t.Errorf("ana's calls: %d rows, want 4 (3 with the email, 1 refusal with the subject only): %v", got, files["calls.csv"])
	}
	if len(files["app-requests.csv"]) != 2 {
		t.Errorf("ana's app requests: %v", files["app-requests.csv"])
	}
}

// The offboarding columns on a real ClickHouse: the grace time (a call 2 s after is not counted, one at 6 s is), every
// status except 401 and 403 counts as accepted (404, 429, 502 are counted and listed), and the first refusal after.
func TestOffboardingEvidenceOnClickHouse(t *testing.T) {
	a := clickhouseTestDB(t, "evidence_test")
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	call := func(ts time.Time, email, status string) {
		insertLog(t, a, ts, map[string]string{"jwt.email": email, "jwt.sub": "sub-" + email, "route": "ambit-system/ambit-llm", "gen_ai.request.model": "gpt-5",
			"http.status": status, "ambit.identity": "key", "ambit.key_id": "k1"})
	}
	call(at.Add(-time.Hour), "ana@example.com", "200")
	call(at.Add(2*time.Second), "ana@example.com", "200") // in flight: inside the grace time
	call(at.Add(6*time.Second), "ana@example.com", "404")
	call(at.Add(7*time.Second), "ana@example.com", "429")
	call(at.Add(8*time.Second), "ana@example.com", "502")
	call(at.Add(9*time.Second), "ana@example.com", "403") // the block
	call(at.Add(10*time.Second), "bob@example.com", "200")
	insertLog(t, a, at.Add(11*time.Second), map[string]string{"jwt.sub": "sub-ana@example.com", "http.status": "401"}) // refusal without an email

	e := newEvidenceFixture(t, nil)
	e.cat.Audit = a
	withSCIM(t, e, OffboardingRecord{Email: "ana@example.com", At: at, Trigger: "scim DELETE", GatewayBlocked: true, Grants: []GrantResult{}})
	resp, body, err := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	files, _ := csvFiles(t, body)
	row := files["offboarding.csv"][1]
	if row[9] != "2026-10-02T08:00:00Z" || row[10] != "3" || row[11] != "2026-10-02T09:00:09Z" {
		t.Errorf("offboarding.csv %v: want last before 08:00, 3 accepted after, first refusal 09:00:09", row)
	}
	var statuses []string
	for _, r := range files["calls-after.csv"][1:] {
		statuses = append(statuses, r[7])
	}
	if strings.Join(statuses, " ") != "404 429 502" {
		t.Errorf("calls-after.csv statuses %v", statuses)
	}
}
