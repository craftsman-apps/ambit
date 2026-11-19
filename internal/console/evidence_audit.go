package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// The audit export: every gateway record of a period (calls.csv), their number per UTC day (daily-counts.csv), and
// the MCP app requests and decisions (app-requests.csv).

// evidenceCallsWhere selects the gateway's records (no ambit.kind: compression records, app events, access snapshots
// and any later kind are other records) of [from, to), of everyone or of one person. A person's records are those
// with their email, and those without an email whose JWT subject was seen with it (refusals logged with the subject
// only; see callsSQL).
const evidenceCallsWhere = `LogAttributes['ambit.kind'] = ''
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
  AND ({email:String} = '' OR LogAttributes['jwt.email'] = {email:String}
       OR (LogAttributes['jwt.email'] = '' AND has({subjects:Array(String)}, LogAttributes['jwt.sub'])))`

var evidenceCallsSQL = withClassify(`{classify}
SELECT Timestamp, LogAttributes['jwt.email'], LogAttributes['route'], LogAttributes['mcp.method.name'], LogAttributes['mcp.target'],
       LogAttributes['gen_ai.tool.name'],
       if(LogAttributes['gen_ai.request.model'] != '', LogAttributes['gen_ai.request.model'], LogAttributes['agw.ai.original_model']),
       {status}, LogAttributes['error'],
       toInt64OrZero(LogAttributes['gen_ai.usage.input_tokens']), toInt64OrZero(LogAttributes['gen_ai.usage.output_tokens']),
       LogAttributes['ambit.router'], LogAttributes['ambit.route'], LogAttributes['ambit.routeReason'],
       LogAttributes['ambit.key_id'], LogAttributes['ambit.identity'], LogAttributes['jwt.sub'],
       LogAttributes['ambit.accessMode'], c_mark, LogAttributes['ambit.rulesVersion'], c_outcome, LogAttributes['url.path'],
       ` + promptGuardSQL + `,
       if(LogAttributes['ambit.identity'] = 'invalid key', LogAttributes['client.address'], '')
FROM otel_logs
WHERE ` + evidenceCallsWhere + `
ORDER BY Timestamp`)

const evidenceCallsCountSQL = `SELECT count() FROM otel_logs
WHERE ` + evidenceCallsWhere

const evidenceAppSQL = appEventsSelect + `
ORDER BY Timestamp`

var callsHeader = []string{"time", "email", "what", "target", "outcome", "status", "error", "input_tokens", "output_tokens", "mark",
	"rules_version", "identity", "key_id", "subject", "route", "method", "server", "tool", "model", "router", "path", "access_mode",
	"client_address", "prompt_guard"}

var appHeader = []string{"time", "action", "actor", "role", "server", "request", "requested_by", "image", "url", "tools", "comment",
	"refused_reason", "pull_request"}

// The Audit page's reading of a call (web/src/routes/audit.tsx: what, target, outcome), so the export and the page
// say the same.
var (
	refusedErr = regexp.MustCompile(`Unknown tool|authorization denied for tool|changed since it was approved`)
	rateLimit  = regexp.MustCompile(`(?i)rate limit`)
)

func callWhat(c Call) string {
	switch {
	case c.Method != "" && c.Tool != "":
		return c.Method + " · " + c.Tool
	case c.Method != "":
		return c.Method
	case c.Path == "/v1/models":
		return "Model list"
	case c.Identity == "invalid key":
		return cmp.Or(c.Path, c.Route)
	case c.Model != "" || strings.HasSuffix(c.Route, "llm"):
		return "LLM request"
	}
	return c.Route
}

func callTarget(c Call) string {
	switch {
	case c.Identity == "invalid key":
		return "from " + cmp.Or(c.ClientAddress, "an unknown address")
	case c.Router != "":
		t := c.Router + " → " + c.Model
		if l := cmp.Or(c.Label, c.LabelReason); l != "" {
			t += " (" + l + ")"
		}
		return t
	}
	return cmp.Or(c.Server, c.Model)
}

