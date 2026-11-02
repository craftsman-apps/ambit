package console

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// Two small summary tables, each filled by a materialized view when the collector inserts a block of otel_logs.
// The Savings page reads these (tens of thousands of rows for 30 days) instead of scanning the map column of every
// record. The names carry v1: a later change creates v2 beside them instead of altering a live view.
//
//   - ambit_llm_daily_v1: LLM requests and tokens per UTC day, group, router and model.
//   - ambit_compress_daily_v1: compressed tool result texts and bytes per UTC day, group, server and type.
//
// The group "" is the all-groups row: it counts each record once. A person in two groups counts in both group rows,
// so every total must read only grp = '' (docs/savings.md).
//
// The views see only blocks inserted after they exist. There is no backfill: a copy from otel_logs cannot be exact
// (the table has no insert time).

const (
	llmTable      = "ambit_llm_daily_v1"
	compressTable = "ambit_compress_daily_v1"
	llmView       = llmTable + "_mv"
	compressView  = compressTable + "_mv"
)

// summaryDDL are the statements that create the summaries; {days} is the retention in whole days. It is a list
// of plain SQL so the kind test and the query-cost check run the very same text.
func summaryDDL(days int) []string {
	d := fmt.Sprint(days)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + llmTable + ` (
  day Date, grp LowCardinality(String), router LowCardinality(String), model LowCardinality(String),
  requests UInt64, input_tokens Int64, output_tokens Int64
) ENGINE = SummingMergeTree ORDER BY (day, grp, router, model)
  TTL day + INTERVAL {days} DAY`,
		// router is '' for a direct request (the router name equals the model name or is empty). The filter is the
		// one of the Usage query: a record without jwt.groups is in neither.
		`CREATE MATERIALIZED VIEW IF NOT EXISTS ` + llmView + ` TO ` + llmTable + ` AS
SELECT toDate(Timestamp, 'UTC') AS day,
       grp,
       if(LogAttributes['ambit.router'] = LogAttributes['gen_ai.request.model'], '', LogAttributes['ambit.router']) AS router,
       LogAttributes['gen_ai.request.model'] AS model,
       count() AS requests,
       sum(toInt64OrZero(LogAttributes['gen_ai.usage.input_tokens'])) AS input_tokens,
       sum(toInt64OrZero(LogAttributes['gen_ai.usage.output_tokens'])) AS output_tokens
FROM otel_logs
ARRAY JOIN arrayConcat([''], if(empty(JSONExtract(LogAttributes['jwt.groups'], 'Array(String)')), ['(no group)'], JSONExtract(LogAttributes['jwt.groups'], 'Array(String)'))) AS grp
WHERE LogAttributes['protocol'] = 'llm' AND LogAttributes['jwt.groups'] != ''
GROUP BY day, grp, router, model`,
		`CREATE TABLE IF NOT EXISTS ` + compressTable + ` (
  day Date, grp LowCardinality(String), server LowCardinality(String), type LowCardinality(String),
  results UInt64, bytes_in UInt64, bytes_out UInt64, omitted UInt64
) ENGINE = SummingMergeTree ORDER BY (day, grp, server, type)
  TTL day + INTERVAL {days} DAY`,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS ` + compressView + ` TO ` + compressTable + ` AS
SELECT toDate(Timestamp, 'UTC') AS day,
       grp,
       LogAttributes['mcp.target'] AS server,
       LogAttributes['ambit.compress.type'] AS type,
       count() AS results,
       sum(toUInt64OrZero(LogAttributes['ambit.compress.bytes_in'])) AS bytes_in,
       sum(toUInt64OrZero(LogAttributes['ambit.compress.bytes_out'])) AS bytes_out,
       sum(toUInt64OrZero(LogAttributes['ambit.compress.omitted'])) AS omitted
FROM otel_logs
ARRAY JOIN arrayConcat([''], if(empty(JSONExtract(LogAttributes['jwt.groups'], 'Array(String)')), ['(no group)'], JSONExtract(LogAttributes['jwt.groups'], 'Array(String)'))) AS grp
WHERE LogAttributes['ambit.kind'] = 'compression' AND LogAttributes['jwt.groups'] != ''
GROUP BY day, grp, server, type`,
	}
	for i := range stmts {
		stmts[i] = strings.ReplaceAll(stmts[i], "{days}", d)
	}
	return stmts
}

// summaryTTL returns the TTL statements for an existing summary, so a changed retention applies at the next start.
// materialize_ttl_after_modify = 0: only new and merged parts get the new TTL; no rewrite of the old ones at each start.
func summaryTTL(days int) []string {
	return []string{
		fmt.Sprintf("ALTER TABLE %s MODIFY TTL day + INTERVAL %d DAY SETTINGS materialize_ttl_after_modify = 0", llmTable, days),
		fmt.Sprintf("ALTER TABLE %s MODIFY TTL day + INTERVAL %d DAY SETTINGS materialize_ttl_after_modify = 0", compressTable, days),
	}
}

// MinSummaryRetention is the shortest retention the console accepts.
const MinSummaryRetention = 24 * time.Hour

// SummaryDays converts the Helm duration of evidence.summaryRetention (a Go duration such as 26280h) into whole days,
// rounded up. A value under 24 hours is refused: the table would expire its own rows the day they are written.
func SummaryDays(s string) (int, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("evidence.summaryRetention %q: %w", s, err)
	}
	if d < MinSummaryRetention {
		return 0, fmt.Errorf("evidence.summaryRetention %q is below 24h", s)
	}
	return int((d + 24*time.Hour - 1) / (24 * time.Hour)), nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EnsureSummaries creates the summary tables and views when they do not exist and sets the retention. It is safe to
// run again and on several replicas at once. It fails until the collector has created otel_logs.
func EnsureSummaries(ctx context.Context, db execer, days int) error {
	for _, q := range append(summaryDDL(days), summaryTTL(days)...) {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(q, "\n", 2)[0], err)
		}
	}
	return nil
}

// Summaries tracks whether the summary tables exist, for the Savings page.
type summaryState struct {
	err error // last failure; nil once EnsureSummaries succeeded
	ok  bool
}

// RunSummaries calls EnsureSummaries until it succeeds, retrying every retry, and records the result. Usage and Audit
// do not depend on it.
func (a *Audit) RunSummaries(ctx context.Context, days int, retry time.Duration) {
	a.runSummaries(ctx, a.DB, days, retry)
}

func (a *Audit) runSummaries(ctx context.Context, db execer, days int, retry time.Duration) {
	for ctx.Err() == nil {
		err := EnsureSummaries(ctx, db, days)
		a.sumMu.Lock()
		a.sum = summaryState{err: err, ok: err == nil}
		a.sumMu.Unlock()
		if err == nil {
			slog.Info("savings summaries ready", "retentionDays", days)
			return
		}
		slog.Warn("savings summaries not created yet", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
	}
}

func (a *Audit) summaryStatus() (bool, error) {
	a.sumMu.Lock()
	defer a.sumMu.Unlock()
	return a.sum.ok, a.sum.err
}

const (
	llmDailySQL = `SELECT toString(day), grp, router, model, sum(requests), sum(input_tokens), sum(output_tokens)
FROM ` + llmTable + ` WHERE day >= {from:Date} AND day <= {to:Date}
GROUP BY day, grp, router, model`
	compressDailySQL = `SELECT toString(day), grp, server, type, sum(results), sum(bytes_in), sum(bytes_out), sum(omitted)
FROM ` + compressTable + ` WHERE day >= {from:Date} AND day <= {to:Date}
GROUP BY day, grp, server, type`
	// The views were created when the summaries began to count.
	sinceSQL = `SELECT toString(max(metadata_modification_time)) FROM system.tables
WHERE database = currentDatabase() AND name IN ('` + llmView + `', '` + compressView + `')`
)

// SummaryRows reads both summaries for the UTC days from..to (inclusive). It always sums, because a SummingMergeTree
// merges later and may hold several rows for one key.
func (a *Audit) SummaryRows(ctx context.Context, from, to time.Time) ([]SavingsLLMRow, []SavingsCompressRow, string, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"from": from.UTC().Format("2006-01-02"), "to": to.UTC().Format("2006-01-02")}))
	var llm []SavingsLLMRow
	rows, err := a.DB.QueryContext(ctx, llmDailySQL)
	if err != nil {
		return nil, nil, "", err
	}
	for rows.Next() {
		var r SavingsLLMRow
		if err := rows.Scan(&r.Day, &r.Group, &r.Router, &r.Model, &r.Requests, &r.In, &r.Out); err != nil {
			rows.Close()
			return nil, nil, "", err
		}
		llm = append(llm, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	rows.Close()
	var comp []SavingsCompressRow
	rows, err = a.DB.QueryContext(ctx, compressDailySQL)
	if err != nil {
		return nil, nil, "", err
	}
	for rows.Next() {
		var r SavingsCompressRow
		if err := rows.Scan(&r.Day, &r.Group, &r.Server, &r.Type, &r.Results, &r.BytesIn, &r.BytesOut, &r.Omitted); err != nil {
			rows.Close()
			return nil, nil, "", err
		}
		comp = append(comp, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	rows.Close()
	var since string
	_ = a.DB.QueryRowContext(ctx, sinceSQL).Scan(&since) // a failure leaves "": the page then omits the date
	return llm, comp, since, nil
}

// routerInfos lists the ModelRouters as they are now, with the Models they can pick. models maps a Model object name
// to the model name clients send.
func RouterInfos(routers []v1.ModelRouter, models []v1.Model) []RouterInfo {
	wire := map[string]string{}
	for _, m := range models {
		wire[m.Name] = m.Spec.Model
	}
	var out []RouterInfo
	for _, r := range routers {
		info := RouterInfo{Name: r.WireName()}
		seen := map[string]bool{}
		for _, obj := range append([]string{r.Spec.Default}, routeModels(r)...) {
			if seen[obj] {
				continue
			}
			seen[obj] = true
			info.Candidates = append(info.Candidates, RouterCandidate{Object: obj, Model: wire[obj]})
		}
		out = append(out, info)
	}
	return out
}

func routeModels(r v1.ModelRouter) []string {
	var out []string
	for _, rt := range r.Spec.Routes {
		out = append(out, rt.Model)
	}
	return out
}
