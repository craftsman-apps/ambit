package console

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeCH answers the export's queries the way ClickHouse would, by query text. Calls rows come from callRow.
type fakeCH struct {
	mu        sync.Mutex
	engine    string    // system.tables.engine_full of otel_logs
	total     int64     // records in otel_logs
	oldest    time.Time // the oldest record
	count     int64     // the answer of the export's count
	countErr  error
	calls     int // rows of the calls query
	callRow   func(i int) []driver.Value
	failAfter int           // > 0: the calls query fails after this many rows
	blockAt   int           // > 0: the calls query waits for its context to end after this many rows
	blocked   chan struct{} // closed when it starts waiting
	canceled  chan struct{} // closed when a query saw its context end
	app       [][]driver.Value
	compress  [][]driver.Value // rows of evidenceCompressionSQL: the first (day's) query gets them all
	subjects  []string
	queries   []string
	snapshots [][]driver.Value   // access snapshots: time, rules version, hash, mode, body (oldest first)
	seen      [][]driver.Value   // rows of peopleSeenSQL
	stats     [][]driver.Value   // answers of offboardStatsSQL, in call order
	after     [][][]driver.Value // rows of offboardCallsAfterSQL, in call order
}

// snapshotsIn are the snapshots before the query's {end} parameter, and from its {start} parameter when withStart
// (read from the context: the test database sees no query parameters).
func (f *fakeCH) snapshotsIn(ctx context.Context, withStart bool) [][]driver.Value {
	p, _ := ctx.Value(paramsKey{}).(clickhouse.Parameters)
	at := func(k string) time.Time {
		t, err := time.Parse("2006-01-02 15:04:05.000000000", p[k])
		if err != nil {
			panic("fakeCH: no " + k + " parameter")
		}
		return t
	}
	end := at("end")
	var out [][]driver.Value
	for _, s := range f.snapshots {
		if t := s[0].(time.Time); t.Before(end) && (!withStart || !t.Before(at("start"))) {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeCH) Connect(context.Context) (driver.Conn, error) { return fakeConn{f}, nil }
func (f *fakeCH) Driver() driver.Driver                        { return nil }

type fakeConn struct{ f *fakeCH }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func (c fakeConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	one := func(cols int, v ...driver.Value) *fakeRows {
		return &fakeRows{ctx: ctx, f: f, cols: cols, n: 1, row: func(int) []driver.Value { return v }}
	}
	switch {
	case strings.Contains(q, "system.tables"):
		return one(1, f.engine), nil
	case q == oldestRecordSQL:
		return one(2, f.total, f.oldest), nil
	case q == subsSQL:
		return &fakeRows{ctx: ctx, f: f, cols: 1, n: len(f.subjects), row: func(i int) []driver.Value { return []driver.Value{f.subjects[i]} }}, nil
	case q == evidenceCountSQL:
		if f.countErr != nil {
			return nil, f.countErr
		}
		return one(1, f.count), nil
	case q == evidenceCallsSQL: // one query per day: the first gets every row
		f.mu.Lock()
		n := f.calls
		f.calls = 0
		f.mu.Unlock()
		return &fakeRows{ctx: ctx, f: f, cols: len(callsHeader), n: n, row: f.callRow, failAfter: f.failAfter, blockAt: f.blockAt}, nil
	case q == firstSnapshotSQL:
		if len(f.snapshots) == 0 {
			return one(2, int64(0), time.Unix(0, 0)), nil
		}
		return one(2, int64(len(f.snapshots)), f.snapshots[0][0]), nil
	case q == snapshotInForceSQL: // the newest snapshot before {end}
		in := f.snapshotsIn(ctx, false)
		if len(in) == 0 {
			return &fakeRows{ctx: ctx, f: f, cols: 5}, nil
		}
		return one(5, in[len(in)-1]...), nil
	case q == snapshotsOfDaySQL: // those in [{start}, {end})
		in := f.snapshotsIn(ctx, true)
		return &fakeRows{ctx: ctx, f: f, cols: 4, n: len(in), row: func(i int) []driver.Value { return in[i][:4] }}, nil
	case q == peopleSeenSQL:
		return &fakeRows{ctx: ctx, f: f, cols: 5, n: len(f.seen), row: func(i int) []driver.Value { return f.seen[i] }}, nil
	case q == offboardStatsSQL:
		f.mu.Lock()
		v := f.stats[0]
		f.stats = f.stats[1:]
		f.mu.Unlock()
		return one(3, v...), nil
	case q == offboardCallsAfterSQL:
		f.mu.Lock()
		rows := f.after[0]
		f.after = f.after[1:]
		f.mu.Unlock()
		return &fakeRows{ctx: ctx, f: f, cols: len(callsHeader), n: len(rows), row: func(i int) []driver.Value { return rows[i] }}, nil
	case q == evidenceCompressionSQL:
		f.mu.Lock()
		rows := f.compress
		f.compress = nil
		f.mu.Unlock()
		return &fakeRows{ctx: ctx, f: f, cols: len(compressionHeader), n: len(rows), row: func(i int) []driver.Value { return rows[i] }}, nil
	case q == evidenceAppSQL:
		return &fakeRows{ctx: ctx, f: f, cols: 13, n: len(f.app), row: func(i int) []driver.Value { return f.app[i] }}, nil
	}
	return nil, fmt.Errorf("fakeCH: unexpected query %.80s", q)
}

type fakeRows struct {
	ctx                context.Context
	f                  *fakeCH
	cols, n, i         int
	row                func(int) []driver.Value
	failAfter, blockAt int
}

func (r *fakeRows) Columns() []string { return make([]string, r.cols) }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.ctx.Err() != nil {
		r.f.cancel()
		return r.ctx.Err()
	}
	if r.failAfter > 0 && r.i == r.failAfter {
		return errors.New("clickhouse: connection reset")
	}
	if r.blockAt > 0 && r.i == r.blockAt {
		close(r.f.blocked)
		<-r.ctx.Done()
		r.f.cancel()
		return r.ctx.Err()
	}
	if r.i >= r.n {
		return io.EOF
	}
	copy(dest, r.row(r.i))
	r.i++
	return nil
}

func (f *fakeCH) cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.canceled != nil {
		select {
		case <-f.canceled:
		default:
			close(f.canceled)
		}
	}
}

