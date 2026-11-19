package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/craftsman-apps/ambit/internal/auditsql"
)

// The offboarding export: did people who left lose access, and when? One row per offboarding record of the period,
// with three columns from the audit store, the app grants revoked, every call accepted after the grace time (the good
// result is an empty file), and the key history of the period.

// offboardGrace covers calls already in flight when the block was written (median revocation is about 350 ms).
const offboardGrace = 5 * time.Second

// keyEventsOfOffboarding are the key history actions that remove or limit access, or give it back.
var keyEventsOfOffboarding = []string{"offboard", "offboard-delete", "revoke", "revoke-self", "reactivate", "exposed", "sa-delete-key", "sa-delete"}

// evidencePerson selects a person's gateway records: their email, or no email and a JWT subject seen with it.
const evidencePerson = `LogAttributes['ambit.kind'] = ''
  AND (LogAttributes['jwt.email'] = {email:String} OR (LogAttributes['jwt.email'] = '' AND has({subjects:Array(String)}, LogAttributes['jwt.sub'])))`

// Accepted is every status except 401 and 403: a 404, 429 or 502 still means the gateway took the credential. The
// offboarding block is a Deny rule (403), so anything else after the grace time is an exception to look at.
var offboardStatsSQL = auditsql.With(`WITH {status} AS c_status
SELECT maxIf(Timestamp, Timestamp < {at:DateTime64(9)} AND c_status NOT IN ('401', '403')),
       countIf(Timestamp >= {after:DateTime64(9)} AND c_status NOT IN ('401', '403')),
       minIf(Timestamp, Timestamp >= {at:DateTime64(9)} AND c_status IN ('401', '403'))
FROM otel_logs
WHERE ` + evidencePerson + `
  AND Timestamp < {until:DateTime64(9)}`)

var offboardCallsAfterSQL = withClassify(`{classify}
SELECT ` + evidenceCallsColumns + `
FROM otel_logs
WHERE ` + evidencePerson + `
  AND Timestamp >= {after:DateTime64(9)} AND Timestamp < {until:DateTime64(9)} AND c_status NOT IN ('401', '403')
ORDER BY Timestamp`)

// offboardStats are the three columns computed from the audit store.
type offboardStats struct {
	lastBefore, firstRefused time.Time
	acceptedAfter            uint64
	params                   clickhouse.Parameters
}