func callOutcome(c Call) string {
	switch c.PromptGuard {
	case "reject":
		return "Refused by prompt guard"
	case "audit":
		return "Prompt guard would refuse"
	case "mask":
		return "Masked by prompt guard"
	}
	switch c.Outcome {
	case "refused":
		return "Refused"
	case "would-refuse":
		return "Would be refused"
	case "other-model":
		return "Would use another model (unless a later route matched)"
	case "mark-missing":
		return "Mark missing"
	case "no-mark":
		return "Served, no verdict"
	}
	status, _ := strconv.Atoi(c.Status)
	refused := c.Status == "401" || c.Status == "403" || (c.Status == "429" && rateLimit.MatchString(c.Error)) || refusedErr.MatchString(c.Error)
	switch {
	case refused:
		return "Refused by policy"
	case c.Error != "" || status >= 400:
		return "Failed"
	}
	return "OK"
}

func (c *Catalog) prepareAuditExport(ctx context.Context, q url.Values) (*exportJob, error) {
	if c.Audit == nil {
		return nil, refuse(http.StatusNotFound, "the audit store is off (evidence.enabled: false), so there is no audit log to export")
	}
	from, to, err := exportPeriod(q, c.Evidence.MaxDays)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(q.Get("email"))
	subjects := []string{}
	if email != "" {
		rows, err := c.Audit.DB.QueryContext(exportContext(ctx, clickhouse.Parameters{"email": email}), subsSQL)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			subjects = append(subjects, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	params := clickhouse.Parameters{"from": chTime(from), "to": chTime(to), "email": email, "subjects": arrayLiteral(subjects)}
	var n uint64
	if err := c.Audit.DB.QueryRowContext(exportContext(ctx, params), evidenceCallsCountSQL).Scan(&n); err != nil {
		return nil, err
	}
	if int64(n) > c.Evidence.MaxRows {
		return nil, refuse(http.StatusRequestEntityTooLarge, "the period has %d records, more than %d (evidence.exports.maxRows): split the period", n, c.Evidence.MaxRows)
	}
	src, err := c.auditSource(ctx)
	if err != nil {
		return nil, err
	}
	inside := src.insideRetention(from)
	first, last := periodName(from, to)
	sj, _ := json.Marshal(subjects)
	job := &exportJob{
		export:     "audit",
		parameters: map[string]string{"from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano), "email": email, "subjects": string(sj)},
		sources: map[string]any{"auditRetention": src.Retention, "auditRetentionFrom": src.RetentionFrom, "oldestRecordKept": src.Oldest,
			"periodStartsInsideRetention": inside, "accessMode": c.publishedMode(ctx)},
		queries:  map[string]string{"calls.csv": evidenceCallsSQL, "daily-counts.csv": "calls.csv rows per UTC day", "app-requests.csv": evidenceAppSQL},
		notes:    []string{noteUTC, noteSigned, notePersonal, noteHashes},
		filename: fmt.Sprintf("ambit-audit-%s-%s.zip", first, last),
		period:   first + " to " + last,
	}
	job.notes = append(job.notes,
		"calls.csv holds every gateway record of the period (LogAttributes['ambit.kind'] = ''), refused calls and requests with an invalid key (empty email) included. Other record kinds (compression, MCP app events, access snapshots) are not in it.",
		"rules_version 'unknown': the gateway wrote no rules version on that record (records from before audit-only mode existed, or written while no access-log policy existed).",
		"daily-counts.csv counts the rows of calls.csv per UTC day. A day with 0 records, or with far fewer than the days around it, may be a gap (the collector was down or records were dropped): the export cannot tell a gap from a quiet day.",
		"app-requests.csv holds the MCP app requests, approvals, denials, approvals of changed tools and refused attempts of the period (LogAttributes['ambit.kind'] = 'app-approval').")
	if email != "" {
		job.notes = append(job.notes, "Limited to "+email+": calls with that jwt.email, and calls without an email whose JWT subject (parameters.subjects) was seen with that email. app-requests.csv: the events this person did or that concern their request.")
	}
	if !inside {
		oldest := "none (the table is empty)"
		if src.Oldest != nil {
			oldest = src.Oldest.Format(time.RFC3339)
		}
		job.notes = append(job.notes, fmt.Sprintf("The period starts before the oldest record kept (%s; retention %s). Records before it were deleted: their absence does not mean there was no activity.", oldest, src.Retention))
	}

	perDay := map[string]int64{}
	var auditFrom, auditTo time.Time // the first and last record served in audit-only mode
	job.files = []exportFile{
		{name: "calls.csv", header: callsHeader, rows: func(ctx context.Context, w *csvWriter) error {
			return eachDay(from, to, func(dayFrom, dayTo time.Time) error {
				p := maps.Clone(params)
				p["from"], p["to"] = chTime(dayFrom), chTime(dayTo)
				return c.writeCalls(ctx, w, p, func(k Call) {
					perDay[k.Time.UTC().Format("2006-01-02")]++
					if k.AccessMode == "audit" {
						if auditFrom.IsZero() {
							auditFrom = k.Time
						}
						auditTo = k.Time
					}
				})
			})
		}},
		{name: "daily-counts.csv", header: []string{"day", "records"}, rows: func(_ context.Context, w *csvWriter) error {
			return eachDay(from, to, func(d, _ time.Time) error {
				w.rows++ // not counted against maxRows: one row per day
				day := d.Format("2006-01-02")
				return w.line([]any{day, perDay[day]})
			})
		}},
		{name: "app-requests.csv", header: appHeader, rows: func(ctx context.Context, w *csvWriter) error {
			rows, err := c.Audit.DB.QueryContext(exportContext(ctx, params), evidenceAppSQL)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				e, err := scanAppEvent(rows)
				if err != nil {
					return err
				}
				if err := w.Row(csvTime(e.Time), e.Action, e.Actor, e.Role, e.Server, e.Request, e.RequestedBy, e.Image, e.URL,
					strings.Join(e.Tools, ", "), e.Comment, e.Reason, e.PullRequest); err != nil {
					return err
				}
			}
			return rows.Err()
		}},
	}
	job.notes = append(job.notes, "calls.csv is read one UTC day at a time with the query in queries (from and to set to each day's bounds inside the period), so ClickHouse sorts one day of records at a time; the rows are the same as one query over the whole period.")
	job.finish = func(j *exportJob) {
		if !auditFrom.IsZero() {
			j.notes = append(j.notes, fmt.Sprintf("Audit-only mode (access.mode: audit) was in force for records from %s to %s: those requests were allowed, and the access rules only marked what they would have refused (columns access_mode and mark).",
				auditFrom.UTC().Format(time.RFC3339), auditTo.UTC().Format(time.RFC3339)))
		}
	}
	return job, nil
}

// eachDay calls fn for each UTC day of [from, to), with the day's bounds cut to the period.
func eachDay(from, to time.Time, fn func(dayFrom, dayTo time.Time) error) error {
	for d := from.UTC().Truncate(24 * time.Hour); d.Before(to); d = d.AddDate(0, 0, 1) {
		f, t := d, d.AddDate(0, 0, 1)
		if f.Before(from) {
			f = from
		}
		if t.After(to) {
			t = to
		}
		if err := fn(f, t); err != nil {
			return err
		}
	}
	return nil
}

// writeCalls writes the gateway records that params select, in time order, and calls seen with each.
func (c *Catalog) writeCalls(ctx context.Context, w *csvWriter, params clickhouse.Parameters, seen func(Call)) error {
	rows, err := c.Audit.DB.QueryContext(exportContext(ctx, params), evidenceCallsSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k Call
		var requested string
		if err := rows.Scan(&k.Time, &k.Email, &k.Route, &k.Method, &k.Server, &k.Tool, &k.Model, &k.Status, &k.Error, &k.InputTokens, &k.OutputTokens,
			&requested, &k.Label, &k.LabelReason, &k.KeyID, &k.Identity, &k.Subject, &k.AccessMode, &k.Mark, &k.RulesVersion, &k.Outcome, &k.Path,
			&k.PromptGuard, &k.ClientAddress); err != nil {
			return err
		}
		k.Router = routerOf(requested, k.Model)
		seen(k)
		if err := w.Row(csvTime(k.Time), k.Email, callWhat(k), callTarget(k), callOutcome(k), k.Status, k.Error, k.InputTokens, k.OutputTokens,
			k.Mark, cmp.Or(k.RulesVersion, "unknown"), k.Identity, k.KeyID, k.Subject, k.Route, k.Method, k.Server, k.Tool, k.Model, k.Router,
			k.Path, k.AccessMode, k.ClientAddress, k.PromptGuard); err != nil {
			return err
		}
	}
	return rows.Err()
}