var evT0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// seededCall is gateway record i: a model call by ana, every 7th an invalid key, every 5th an audit-mode record
// without a rules version, with adversarial text in the fields a person or a config controls.
func seededCall(i int) []driver.Value {
	email, identity, client, rules, mode, status, errText := "ana@example.com", "jwt", "", "3f9a1c0e2b7d", "enforce", "200", ""
	tool, method := "", ""
	switch {
	case i%7 == 3:
		email, identity, client, status, errText = "", "invalid key", "10.0.0.7", "403", "=HYPERLINK(\"http://evil\")"
	case i%5 == 1:
		rules, mode = "", "audit"
	case i%11 == 2:
		method, tool, status, errText = "tools/call", "@SUM(A1),\"x\"\nnext", "400", "Unknown tool"
	}
	return []driver.Value{evT0.Add(time.Duration(i) * 3 * time.Hour), email, "ambit-system/ambit-llm", method, "", tool, "gpt-5", status, errText,
		int64(12), int64(3), "", "", "", "k1", identity, "sub-ana", mode, "", rules, "n/a", "/v1/chat/completions", "", client, fmt.Sprintf("call-%d", i)}
}

type evidenceFixture struct {
	srv   *httptest.Server
	ch    *fakeCH
	kube  client.Client
	auth  *Auth
	cat   *Catalog
	admin *http.Cookie
}

func newEvidenceFixture(t *testing.T, ch *fakeCH) *evidenceFixture {
	t.Helper()
	kube := fake.NewClientBuilder().Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	cat := &Catalog{Client: kube, Namespace: testNS, PublicURL: "https://console.example.com", AccessMode: "enforce",
		Keys:     &KeyService{Client: kube, Namespace: testNS, Now: time.Now},
		Evidence: &EvidenceExports{MaxRows: 1000, MaxDays: 366, Retention: "8760h", Version: "0.5.0"}}
	if ch != nil {
		cat.Audit = &Audit{DB: sql.OpenDB(ch)}
	}
	srv := httptest.NewServer(NewServer(auth, testSPA, LoginOptions{}, cat))
	t.Cleanup(srv.Close)
	return &evidenceFixture{srv: srv, ch: ch, kube: kube, auth: auth, cat: cat, admin: as(auth, RoleAdmin)}
}

