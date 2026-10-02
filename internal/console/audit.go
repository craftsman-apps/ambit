package console

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// Audit answers from the gateway's access logs in ClickHouse (journey A5: "every call Alice made in
// March, including denied ones"; and token usage per group). Every input reaches ClickHouse as a
// server-side query parameter, never as SQL text.
type Audit struct {
	DB *sql.DB
	// Prices returns each model's price by the name clients send (from Model objects); nil means none.
	Prices func(ctx context.Context) map[string]v1.ModelPrice
}

// ClickHouse is where the audit store is: the chart's own (plain HTTP inside the cluster) or an external
// one such as ClickHouse Cloud (HTTPS, often on port 8443).
type ClickHouse struct {
	Addr     string // host:port of the HTTP or HTTPS interface
	User     string // default "ambit"
	Database string // default "otel"
	Password string
	TLS      *tls.Config // nil: plain HTTP
}

func clickhouseOptions(c ClickHouse) *clickhouse.Options {
	if c.User == "" {
		c.User = "ambit"
	}
	if c.Database == "" {
		c.Database = "otel"
	}
	return &clickhouse.Options{Protocol: clickhouse.HTTP, Addr: []string{c.Addr}, TLS: c.TLS,
		Auth: clickhouse.Auth{Database: c.Database, Username: c.User, Password: c.Password}, DialTimeout: 5 * time.Second}
}

// OpenAudit connects over ClickHouse's HTTP interface (HTTPS when c.TLS is set).
func OpenAudit(c ClickHouse) *Audit {
	return &Audit{DB: clickhouse.OpenDB(clickhouseOptions(c))}
}

// Call is one gateway request as the auditor sees it.
type Call struct {
	Time         time.Time `json:"time"`
	Email        string    `json:"email"`
	Route        string    `json:"route"`
	Method       string    `json:"method"` // MCP method, e.g. tools/call; empty for LLM calls
	Server       string    `json:"server"` // MCP server (mcp.target)
	Tool         string    `json:"tool"`
	Model        string    `json:"model"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
}

// status reads the HTTP status under both names agentgateway has used: http.status (v1.5) and the
// OpenTelemetry name http.response.status_code (v1.6), so an upgrade does not blank the Audit page.
const status = "if(LogAttributes['http.response.status_code'] != '', LogAttributes['http.response.status_code'], LogAttributes['http.status'])"

var callsSQL = strings.Replace(`
SELECT Timestamp,
       {email:String}, LogAttributes['route'], LogAttributes['mcp.method.name'], LogAttributes['mcp.target'],
       LogAttributes['gen_ai.tool.name'], LogAttributes['gen_ai.request.model'], {status}, LogAttributes['error'],
       toInt64OrZero(LogAttributes['gen_ai.usage.input_tokens']), toInt64OrZero(LogAttributes['gen_ai.usage.output_tokens'])
FROM otel_logs
WHERE (LogAttributes['jwt.email'] = {email:String}
       -- Some refusals (e.g. a model the user may not use) are logged with the token's subject but
       -- without the email attribute; match them through the subjects seen with this email.
       OR (LogAttributes['jwt.email'] = '' AND has({subs:Array(String)}, LogAttributes['jwt.sub'])))
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}`, "{status}", status, 1)

const subsSQL = `SELECT DISTINCT LogAttributes['jwt.sub'] FROM otel_logs
WHERE LogAttributes['jwt.email'] = {email:String} AND LogAttributes['jwt.sub'] != '' LIMIT 100`

// Calls lists one user's gateway calls in [from, to), newest first.
func (a *Audit) Calls(ctx context.Context, email string, from, to time.Time, limit int) ([]Call, error) {
	// Two steps: ClickHouse 26.3 fails on IN (subquery) inside OR over map keys ("Not-ready Set").
	var subs []string
	srows, err := a.DB.QueryContext(clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{"email": email})), subsSQL)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var s string
		if err := srows.Scan(&s); err != nil {
			srows.Close()
			return nil, err
		}
		subs = append(subs, s)
	}
	srows.Close()
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"email": email, "subs": arrayLiteral(subs),
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000"),
		"limit": strconv.Itoa(limit)}))
	rows, err := a.DB.QueryContext(ctx, callsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		if err := rows.Scan(&c.Time, &c.Email, &c.Route, &c.Method, &c.Server, &c.Tool, &c.Model, &c.Status, &c.Error, &c.InputTokens, &c.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// arrayLiteral writes an Array(String) parameter value: quoted, with \ and ' escaped.
func arrayLiteral(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(it, `\`, `\\`), `'`, `\'`) + "'"
	}
	return "[" + strings.Join(q, ",") + "]"
}

