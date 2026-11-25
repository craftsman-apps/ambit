package console

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/craftsman-apps/ambit/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Evidence exports (issue #89, Settings → Evidence): one ZIP per export with CSV files and a manifest.json that names
// how the file was made, the rows of each file and their SHA-256, so an auditor can test that it is complete and
// unchanged. Admins only. The ZIP is written straight to the response: rows go from ClickHouse through the CSV writer
// into the ZIP, and the code holds no list of rows.

// EvidenceExports is the configuration of the exports (Helm evidence.exports.*); nil turns the page off.
type EvidenceExports struct {
	MaxRows   int64  // an export with more rows is refused (413) or, when rows keep arriving, fails
	MaxDays   int    // the longest period of one export
	Retention string // evidence.retention, shown only when the table's TTL cannot be read
	Version   string // the Ambit version, for the manifest
	Now       func() time.Time

	running atomic.Bool // one export at a time: the console runs as one replica (console.replicas must be 1)
}

func (e *EvidenceExports) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// exportLimit is the longest an export may run; ClickHouse stops each query after the same time.
const exportLimit = 30 * time.Minute

// evidenceAction is the history action of an export (a key-history ConfigMap, kept 400 days).
const evidenceAction = "evidence-export"

// Notes in every manifest.
var (
	noteUTC      = "Times are UTC (RFC 3339)."
	noteSigned   = "Records are written by the gateway and Ambit's components; Ambit does not sign them at the source. Anyone with write access to ClickHouse can change records before an export."
	notePersonal = "This file contains personal data. Store and share it under your company's data rules. Ambit's retention does not apply to downloaded files."
	noteHashes   = "The SHA-256 of each file detects a change or damage of the file after the export. It does not prove that the records were never changed. Send the ZIP's SHA-256 (shown in Settings → Evidence) to the auditor when you hand over the file."
)

// exportErr is a refusal before the first byte: its status and message are the answer.
type exportErr struct {
	status int
	msg    string
}

func (e *exportErr) Error() string { return e.msg }

func refuse(status int, format string, a ...any) error {
	return &exportErr{status, fmt.Sprintf(format, a...)}
}

// exportFile is one CSV file of an export, written in order.
type exportFile struct {
	name   string
	header []string
	rows   func(ctx context.Context, w *csvWriter) error // writes the data rows
}

// exportJob is a prepared export: everything that can fail with a status has been checked.
type exportJob struct {
	export     string
	parameters map[string]string
	sources    map[string]any
	queries    map[string]string // file name → the exact SQL, with parameter names and no values
	notes      []string
	files      []exportFile
	filename   string // the ZIP's name: export and validated dates only
	period     string // for the history: "2026-10-01 to 2026-12-31" or "2026-12-31"
	// finish runs after the last CSV file and may add notes from what the rows showed.
	finish func(j *exportJob)
}

type manifestFile struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Export       string            `json:"export"`
	AmbitVersion string            `json:"ambitVersion"`
	GeneratedAt  time.Time         `json:"generatedAt"`
	GeneratedBy  string            `json:"generatedBy"`
	Cluster      map[string]string `json:"cluster"`
	Parameters   map[string]string `json:"parameters"`
	Sources      map[string]any    `json:"sources"`
	Files        []manifestFile    `json:"files"`
	Queries      map[string]string `json:"queries"`
	Notes        []string          `json:"notes"`
}

// csvWriter writes RFC 4180 CSV (every cell quoted, CRLF) with the formula guard of web/src/lib/csv.ts, and counts the
// data rows of the export against its limit.
type csvWriter struct {
	w     *bufio.Writer
	rows  int64  // data rows of this file
	total *int64 // data rows of the export so far
	max   int64
}

// errTooManyRows stops an export whose rows grew past the limit after the count (records keep arriving).
var errTooManyRows = errors.New("more rows than evidence.exports.maxRows arrived while the export ran")

// csvCell quotes one cell. A string that a spreadsheet would run as a formula (starting with = + - @, a tab or a
// carriage return) gets a leading apostrophe; numbers stay numbers.
func csvCell(v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
		if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
			s = "'" + s
		}
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case uint64:
		s = strconv.FormatUint(x, 10)
	default:
		panic(fmt.Sprintf("csvCell: %T", v))
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (c *csvWriter) line(cells []any) error {
	for i, v := range cells {
		if i > 0 {
			c.w.WriteByte(',')
		}
		c.w.WriteString(csvCell(v))
	}
	_, err := c.w.WriteString("\r\n")
	return err
}