func (e *evidenceFixture) get(t *testing.T, ctx context.Context, path string, ck *http.Cookie) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	if ck != nil {
		req.AddCookie(ck)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

// history is the export events in the key history, oldest first.
func (e *evidenceFixture) history(t *testing.T) []exportEvent {
	t.Helper()
	var l corev1.ConfigMapList
	if err := e.kube.List(t.Context(), &l, client.MatchingLabels{keyEventLabel: "true"}); err != nil {
		t.Fatal(err)
	}
	var out []exportEvent
	for _, cm := range l.Items {
		if cm.Data["action"] != evidenceAction {
			continue
		}
		var d exportEvent
		if err := json.Unmarshal([]byte(cm.Data["detail"]), &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func goodCH(calls int) *fakeCH {
	return &fakeCH{engine: "MergeTree PARTITION BY toDate(TimestampTime) ORDER BY (ServiceName, TimestampTime) TTL TimestampTime + toIntervalHour(8760) SETTINGS index_granularity = 8192",
		total: 5000, oldest: evT0.AddDate(0, -3, 0), count: int64(calls), calls: calls, callRow: seededCall}
}

// The cases of web/e2e/csv.spec.ts give the same cells in Go, and quotes, commas and new lines stay inside the cell.
func TestCSVCellMatchesTheWebGuard(t *testing.T) {
	if got := csvCell("=HYPERLINK(\"http://evil\")"); got != `"'=HYPERLINK(""http://evil"")"` {
		t.Errorf("formula: %s", got)
	}
	for _, p := range []string{"+1", "-1+2", "@SUM(A1)", "\tcmd", "\rx"} {
		if got := csvCell(p); !strings.HasPrefix(got, `"'`) {
			t.Errorf("%q → %s", p, got)
		}
	}
	for in, want := range map[any]string{"tools/call · echo": `"tools/call · echo"`, 42: `"42"`, -3: `"-3"`, int64(-3): `"-3"`, "": `""`,
		"a,b\r\nc\"d": "\"a,b\r\nc\"\"d\""} {
		if got := csvCell(in); got != want {
			t.Errorf("%#v → %s, want %s", in, got, want)
		}
	}
	var buf bytes.Buffer
	total := int64(0)
	w := &csvWriter{w: bufio.NewWriter(&buf), total: &total, max: 10}
	_ = w.Row("a", "b,c")
	_ = w.w.Flush()
	if buf.String() != "\"a\",\"b,c\"\r\n" {
		t.Errorf("row %q", buf.String())
	}
}

func TestExportPeriod(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	from, to, err := exportPeriod(q("from=2026-10-01&to=2026-12-31"), 366)
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dates: %v %v %v", from, to, err)
	}
	from, to, err = exportPeriod(q("from=2026-10-01T10:00:00%2B02:00&to=2026-10-01T12:00:00Z"), 366)
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("instants: %v %v %v", from, to, err)
	}
	if _, _, err := exportPeriod(q("from=2026-01-01&to=2026-12-31"), 366); err != nil { // 365 days
		t.Errorf("a year: %v", err)
	}
	for _, bad := range []string{"", "from=2026-10-01", "from=march&to=2026-10-02", "from=2026-10-02&to=2026-10-01",
		"from=2026-10-02T00:00:00Z&to=2026-10-02T00:00:00Z", "from=2025-01-01&to=2026-12-31", "from=2026-10-01&to=1'%3B DROP TABLE otel_logs"} {
		if _, _, err := exportPeriod(q(bad), 366); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if a, b := periodName(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); a != "2026-10-01" || b != "2026-12-31" {
		t.Errorf("periodName %s %s", a, b)
	}
}

func TestTTLRetention(t *testing.T) {
	for in, want := range map[string]string{
		"MergeTree ORDER BY (ServiceName, TimestampTime) TTL TimestampTime + toIntervalHour(8760) SETTINGS x": "8760h",
		"MergeTree TTL toDateTime(Timestamp) + toIntervalDay(365)":                                            "8760h",
		"MergeTree TTL TimestampTime + toIntervalMonth(13)":                                                   "13 months",
		"MergeTree ORDER BY Timestamp SETTINGS index_granularity = 8192":                                      "",
	} {
		if got := ttlRetention(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// Both endpoints: no session 401, viewer and approver 403, admin 200. A refusal sends no ZIP byte.
func TestEvidenceIsForAdminsOnly(t *testing.T) {
	e := newEvidenceFixture(t, goodCH(3))
	for _, p := range []string{"/api/evidence", "/api/evidence/audit.zip?from=2026-10-01&to=2026-10-02"} {
		for ck, want := range map[*http.Cookie]int{nil: 401, as(e.auth, RoleViewer): 403, as(e.auth, RoleApprover): 403, e.admin: 200} {
			resp, body, err := e.get(t, t.Context(), p, ck)
			if err != nil || resp.StatusCode != want {
				t.Errorf("%s as %v: %v %v", p, ck != nil, resp.StatusCode, err)
				continue
			}
			if want != 200 && (resp.Header.Get("Content-Type") == "application/zip" || bytes.HasPrefix(body, []byte("PK"))) {
				t.Errorf("%s: a refusal carried ZIP bytes", p)
			}
		}
	}
}

func TestEvidenceRefusalsBeforeTheFirstByte(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		setup func(e *evidenceFixture)
		want  int
		text  string
	}{
		{"unknown export", "/api/evidence/payroll.zip?from=2026-10-01&to=2026-10-02", nil, 400, "unknown export"},
		{"no .zip", "/api/evidence/audit?from=2026-10-01&to=2026-10-02", nil, 400, "unknown export"},
		{"bad date", "/api/evidence/audit.zip?from=2026-13-01&to=2026-12-31", nil, 400, "from"},
		{"from after to", "/api/evidence/audit.zip?from=2026-12-31&to=2026-10-01", nil, 400, "before"},
		{"over 366 days", "/api/evidence/audit.zip?from=2025-01-01&to=2026-12-31", nil, 400, "366 days"},
		{"over maxRows", "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", func(e *evidenceFixture) { e.ch.count = 1001 }, 413, "split the period"},
		{"store down", "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", func(e *evidenceFixture) { e.ch.countErr = errors.New("dial tcp: refused") }, 502, "ClickHouse"},
		{"one at a time", "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", func(e *evidenceFixture) { e.cat.Evidence.running.Store(true) }, 409, "another"},
		{"no audit store", "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", func(e *evidenceFixture) { e.cat.Audit = nil }, 404, "evidence.enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEvidenceFixture(t, goodCH(3))
			if c.setup != nil {
				c.setup(e)
			}
			resp, body, err := e.get(t, t.Context(), c.path, e.admin)
			if err != nil || resp.StatusCode != c.want || !strings.Contains(string(body), c.text) {
				t.Fatalf("%v %s (%v)", resp.StatusCode, body, err)
			}
			if resp.Header.Get("Content-Type") == "application/zip" || len(e.history(t)) != 0 {
				t.Errorf("refusal started an export: %v %v", resp.Header, e.history(t))
			}
			if c.want != 409 && e.cat.Evidence.running.Load() {
				t.Error("the export slot stayed taken")
			}
		})
	}
}

func readZip(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := map[string][]byte{}
	for i, f := range zr.File {
		rc, _ := f.Open()
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
		if f.Name == "manifest.json" && i != len(zr.File)-1 {
			t.Error("manifest.json is not the last file")
		}
	}
	return out
}

// A whole export: the ZIP opens, the manifest's rows and SHA-256 equal a recomputation from the files, every day of the
// period has a count and the counts add up to the rows, values stay data, and the history has the ZIP's hash.
func TestAuditExportZipAndManifest(t *testing.T) {
	ch := goodCH(40)
	ch.compress = [][]driver.Value{{evT0.Add(time.Hour), "call-0", "ana@example.com", "k1", "github", "list_issues", "json", "shorten", int64(0),
		int64(48000), int64(9000), int64(14), `["eng"]`}}
	ch.app = [][]driver.Value{{evT0.Add(time.Hour), "approved", "=cmd@example.com", "admin", "github", "req-1", "ana@example.com", "ghcr.io/x/y@sha256:1", "", "ok", "", `["a","b"]`, ""}}
	e := newEvidenceFixture(t, ch)
	resp, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-10-07", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	if resp.Header.Get("Content-Type") != "application/zip" || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="ambit-audit-2026-10-01-2026-10-07.zip"` {
		t.Errorf("headers %v", resp.Header)
	}
	files := readZip(t, body)
	var m manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Export != "audit" || m.GeneratedBy != "admin@example.com" || m.AmbitVersion != "0.5.0" || m.Cluster["consoleURL"] != "https://console.example.com" ||
		m.Parameters["from"] != "2026-10-01T00:00:00Z" || m.Parameters["to"] != "2026-10-08T00:00:00Z" || m.GeneratedAt.Location() != time.UTC {
		t.Errorf("manifest %+v", m)
	}
	if m.Sources["auditRetention"] != "8760h" || m.Sources["auditRetentionFrom"] != "table" || m.Sources["periodStartsInsideRetention"] != true || m.Sources["accessMode"] != "enforce" {
		t.Errorf("sources %v", m.Sources)
	}
	if q := m.Queries["calls.csv"]; !strings.Contains(q, "{from:DateTime64(9)}") || strings.Contains(q, "2026-10-01") {
		t.Errorf("query is not the parameterised SQL: %.200s", q)
	}
	names := []string{}
	for _, f := range m.Files {
		names = append(names, f.Name)
		sum := sha256.Sum256(files[f.Name])
		recs, err := csv.NewReader(bytes.NewReader(files[f.Name])).ReadAll()
		if err != nil || hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(recs)-1) != f.Rows {
			t.Errorf("%s: manifest %d rows %s; file %d rows %x (%v)", f.Name, f.Rows, f.SHA256, len(recs)-1, sum, err)
		}
	}
	if strings.Join(names, " ") != "calls.csv daily-counts.csv app-requests.csv compression.csv" {
		t.Errorf("files %v", names)
	}
	calls, _ := csv.NewReader(bytes.NewReader(files["calls.csv"])).ReadAll()
	if strings.Join(calls[0], ",") != strings.Join(callsHeader, ",") || len(calls) != 41 {
		t.Fatalf("calls: %d rows, header %v", len(calls)-1, calls[0])
	}
	col := map[string]int{}
	for i, h := range calls[0] {
		col[h] = i
	}
	var invalid, unknown, formula, multiline int
	for _, r := range calls[1:] {
		if _, err := time.Parse(time.RFC3339Nano, r[col["time"]]); err != nil || !strings.HasSuffix(r[col["time"]], "Z") {
			t.Errorf("time %q", r[col["time"]])
		}
		if r[col["identity"]] == "invalid key" {
			invalid++
			if r[col["email"]] != "" || r[col["client_address"]] != "10.0.0.7" || r[col["target"]] != "from 10.0.0.7" || r[col["outcome"]] != "Refused by policy" {
				t.Errorf("invalid-key row %v", r)
			}
		} else if r[col["client_address"]] != "" {
			t.Errorf("client address outside an invalid-key row: %v", r)
		}
		if r[col["rules_version"]] == "unknown" {
			unknown++
		}
		if strings.HasPrefix(r[col["error"]], "'=HYPERLINK") {
			formula++
		}
		if r[col["tool"]] == "'@SUM(A1),\"x\"\nnext" {
			multiline++
		}
	}
	if invalid == 0 || unknown == 0 || formula != invalid || multiline == 0 {
		t.Errorf("invalid %d unknown %d formula %d multiline %d", invalid, unknown, formula, multiline)
	}
	days, _ := csv.NewReader(bytes.NewReader(files["daily-counts.csv"])).ReadAll()
	var sum int
	for _, d := range days[1:] {
		var n int
		fmt.Sscan(d[1], &n)
		sum += n
	}
	if len(days) != 8 || days[1][0] != "2026-10-01" || days[7][0] != "2026-10-07" || sum != 40 {
		t.Errorf("daily counts %v (sum %d)", days, sum)
	}
	if calls[1][col["call_id"]] != "call-0" {
		t.Errorf("call_id %v", calls[1])
	}
	comp, _ := csv.NewReader(bytes.NewReader(files["compression.csv"])).ReadAll()
	if len(comp) != 2 || strings.Join(comp[0], ",") != strings.Join(compressionHeader, ",") ||
		strings.Join(comp[1], ",") != `2026-10-01T09:00:00Z,call-0,ana@example.com,k1,github,list_issues,json,shorten,0,48000,9000,14,["eng"]` {
		t.Errorf("compression %v", comp)
	}
	app, _ := csv.NewReader(bytes.NewReader(files["app-requests.csv"])).ReadAll()
	if len(app) != 2 || app[1][2] != "'=cmd@example.com" || app[1][9] != "a, b" {
		t.Errorf("app requests %v", app)
	}
	notes := strings.Join(m.Notes, "\n")
	for _, want := range []string{"personal data", "Audit-only mode", "does not sign", "SHA-256"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q", want)
		}
	}
	h := e.history(t)
	zipSum := sha256.Sum256(body)
	if len(h) != 2 || h[0].Result != "started" || h[1].Result != "complete" || h[1].Rows != 42 || h[1].ZipSHA256 != hex.EncodeToString(zipSum[:]) {
		t.Errorf("history %+v", h)
	}
	for _, ev := range h {
		if b, _ := json.Marshal(ev); strings.Contains(string(b), "ana@example.com") || strings.Contains(string(b), "HYPERLINK") {
			t.Errorf("history holds file contents: %s", b)
		}
	}
	// The page sees the export and the ZIP's hash; the Keys history does not list it.
	_, info, _ := e.get(t, t.Context(), "/api/evidence", e.admin)
	if !strings.Contains(string(info), hex.EncodeToString(zipSum[:])) || !strings.Contains(string(info), `"oldestRecord":"2026-07-01T08:00:00Z"`) {
		t.Errorf("GET /api/evidence: %s", info)
	}
	if ev, err := e.cat.Keys.Events(t.Context()); err != nil || len(ev) != 0 {
		t.Errorf("key history lists exports: %v %v", ev, err)
	}
}

// One person only: the email reaches the query as a parameter; the manifest names the subjects used.
func TestAuditExportForOnePerson(t *testing.T) {
	ch := goodCH(2)
	ch.subjects = []string{"sub-ana"}
	e := newEvidenceFixture(t, ch)
	resp, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-10-01&email=ana%40example.com", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp.StatusCode, err)
	}
	var m manifest
	_ = json.Unmarshal(readZip(t, body)["manifest.json"], &m)
	if m.Parameters["email"] != "ana@example.com" || m.Parameters["subjects"] != `["sub-ana"]` || !strings.Contains(strings.Join(m.Notes, " "), "Limited to ana@example.com") {
		t.Errorf("manifest %+v", m)
	}
}