// GroupUsage is LLM requests, tokens and cost per group per day; a user in two groups counts toward both.
// Cost comes from each Model's price; tokens of models without a price are counted and named.
type GroupUsage struct {
	Day            string   `json:"day"`
	Group          string   `json:"group"`
	Requests       uint64   `json:"requests"`
	InputTokens    int64    `json:"inputTokens"`
	OutputTokens   int64    `json:"outputTokens"`
	Cost           *float64 `json:"cost,omitempty"` // US dollars; nil when no priced model was used
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

type usageRow struct {
	Day, Group, Model string
	Requests          uint64
	In, Out           int64
}

const usageSQL = `
SELECT toString(toDate(Timestamp)) AS day,
       arrayJoin(if(empty(JSONExtract(LogAttributes['jwt.groups'], 'Array(String)')), ['(no group)'], JSONExtract(LogAttributes['jwt.groups'], 'Array(String)'))) AS grp,
       LogAttributes['gen_ai.request.model'] AS model,
       count(),
       sum(toInt64OrZero(LogAttributes['gen_ai.usage.input_tokens'])),
       sum(toInt64OrZero(LogAttributes['gen_ai.usage.output_tokens']))
FROM otel_logs
WHERE LogAttributes['protocol'] = 'llm' AND LogAttributes['jwt.groups'] != ''
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
GROUP BY day, grp, model
ORDER BY day DESC, grp, model`

// Usage reads LLM use per group per day; prices maps the model name clients send to its price.
func (a *Audit) Usage(ctx context.Context, from, to time.Time, prices map[string]v1.ModelPrice) ([]GroupUsage, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000")}))
	rows, err := a.DB.QueryContext(ctx, usageSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []usageRow
	for rows.Next() {
		var u usageRow
		if err := rows.Scan(&u.Day, &u.Group, &u.Model, &u.Requests, &u.In, &u.Out); err != nil {
			return nil, err
		}
		all = append(all, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return aggregateUsage(all, prices), nil
}

// aggregateUsage totals the rows per day and group (keeping their order) and prices each model's tokens.
func aggregateUsage(rows []usageRow, prices map[string]v1.ModelPrice) []GroupUsage {
	out := []GroupUsage{}
	idx := map[string]int{}
	for _, r := range rows {
		k := r.Day + "\x00" + r.Group
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, GroupUsage{Day: r.Day, Group: r.Group})
		}
		g := &out[i]
		g.Requests += r.Requests
		g.InputTokens += r.In
		g.OutputTokens += r.Out
		if r.Model == "" || r.In+r.Out == 0 {
			continue
		}
		p, priced := prices[r.Model]
		if !priced {
			if !slices.Contains(g.UnpricedModels, r.Model) {
				g.UnpricedModels = append(g.UnpricedModels, r.Model)
			}
			continue
		}
		in, _ := strconv.ParseFloat(p.InputPerMillionTokens, 64)
		outp, _ := strconv.ParseFloat(p.OutputPerMillionTokens, 64)
		c := (float64(r.In)*in + float64(r.Out)*outp) / 1e6
		if g.Cost == nil {
			g.Cost = new(float64)
		}
		*g.Cost += c
	}
	return out
}

// period reads from/to (RFC 3339 or YYYY-MM-DD) with a default of the last 30 days.
func period(r *http.Request) (time.Time, time.Time, error) {
	parse := func(v string, def time.Time) (time.Time, error) {
		if v == "" {
			return def, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02", v)
	}
	to, err := parse(r.URL.Query().Get("to"), time.Now().Add(time.Minute))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to: use 2026-03-31 or RFC 3339")
	}
	from, err := parse(r.URL.Query().Get("from"), to.AddDate(0, 0, -30))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from: use 2026-03-01 or RFC 3339")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be before to")
	}
	return from, to, nil
}

func (a *Audit) register(mux *http.ServeMux, admin func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc) {
	mux.HandleFunc("GET /api/audit", admin(func(w http.ResponseWriter, r *http.Request, s Session) {
		email := r.URL.Query().Get("email")
		if email == "" {
			writeErr(w, http.StatusBadRequest, "email is required")
			return
		}
		from, to, err := period(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		calls, err := a.Calls(r.Context(), email, from, to, 1000)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"email": email, "from": from, "to": to, "calls": calls, "limited": len(calls) == 1000})
	}))
	mux.HandleFunc("GET /api/usage", admin(func(w http.ResponseWriter, r *http.Request, _ Session) {
		from, to, err := period(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var prices map[string]v1.ModelPrice
		if a.Prices != nil {
			prices = a.Prices(r.Context())
		}
		u, err := a.Usage(r.Context(), from, to, prices)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "usage": u})
	}))
}

// storeErr reports a failed ClickHouse query with its cause (admins only see these pages), so an
// empty page is never mistaken for "no calls".
func storeErr(w http.ResponseWriter, err error) {
	slog.Error("audit store", "err", err)
	writeErr(w, http.StatusBadGateway, "the audit store (ClickHouse) did not answer, so no results are shown: "+err.Error())
}