// Row writes one data row; it fails once the export has more rows than its limit.
func (c *csvWriter) Row(cells ...any) error {
	c.rows++
	if *c.total++; *c.total > c.max {
		return errTooManyRows
	}
	return c.line(cells)
}

// csvTime is a time cell: UTC RFC 3339, empty for the zero time.
func csvTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseDay reads YYYY-MM-DD or RFC 3339. end is true for the end of a period: a date is a whole UTC day, so the end
// of 2026-12-31 is 2027-01-01T00:00:00Z.
func parseDay(v string, end bool) (time.Time, bool, error) {
	if t, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			t = t.AddDate(0, 0, 1)
		}
		return t, true, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t.UTC(), false, err
}

// exportPeriod reads from and to (both required; dates are inclusive whole UTC days, RFC 3339 times are exact
// instants with to exclusive) and checks the limit of maxDays.
func exportPeriod(q url.Values, maxDays int) (from, to time.Time, err error) {
	fs, ts := q.Get("from"), q.Get("to")
	if fs == "" || ts == "" {
		return from, to, refuse(http.StatusBadRequest, "give from and to, as 2026-10-01 or RFC 3339")
	}
	if from, _, err = parseDay(fs, false); err != nil {
		return from, to, refuse(http.StatusBadRequest, "from: use 2026-10-01 or RFC 3339")
	}
	if to, _, err = parseDay(ts, true); err != nil {
		return from, to, refuse(http.StatusBadRequest, "to: use 2026-12-31 or RFC 3339")
	}
	if !from.Before(to) {
		return from, to, refuse(http.StatusBadRequest, "from must be before to")
	}
	if to.Sub(from) > time.Duration(maxDays)*24*time.Hour {
		return from, to, refuse(http.StatusBadRequest, "the period is longer than %d days (evidence.exports.maxDays): split it", maxDays)
	}
	return from, to, nil
}

// periodName is the dates of [from, to) for file names and the page: the first day and the last day included.
func periodName(from, to time.Time) (string, string) {
	return from.UTC().Format("2006-01-02"), to.UTC().Add(-time.Nanosecond).Format("2006-01-02")
}

type paramsKey struct{}

// exportContext has the ClickHouse settings of every export query: a time limit, and read-only.
func exportContext(ctx context.Context, params clickhouse.Parameters) context.Context {
	ctx = context.WithValue(ctx, paramsKey{}, params) // for the test database, which cannot read ClickHouse's own copy
	return clickhouse.Context(ctx, clickhouse.WithParameters(params),
		clickhouse.WithSettings(clickhouse.Settings{"max_execution_time": int(exportLimit.Seconds()), "readonly": 1}))
}

// chTime is a DateTime64(9) parameter value.
func chTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000000") }

var ttlRe = regexp.MustCompile(`TTL .*?toInterval(Second|Minute|Hour|Day|Week|Month|Quarter|Year)\((\d+)\)`)

// ttlRetention reads the retention from the CREATE statement of otel_logs (system.tables.engine_full), e.g.
// "TTL toDateTime(Timestamp) + toIntervalHour(8760)" → "8760h"; "" when there is none.
func ttlRetention(engineFull string) string {
	m := ttlRe.FindStringSubmatch(engineFull)
	if m == nil {
		return ""
	}
	n, _ := strconv.Atoi(m[2])
	hours := map[string]int{"Hour": 1, "Day": 24, "Week": 168}
	switch m[1] {
	case "Second", "Minute":
		return m[2] + map[string]string{"Second": "s", "Minute": "m"}[m[1]]
	case "Month", "Quarter", "Year":
		return m[2] + " " + strings.ToLower(m[1]) + "s"
	}
	return strconv.Itoa(n*hours[m[1]]) + "h"
}

// auditSource is what the audit store can show: its retention (from the table, or the Helm value), the oldest record
// kept, and whether a period starting at from starts inside it.
type auditSource struct {
	Retention     string     `json:"auditRetention"`
	RetentionFrom string     `json:"auditRetentionFrom"` // "table" or "helm"
	Oldest        *time.Time `json:"oldestRecordKept"`
}

const oldestRecordSQL = "SELECT count(), min(Timestamp) FROM otel_logs"

func (c *Catalog) auditSource(ctx context.Context) (auditSource, error) {
	src := auditSource{Retention: c.Evidence.Retention, RetentionFrom: "helm"}
	var engine string
	err := c.Audit.DB.QueryRowContext(exportContext(ctx, nil), "SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = 'otel_logs'").Scan(&engine)
	if err != nil {
		return src, err
	}
	if r := ttlRetention(engine); r != "" {
		src.Retention, src.RetentionFrom = r, "table"
	}
	var n uint64
	var oldest time.Time
	if err := c.Audit.DB.QueryRowContext(exportContext(ctx, nil), oldestRecordSQL).Scan(&n, &oldest); err != nil {
		return src, err
	}
	if n > 0 {
		o := oldest.UTC()
		src.Oldest = &o
	}
	return src, nil
}