// A period that starts before the oldest kept record says so.
func TestAuditExportBeforeRetention(t *testing.T) {
	ch := goodCH(1)
	ch.oldest = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	ch.engine = "MergeTree ORDER BY Timestamp" // no TTL: the Helm value is shown
	e := newEvidenceFixture(t, ch)
	_, body, _ := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-10-07", e.admin)
	var m manifest
	_ = json.Unmarshal(readZip(t, body)["manifest.json"], &m)
	if m.Sources["periodStartsInsideRetention"] != false || m.Sources["auditRetentionFrom"] != "helm" ||
		!strings.Contains(strings.Join(m.Notes, " "), "starts before the oldest record kept (2026-10-05T00:00:00Z; retention 8760h)") {
		t.Errorf("sources %v notes %v", m.Sources, m.Notes)
	}
}

// failedExport checks a failure after the first byte: the client gets a ZIP that does not open and has no manifest,
// and the history says failed with the reason.
func failedExport(t *testing.T, e *evidenceFixture, body []byte, reason string) {
	t.Helper()
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err == nil {
		t.Error("the truncated ZIP opens")
	}
	if bytes.Contains(body, []byte("manifest.json")) {
		t.Error("the truncated ZIP has a manifest")
	}
	var h []exportEvent
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		// The history is written before the handler returns and frees the export slot.
		if h = e.history(t); len(h) == 2 && !e.cat.Evidence.running.Load() {
			break
		}
	}
	if len(h) != 2 || h[1].Result != "failed" || !strings.Contains(h[1].Reason, reason) {
		t.Errorf("history %+v", h)
	}
	if e.cat.Evidence.running.Load() {
		t.Error("the export slot stayed taken")
	}
}

