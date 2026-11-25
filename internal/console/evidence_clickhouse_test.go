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
	insertLog(t, a, day, gw("ana@example.com", "sub-ana", map[string]string{"ambit.call_id": "c-1"}))
	insertLog(t, a, day.Add(time.Hour), gw("ana@example.com", "sub-ana", map[string]string{"ambit.accessMode": "audit", "ambit.rulesVersion": ""}))
	insertLog(t, a, day.Add(25*time.Hour), gw("ana@example.com", "sub-ana", map[string]string{"error": "=HYPERLINK(\"x\")", "http.status": "502"}))
	insertLog(t, a, day.Add(26*time.Hour), gw("", "sub-ana", map[string]string{"http.status": "403"})) // a refusal with the subject only
	insertLog(t, a, day.Add(27*time.Hour), map[string]string{"ambit.identity": "invalid key", "client.address": "10.0.0.7", "http.status": "403", "url.path": "/v1/chat/completions"})
	insertLog(t, a, day.Add(50*time.Hour), gw("bob@example.com", "sub-bob", nil))
	insertLog(t, a, day.AddDate(0, 0, -2), gw("ana@example.com", "sub-ana", nil)) // before the period
	// Other record kinds in the period: never in calls.csv.
	insertLog(t, a, day.Add(2*time.Second), map[string]string{"ambit.kind": "compression", "mcp.target": "github", "jwt.groups": `["eng"]`, "ambit.call_id": "c-1",
		"jwt.email": "ana@example.com", "ambit.compress.mode": "shorten", "ambit.compress.bytes_in": "48000", "ambit.compress.bytes_out": "9000", "ambit.compress.part": "0"})
	insertLog(t, a, day.Add(2*time.Hour), map[string]string{"ambit.kind": "compression", "mcp.target": "github", "jwt.groups": `["eng"]`}) // caller: false
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
	if c := files["compression.csv"]; len(c) != 3 || c[1][1] != "c-1" || c[1][2] != "ana@example.com" || c[1][9] != "48000" || c[1][10] != "9000" || c[2][2] != "" {
		t.Errorf("compression %v", c)
	}
	if r := files["calls.csv"][1]; r[len(r)-1] != "c-1" {
		t.Errorf("the first call's call_id: %v", r)
	}
	if len(files["app-requests.csv"]) != 2 || files["app-requests.csv"][1][1] != "approved" {
		t.Errorf("app requests %v", files["app-requests.csv"])
	}
	if m.Sources["oldestRecordKept"] == nil || m.Sources["periodStartsInsideRetention"] != true {
		t.Errorf("sources %v", m.Sources)
	}
	t.Logf("retention %v from %v", m.Sources["auditRetention"], m.Sources["auditRetentionFrom"])

	// maxRows counts the compression records too: 6 calls and 2 compression records are more than 7.
	e.cat.Evidence.MaxRows = int64(want) + 1
	if resp, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-10-03", e.admin); err != nil || resp.StatusCode != 413 ||
		!strings.Contains(string(body), "has 8 records") {
		t.Errorf("maxRows %d: %v %v %s", want+1, resp.StatusCode, err, body)
	}
	e.cat.Evidence.MaxRows = 1000

	files, _ = export("from=2026-10-01&to=2026-10-03&email=ana%40example.com")
	if got := len(files["calls.csv"]) - 1; got != 4 {
		t.Errorf("ana's calls: %d rows, want 4 (3 with the email, 1 refusal with the subject only): %v", got, files["calls.csv"])
	}
	if c := files["compression.csv"]; len(c) != 2 || c[1][1] != "c-1" {
		t.Errorf("ana's compression records: %v", c)
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

// The access export on a real ClickHouse: the snapshot in force at the end of D is the newest before it (15:00 of
// two on D), both are listed; a date before the first snapshot is a 404 that names it; people seen per group.
func TestAccessEvidenceOnClickHouse(t *testing.T) {
	a := clickhouseTestDB(t, "evidence_test")
	d := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	snap := func(at time.Time, version, body string) {
		q := fmt.Sprintf("INSERT INTO otel_logs (Timestamp, LogAttributes, Body) VALUES ('%s', {'ambit.kind':'access-snapshot','ambit.rulesVersion':%s,'ambit.rulesHash':'h-%s','ambit.snapshot.accessMode':'enforce'}, %s)",
			at.Format("2006-01-02 15:04:05.000000000"), sq(version), version, sq(body))
		if _, err := a.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	snap(d.AddDate(0, 0, -3), "old000000000", `[{"name":"r","spec":{"groups":["eng"],"models":["old"]}}]`)
	snap(d.Add(10*time.Hour), "ten000000000", `[{"name":"r","spec":{"groups":["eng"],"models":["ten"]}}]`)
	snap(d.Add(15*time.Hour), "fif000000000", `[{"name":"r","spec":{"groups":["eng"],"models":["fifteen"]}}]`)
	snap(d.AddDate(0, 0, 1).Add(time.Hour), "next00000000", `[{"name":"r","spec":{"groups":["eng"],"models":["next"]}}]`)
	insertLog(t, a, d.Add(-24*time.Hour), map[string]string{"jwt.email": "ana@example.com", "jwt.groups": `["eng","ml"]`, "http.status": "200"})
	insertLog(t, a, d.Add(-48*time.Hour), map[string]string{"jwt.email": "ana@example.com", "jwt.groups": `["eng"]`, "http.status": "403"})
	insertLog(t, a, d.AddDate(0, 0, -100), map[string]string{"jwt.email": "bob@example.com", "jwt.groups": `["eng"]`, "http.status": "200"}) // over 90 days before

	e := newEvidenceFixture(t, nil)
	e.cat.Audit = a
	resp, body, err := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	files, m := csvFiles(t, body)
	if g := files["grants.csv"]; len(g) != 2 || g[1][0] != "2026-12-31T15:00:00Z" || g[1][1] != "fif000000000" || g[1][7] != "fifteen" {
		t.Errorf("grants.csv %v", g)
	}
	ofDay, _ := json.Marshal(m.Sources["snapshotsOfTheDay"])
	if !strings.Contains(string(ofDay), "ten000000000") || !strings.Contains(string(ofDay), "fif000000000") || strings.Contains(string(ofDay), "old0") {
		t.Errorf("snapshots of the day %s", ofDay)
	}
	var seen []string
	for _, r := range files["people-seen.csv"][1:] {
		seen = append(seen, r[0]+" "+r[1]+" "+r[4])
	}
	if strings.Join(seen, ", ") != "eng ana@example.com 2, ml ana@example.com 1" {
		t.Errorf("people-seen.csv %v", seen)
	}
	// at=T: the newest snapshot at or before T (also one from an earlier day); a snapshot at exactly T counts.
	for at, want := range map[string]string{"2026-12-31T09:00:00Z": "old000000000", "2026-12-31T10:00:00Z": "ten000000000", "2026-12-31T14:59:59.999999999Z": "ten000000000", "2026-12-31T15:00:00Z": "fif000000000"} {
		resp, body, err := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31&at="+at, e.admin)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("at=%s: %v %v %s", at, resp.StatusCode, err, body)
		}
		if files, _ := csvFiles(t, body); len(files["grants.csv"]) != 2 || files["grants.csv"][1][1] != want {
			t.Errorf("at=%s: grants.csv %v, want %s", at, files["grants.csv"], want)
		}
	}
	_, body, _ = e.get(t, t.Context(), "/api/evidence/access/snapshots?date=2026-12-31", e.admin)
	if !strings.Contains(string(body), `"rulesVersion":"ten000000000"`) || !strings.Contains(string(body), `"rulesVersion":"fif000000000"`) || strings.Contains(string(body), "old0") || strings.Contains(string(body), "next0") {
		t.Errorf("snapshots of the day %s", body)
	}
	resp, body, _ = e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-01", e.admin)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "Snapshots exist from 2026-12-28") {
		t.Errorf("before the first snapshot: %d %s", resp.StatusCode, body)
	}
}