// insideRetention says only that the period starts after the oldest kept record, not that no record is missing.
func (s auditSource) insideRetention(from time.Time) bool {
	return s.Oldest != nil && !from.Before(*s.Oldest)
}

// publishedMode is the access mode the controller published ("" before it has).
func (c *Catalog) publishedMode(ctx context.Context) string {
	var cm corev1.ConfigMap
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: controller.AccessModeConfigMap}, &cm); err != nil {
		return c.AccessMode
	}
	return cm.Data["mode"]
}

// exportEvent is the detail of one export history event.
type exportEvent struct {
	ID        string `json:"id"`
	Export    string `json:"export"`
	Period    string `json:"period"`
	Email     string `json:"email,omitempty"`
	Result    string `json:"result"` // started, complete, failed
	Rows      int64  `json:"rows,omitempty"`
	ZipSHA256 string `json:"zipSha256,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (c *Catalog) recordExport(ctx context.Context, actor string, ev exportEvent) {
	b, _ := json.Marshal(ev)
	slog.Info("evidence export", "id", ev.ID, "export", ev.Export, "period", ev.Period, "by", actor, "result", ev.Result, "rows", ev.Rows, "zipSha256", ev.ZipSHA256, "reason", ev.Reason)
	if c.Keys != nil {
		c.Keys.Record(context.WithoutCancel(ctx), actor, evidenceAction, string(b))
	}
}

// ExportHistoryRow is one export as the page shows it: its start and its result.
type ExportHistoryRow struct {
	At        time.Time `json:"at"`
	Admin     string    `json:"admin"`
	Export    string    `json:"export"`
	Period    string    `json:"period"`
	Email     string    `json:"email,omitempty"`
	Rows      int64     `json:"rows"`
	ZipSHA256 string    `json:"zipSha256"`
	Result    string    `json:"result"` // complete, failed, or started (still running, or the console stopped)
	Reason    string    `json:"reason,omitempty"`
}

func (c *Catalog) exportHistory(ctx context.Context) ([]ExportHistoryRow, error) {
	out := []ExportHistoryRow{}
	if c.Keys == nil {
		return out, nil
	}
	events, err := c.Keys.eventsWhere(ctx, func(action string) bool { return action == evidenceAction })
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i := len(events) - 1; i >= 0; i-- { // oldest first: the start, then the result
		e := events[i]
		var d exportEvent
		if json.Unmarshal([]byte(e.Detail), &d) != nil || d.ID == "" {
			continue
		}
		k, ok := idx[d.ID]
		if !ok {
			k = len(out)
			idx[d.ID] = k
			out = append(out, ExportHistoryRow{At: e.At, Admin: e.Actor, Export: d.Export, Period: d.Period, Email: d.Email})
		}
		r := &out[k]
		r.Result, r.Reason = d.Result, d.Reason
		if d.Result != "started" {
			r.Rows, r.ZipSHA256 = d.Rows, d.ZipSHA256
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // newest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func newExportID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// evidenceInfo answers GET /api/evidence: what can be exported, for the page.
func (c *Catalog) evidenceInfo(w http.ResponseWriter, r *http.Request, _ Session) {
	type exportInfo struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
		Reason    string `json:"reason,omitempty"`
	}
	exports := []exportInfo{{Name: "access", Available: c.Audit != nil}, {Name: "offboarding", Available: c.SCIM != nil}, {Name: "audit", Available: c.Audit != nil}}
	if c.Audit == nil {
		exports[0].Reason = "the audit store is off (evidence.enabled: false); access snapshots are kept there"
		exports[2].Reason = "the audit store is off (evidence.enabled: false)"
	}
	if c.SCIM == nil {
		exports[1].Reason = "SCIM is off, so there are no offboarding records"
	}
	out := map[string]any{"exports": exports, "running": c.Evidence.running.Load(), "maxDays": c.Evidence.MaxDays, "maxRows": c.Evidence.MaxRows,
		"retention": c.Evidence.Retention, "retentionFrom": "helm"}
	if c.Audit != nil {
		src, err := c.auditSource(r.Context())
		if err != nil {
			storeErr(w, err)
			return
		}
		out["retention"], out["retentionFrom"], out["oldestRecord"] = src.Retention, src.RetentionFrom, src.Oldest
		first, err := c.firstSnapshot(r.Context())
		if err != nil {
			storeErr(w, err)
			return
		}
		out["snapshotsSince"] = first
	}
	h, err := c.exportHistory(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read the export history: "+err.Error())
		return
	}
	out["history"] = h
	writeJSON(w, http.StatusOK, out)
}

// evidenceExport answers GET /api/evidence/{export}.zip.
func (c *Catalog) evidenceExport(w http.ResponseWriter, r *http.Request, s Session) {
	name, ok := strings.CutSuffix(r.PathValue("file"), ".zip")
	var prepare func(ctx context.Context, q url.Values) (*exportJob, error)
	switch {
	case ok && name == "audit":
		prepare = c.prepareAuditExport
	case ok && name == "offboarding":
		prepare = c.prepareOffboardingExport
	case ok && name == "access":
		prepare = c.prepareAccessExport
	default:
		writeErr(w, http.StatusBadRequest, "unknown export: use access.zip, offboarding.zip or audit.zip")
		return
	}
	if !c.Evidence.running.CompareAndSwap(false, true) {
		writeErr(w, http.StatusConflict, "another evidence export is running; try again when it has finished")
		return
	}
	defer c.Evidence.running.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), exportLimit)
	defer cancel()
	job, err := prepare(ctx, r.URL.Query())
	if err != nil {
		var ee *exportErr
		if errors.As(err, &ee) {
			writeErr(w, ee.status, ee.msg)
		} else {
			storeErr(w, err)
		}
		return
	}
	c.streamExport(ctx, w, s, job)
}

// streamExport writes the ZIP. A failure after the first byte cannot change the status (200): the console stops
// without writing the manifest or the ZIP's central directory and closes the connection, so the client has a file
// that does not open, never one that looks complete. The history and the log say why.
func (c *Catalog) streamExport(ctx context.Context, w http.ResponseWriter, s Session, job *exportJob) {
	id := newExportID()
	ev := exportEvent{ID: id, Export: job.export, Period: job.period, Email: job.parameters["email"], Result: "started"}
	c.recordExport(ctx, s.Email, ev)
	generated := c.Evidence.now()

	zipHash := sha256.New()
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="`+job.filename+`"`)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(io.MultiWriter(w, zipHash))

	var total int64
	var files []manifestFile
	fail := func(err error) {
		reason := err.Error()
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			reason = "the export ran longer than 30 minutes"
		} else if ctx.Err() != nil {
			reason = "the client closed the connection"
		}
		ev.Result, ev.Reason, ev.Rows = "failed", reason, total
		c.recordExport(ctx, s.Email, ev)
		slog.Error("evidence export failed; the ZIP has no manifest and does not open", "id", id, "export", job.export, "err", err)
		panic(http.ErrAbortHandler) // close the connection: no end of the chunked body, no central directory
	}
	for _, f := range job.files {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: generated})
		if err != nil {
			fail(err)
		}
		fh := sha256.New()
		cw := &csvWriter{w: bufio.NewWriterSize(io.MultiWriter(fw, fh), 64<<10), total: &total, max: c.Evidence.MaxRows}
		header := make([]any, len(f.header))
		for i, v := range f.header {
			header[i] = v
		}
		if err := cw.line(header); err != nil {
			fail(err)
		}
		if err := f.rows(ctx, cw); err != nil {
			fail(err)
		}
		if err := cw.w.Flush(); err != nil {
			fail(err)
		}
		files = append(files, manifestFile{Name: f.name, Rows: cw.rows, SHA256: hex.EncodeToString(fh.Sum(nil))})
	}
	if job.finish != nil {
		job.finish(job)
	}
	m := manifest{Export: job.export, AmbitVersion: c.Evidence.Version, GeneratedAt: generated, GeneratedBy: s.Email,
		Cluster: map[string]string{"namespace": c.Namespace, "consoleURL": c.PublicURL}, Parameters: job.parameters,
		Sources: job.sources, Files: files, Queries: job.queries, Notes: job.notes}
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: generated})
	if err != nil {
		fail(err)
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		fail(err)
	}
	if err := zw.Close(); err != nil {
		fail(err)
	}
	ev.Result, ev.Rows, ev.ZipSHA256 = "complete", total, hex.EncodeToString(zipHash.Sum(nil))
	c.recordExport(ctx, s.Email, ev)
}

func (c *Catalog) registerEvidence(mux *http.ServeMux, admin func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc) {
	mux.HandleFunc("GET /api/evidence", admin(c.evidenceInfo))
	mux.HandleFunc("GET /api/evidence/access/snapshots", admin(c.accessSnapshots))
	mux.HandleFunc("GET /api/evidence/{file}", admin(c.evidenceExport))
}