// The count passed, but more rows arrived than maxRows: the stream stops at maxRows + 1.
func TestAuditExportStopsAtTheRowLimitDuringTheStream(t *testing.T) {
	ch := goodCH(5)
	ch.calls = 1001 // the count said 5
	e := newEvidenceFixture(t, ch)
	resp, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", e.admin)
	if resp == nil || resp.StatusCode != 200 || err == nil {
		t.Fatalf("want a 200 whose body ends early: %v %v", resp, err)
	}
	failedExport(t, e, body, "maxRows")
}

// ClickHouse fails after 1,000 rows.
func TestAuditExportTruncatedByTheStore(t *testing.T) {
	ch := goodCH(5000)
	ch.count, ch.failAfter = 900, 1000
	e := newEvidenceFixture(t, ch)
	e.cat.Evidence.MaxRows = 10000
	_, body, err := e.get(t, t.Context(), "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", e.admin)
	if err == nil {
		t.Error("the body ended normally")
	}
	failedExport(t, e, body, "connection reset")
}

// The client leaves in the middle: the query's context is cancelled, and the history says failed.
func TestAuditExportClientDisconnectCancelsTheQuery(t *testing.T) {
	ch := goodCH(5000)
	ch.count, ch.blockAt = 10, 500
	ch.blocked, ch.canceled = make(chan struct{}), make(chan struct{})
	e := newEvidenceFixture(t, ch)
	e.cat.Evidence.MaxRows = 10000
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-ch.blocked; cancel() }()
	_, _, _ = e.get(t, ctx, "/api/evidence/audit.zip?from=2026-10-01&to=2026-12-31", e.admin)
	select {
	case <-ch.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the query did not see the cancel")
	}
	failedExport(t, e, nil, "closed the connection")
}