func (c *Catalog) prepareOffboardingExport(ctx context.Context, q url.Values) (*exportJob, error) {
	if c.SCIM == nil {
		return nil, refuse(http.StatusNotFound, "SCIM is off (no grants namespace), so there are no offboarding records to export")
	}
	if q.Get("email") != "" {
		return nil, refuse(http.StatusBadRequest, "email is only for the audit export")
	}
	from, to, err := exportPeriod(q, c.Evidence.MaxDays)
	if err != nil {
		return nil, err
	}
	all, err := c.SCIM.Records(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the offboarding records: %w", err)
	}
	var records []OffboardingRecord
	var since time.Time // the oldest record: SCIM was first used to offboard then
	for _, r := range all {
		if since.IsZero() || r.At.Before(since) {
			since = r.At
		}
		if !r.At.Before(from) && r.At.Before(to) {
			records = append(records, r)
		}
	}
	slices.SortStableFunc(records, func(a, b OffboardingRecord) int { return a.At.Compare(b.At) })
	until := c.Evidence.now() // calls after an offboarding are counted up to the export
	first, last := periodName(from, to)
	job := &exportJob{
		export:     "offboarding",
		parameters: map[string]string{"from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano), "callsAfterUntil": until.Format(time.RFC3339Nano), "grace": offboardGrace.String()},
		sources:    map[string]any{},
		queries:    map[string]string{},
		notes:      []string{noteUTC, noteSigned, notePersonal, noteHashes},
		filename:   fmt.Sprintf("ambit-offboarding-%s-%s.zip", first, last),
		period:     first + " to " + last,
	}
	offboarding := map[string]any{"kept": "forever (sealed Secrets, never deleted)", "since": nil}
	if !since.IsZero() {
		offboarding["since"] = since.UTC()
	}
	job.sources["offboardingRecords"] = offboarding
	job.notes = append(job.notes,
		"offboarding.csv has one row per offboarding record whose time is in the period. Records exist from the first offboarding through SCIM (sources.offboardingRecords.since); there are none before it.",
		"grants-revoked.csv has one row per app grant of each record: whether the app vendor confirmed the revocation, or its error.")

	// The audit columns, before the first byte: a store that does not answer is a 502, not a broken file.
	stats := make([]offboardStats, len(records))
	if c.Audit != nil {
		src, err := c.auditSource(ctx)
		if err != nil {
			return nil, err
		}
		for i, r := range records {
			subjects, err := c.subjectsOf(ctx, r.Email)
			if err != nil {
				return nil, err
			}
			p := clickhouse.Parameters{"email": r.Email, "subjects": arrayLiteral(subjects), "at": chTime(r.At),
				"after": chTime(r.At.Add(offboardGrace)), "until": chTime(until)}
			st := offboardStats{params: p}
			// ponytail: one query per record over the whole retention; batch them if an export ever holds hundreds.
			if err := c.Audit.DB.QueryRowContext(exportContext(ctx, p), offboardStatsSQL).Scan(&st.lastBefore, &st.acceptedAfter, &st.firstRefused); err != nil {
				return nil, err
			}
			stats[i] = st
		}
		job.sources["auditRetention"], job.sources["auditRetentionFrom"], job.sources["oldestRecordKept"] = src.Retention, src.RetentionFrom, src.Oldest
		job.queries["offboarding.csv"] = offboardStatsSQL
		job.queries["calls-after.csv"] = offboardCallsAfterSQL
		job.notes = append(job.notes,
			"accepted_calls_after counts the person's gateway calls from 5 s after the offboarding (calls already in flight) until the export (parameters.callsAfterUntil) that got any status except 401 and 403. It should be 0. A 404, 429 or 502 still means the gateway accepted the credential, so this can only over-report. calls-after.csv lists each of them; an empty file is the good result.",
			"last_accepted_call_before and first_refused_call_after come from the audit store: empty when there is no such call inside the audit retention (sources.oldestRecordKept). first_refused_call_after shows the block worked when the person or a tool tried again.",
			"A person's calls are those with their email, and those without an email whose JWT subject was seen with that email (as the Audit page's search).")
		if src.Oldest == nil || since.IsZero() || src.Oldest.After(since) {
			job.notes = append(job.notes, "The audit store does not reach back to every offboarding record: the computed columns of records older than the oldest audit record kept are incomplete.")
		}
	} else {
		job.notes = append(job.notes, "The audit store is off (evidence.enabled: false): last_accepted_call_before, accepted_calls_after and first_refused_call_after are empty and calls-after.csv is not in this file. They cannot show whether the block worked.")
	}
	keyRetention := map[string]any{"retention": eventRetention.String(), "oldestEventKept": nil}
	var events []KeyEvent
	if c.Keys != nil {
		if events, err = c.Keys.EventsBetween(ctx, from, to, keyEventsOfOffboarding...); err != nil {
			return nil, fmt.Errorf("read the key history: %w", err)
		}
		oldest, err := c.Keys.OldestEvent(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the key history: %w", err)
		}
		if !oldest.IsZero() {
			keyRetention["oldestEventKept"] = oldest.UTC()
		}
		if oldest.IsZero() || oldest.After(from) {
			job.notes = append(job.notes, "The key history is kept 400 days (sources.keyHistory): key-events.csv has no events before its oldest kept event.")
		}
	}
	job.sources["keyHistory"] = keyRetention
	job.queries["key-events.csv"] = "key history ConfigMaps (label ambitproject.dev/key-event) in the period with action " + fmt.Sprint(keyEventsOfOffboarding)
	job.notes = append(job.notes, "key-events.csv has the key history of the period that removes or limits access, or gives it back: offboard, offboard-delete, revoke, revoke-self, reactivate, and for service accounts exposed (a key's end date lowered because the admin who issued it left), sa-delete-key and sa-delete.",
		"The export is complete only for the shortest period that every source covers (sources).")

	ts := func(t time.Time) string {
		if t.Unix() <= 0 { // no such call: ClickHouse's maxIf/minIf answer the epoch
			return ""
		}
		return csvTime(t)
	}
	job.files = []exportFile{
		{name: "offboarding.csv", header: []string{"email", "at", "trigger", "sessions_ended", "gateway_blocked", "keys_deleted", "keys_exposed",
			"keys_error", "complete", "last_accepted_call_before", "accepted_calls_after", "first_refused_call_after"},
			rows: func(_ context.Context, w *csvWriter) error {
				for i, r := range records {
					last, after, refused := "", "", ""
					if c.Audit != nil {
						last, after, refused = ts(stats[i].lastBefore), strconv.FormatUint(stats[i].acceptedAfter, 10), ts(stats[i].firstRefused)
					}
					if err := w.Row(r.Email, csvTime(r.At), r.Trigger, r.SessionsEnded, strconv.FormatBool(r.GatewayBlocked), r.KeysDeleted, r.KeysExposed,
						r.KeysError, strconv.FormatBool(r.CompleteSuccess), last, after, refused); err != nil {
						return err
					}
				}
				return nil
			}},
		{name: "grants-revoked.csv", header: []string{"email", "at", "connection", "revoked_at_provider", "error"}, rows: func(_ context.Context, w *csvWriter) error {
			for _, r := range records {
				for _, g := range r.Grants {
					if err := w.Row(r.Email, csvTime(r.At), g.Connection, strconv.FormatBool(g.RevokedAtProvider), g.Error); err != nil {
						return err
					}
				}
			}
			return nil
		}},
	}
	if c.Audit != nil {
		job.files = append(job.files, exportFile{name: "calls-after.csv", header: append([]string{"offboarded_email", "offboarded_at"}, callsHeader...),
			rows: func(ctx context.Context, w *csvWriter) error {
				for i, r := range records {
					if stats[i].acceptedAfter == 0 {
						continue
					}
					if err := c.writeCalls(ctx, w, offboardCallsAfterSQL, stats[i].params, func(Call) {}, r.Email, csvTime(r.At)); err != nil {
						return err
					}
				}
				return nil
			}})
	}
	job.files = append(job.files, exportFile{name: "key-events.csv", header: []string{"at", "actor", "action", "key_id", "email", "detail"},
		rows: func(_ context.Context, w *csvWriter) error {
			for _, e := range events {
				if err := w.Row(csvTime(e.At), e.Actor, e.Action, e.KeyID, e.Email, e.Detail); err != nil {
					return err
				}
			}
			return nil
		}})
	return job, nil
}
