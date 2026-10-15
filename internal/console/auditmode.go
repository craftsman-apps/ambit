package console

import (
	"context"
	"net/http"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/craftsman-apps/ambit/internal/auditsql"
)

// Audit-only mode (spec 4.2): the gateway writes a verdict into each access-log record when it serves the
// request, from the rules in force at that moment. The console only reads it; auditsql holds the one definition
// of the outcome, which `ambit check` uses too.
func withClassify(q string) string { return auditsql.With(q) }

// In the summary, every record of audit mode in the window counts toward newest; rules narrows to one rules version.
const auditWindow = `WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
  AND LogAttributes['ambit.accessMode'] = 'audit'
  AND ({all:UInt8} = 1 OR LogAttributes['ambit.rulesVersion'] = {rules:String})`

var wouldRefuseTotalsSQL = withClassify(`{classify}
SELECT countIf(c_outcome = 'would-refuse'), uniqExactIf(c_person, c_outcome = 'would-refuse'),
       countIf(c_outcome = 'mark-missing'), countIf(c_outcome = 'refused'), count(), max(Timestamp)
FROM otel_logs ` + auditWindow)

// wouldRefuseMaxGroups bounds the table; one more row is read to tell that there is more.
const wouldRefuseMaxGroups = 10000

var wouldRefuseGroupsSQL = withClassify(`{classify}
SELECT arrayJoin(if(empty(c_groups), ['(no group)'], c_groups)) AS grp, c_target, c_outcome, count(), uniqExact(c_person)
FROM otel_logs ` + auditWindow + `
  AND c_outcome IN ('would-refuse', 'other-model')
GROUP BY grp, c_target, c_outcome
ORDER BY count() DESC, grp, c_target, c_outcome
LIMIT 10001`)

// WouldRefuseGroup is one row of the summary table: the requests and distinct people for one group and target.
type WouldRefuseGroup struct {
	Group    string `json:"group"`
	Target   string `json:"target"` // a model name, or "<server> · <tool>"
	Outcome  string `json:"outcome"`
	Requests uint64 `json:"requests"`
	People   uint64 `json:"people"`
}

// WouldRefuse is the answer of GET /api/audit/would-refuse. A person in two groups counts in both rows; the
// totals count each person once.
type WouldRefuse struct {
	From         time.Time          `json:"from"`
	To           time.Time          `json:"to"`
	Rules        string             `json:"rules"`                  // "all" or "current"
	RulesVersion string             `json:"rulesVersion,omitempty"` // the version rules=current used
	Requests     uint64             `json:"requests"`               // would be refused
	People       uint64             `json:"people"`
	MarkMissing  uint64             `json:"markMissing"` // call records with a missing or failed mark
	Refused      uint64             `json:"refused"`     // refused for real (401, 403, 429)
	Newest       *time.Time         `json:"newest"`      // newest audit-mode record in the window; nil when none
	Groups       []WouldRefuseGroup `json:"groups"`
	Limited      bool               `json:"limited"` // more than 10,000 rows exist
}

// WouldRefuse reads the summary of audit-mode records in [from, to). rulesVersion "" counts every version.
func (a *Audit) WouldRefuse(ctx context.Context, from, to time.Time, rulesVersion string) (WouldRefuse, error) {
	out := WouldRefuse{From: from, To: to, Rules: "all", Groups: []WouldRefuseGroup{}}
	all := "1"
	if rulesVersion != "" {
		out.Rules, out.RulesVersion, all = "current", rulesVersion, "0"
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000"),
		"all": all, "rules": rulesVersion}))
	var total uint64
	var newest time.Time
	if err := a.DB.QueryRowContext(ctx, wouldRefuseTotalsSQL).Scan(&out.Requests, &out.People, &out.MarkMissing, &out.Refused, &total, &newest); err != nil {
		return out, err
	}
	if total > 0 {
		out.Newest = &newest
	}
	rows, err := a.DB.QueryContext(ctx, wouldRefuseGroupsSQL)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var g WouldRefuseGroup
		if err := rows.Scan(&g.Group, &g.Target, &g.Outcome, &g.Requests, &g.People); err != nil {
			return out, err
		}
		out.Groups = append(out.Groups, g)
	}
	if len(out.Groups) > wouldRefuseMaxGroups {
		out.Groups, out.Limited = out.Groups[:wouldRefuseMaxGroups], true
	}
	return out, rows.Err()
}

// wouldRefuse serves GET /api/audit/would-refuse?from&to&rules=current|all. rules=current needs the rules
// version the controller published.
func (c *Catalog) wouldRefuse(w http.ResponseWriter, r *http.Request, _ Session) {
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	version := ""
	switch rules := r.URL.Query().Get("rules"); rules {
	case "", "all":
	case "current":
		if version = c.publishedRulesVersion(r); version == "" {
			writeErr(w, http.StatusConflict, "the controller has not published a rules version yet, so there is no current version to filter by")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "rules must be current or all")
		return
	}
	res, err := c.Audit.WouldRefuse(r.Context(), from, to, version)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