func TestCallReadingMatchesTheAuditPage(t *testing.T) {
	for _, c := range []struct {
		call                  Call
		what, target, outcome string
	}{
		{Call{Method: "tools/call", Tool: "echo", Server: "everything", Status: "200"}, "tools/call · echo", "everything", "OK"},
		{Call{Path: "/v1/models", Route: "ambit-system/ambit-llm", Status: "200"}, "Model list", "", "OK"},
		{Call{Model: "gpt-5", Router: "auto", Label: "coding", Status: "403"}, "LLM request", "auto → gpt-5 (coding)", "Refused by policy"},
		{Call{Model: "gpt-5", Status: "429", Error: "rate limit exceeded"}, "LLM request", "gpt-5", "Refused by policy"},
		{Call{Model: "gpt-5", Status: "429", Error: "provider busy"}, "LLM request", "gpt-5", "Failed"},
		{Call{Model: "gpt-5", Status: "502"}, "LLM request", "gpt-5", "Failed"},
		{Call{Model: "gpt-5", Status: "403", PromptGuard: "reject"}, "LLM request", "gpt-5", "Refused by prompt guard"},
		{Call{Model: "gpt-5", Status: "200", Outcome: "would-refuse"}, "LLM request", "gpt-5", "Would be refused"},
		{Call{Identity: "invalid key", Route: "r", Path: "/v1/chat/completions", Status: "403"}, "/v1/chat/completions", "from an unknown address", "Refused by policy"},
	} {
		if w, g, o := callWhat(c.call), callTarget(c.call), callOutcome(c.call); w != c.what || g != c.target || o != c.outcome {
			t.Errorf("%+v → %q %q %q", c.call, w, g, o)
		}
	}
}
