package console

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/auditsql"
)

// Audit answers from the gateway's access logs in ClickHouse (journey A5: "every call User01 made in
// March, including denied ones"; and token usage per group). Every input reaches ClickHouse as a
// server-side query parameter, never as SQL text.
type Audit struct {
	DB *sql.DB
	// Prices returns each model's price by the name clients send (from Model objects); nil means none.
	Prices func(ctx context.Context) map[string]v1.ModelPrice
	// Routers returns the ModelRouters as they are now (for the routing baseline of the Savings page); nil means none.
	Routers func(ctx context.Context) []RouterInfo
	// ReferenceInputPrice is the admin's price per million input tokens for compression dollars of a group without
	// priced LLM traffic; nil means none (console.savings.referenceInputPricePerMillion).
	ReferenceInputPrice *float64
	// AgentOwners returns each agent's owner as the records say now (agent name to owner); nil means none are known.
	// Audit adds it to a call as agentOwnerNow; the call itself keeps the owner at issue.
	AgentOwners func(ctx context.Context) (map[string]string, error)

	sumMu sync.Mutex
	sum   summaryState
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
	// Router is the model name the client asked for when it differs from Model (a ModelRouter such as "auto").
	Router string `json:"router"`
	// Label and LabelReason are a classifier's label and why it was or was not set (empty for type Rules).
	Label       string `json:"label"`
	LabelReason string `json:"labelReason"`
	// KeyID is the personal API key id of a call made with a key; Identity says how the caller was identified
	// (key or JWT); Subject is the token's jwt.sub (empty for keys).
	KeyID    string `json:"keyId"`
	Identity string `json:"identity"`
	Subject  string `json:"subject"`
	// AccessMode is ambit.accessMode, Mark the verdict written when the request was served (empty when absent),
	// RulesVersion the rules it was judged by, and Outcome the class of auditmode.go: refused, would-refuse,
	// allow, other-model, mark-missing, no-mark or n/a.
	AccessMode   string `json:"accessMode"`
	Mark         string `json:"mark"`
	RulesVersion string `json:"rulesVersion"`
	Outcome      string `json:"outcome"`
	// Path is the request path (url.path), for display: /v1/models is a model-list call. ClientAddress is set only
	// for requests with an invalid key (InvalidKeyCalls).
	Path          string `json:"path"`
	ClientAddress string `json:"clientAddress,omitempty"`
	// PromptGuard is what the prompt guard (llm.promptGuard) did: reject, audit (it would have refused; audit mode),
	// mask, allow, or empty (no guard ran). From the ambit.promptGuard field: never the matched text.
	PromptGuard string `json:"promptGuard"`
	// Compression is what the guard did to the tool result (#203); nil when it did not compress it, or when no record
	// of it arrived. CallID (ambit.call_id) links the guard's records to this call; it is not in the answer.
	Compression *Compression `json:"compression,omitempty"`
	CallID      string       `json:"-"`
	// The agent of the call (docs/agents.md, experimental), read from a verified key or token; empty for a person's own
	// key or token. PlatformDeclared is what the agent's record says it is, never a proof. AgentOwnerAtIssue is the
	// owner when the credential was issued; AgentOwnerNow is added when the console reads (the agent's record today;
	// empty when the record is gone). GrantID joins an OAuth grant; TokenIssuer is the validated issuer of a token.
	// Ingress is private or public: the Gateway that took the call.
	Agent             string `json:"agent,omitempty"`
	AgentMode         string `json:"agentMode,omitempty"`
	AgentOwnerAtIssue string `json:"agentOwnerAtIssue,omitempty"`
	AgentOwnerNow     string `json:"agentOwnerNow,omitempty"`
	PlatformDeclared  string `json:"platformDeclared,omitempty"`
	GrantID           string `json:"grantId,omitempty"`
	TokenIssuer       string `json:"tokenIssuer,omitempty"`
	Ingress           string `json:"ingress,omitempty"`
}

// agentColumns are the access-log attributes of an agent call, in the order of Call's agent fields; they come last in
// every query that reads calls.
const agentColumns = `LogAttributes['ambit.agent'], LogAttributes['ambit.agent_mode'], LogAttributes['ambit.agent_owner_at_issue'],
       LogAttributes['ambit.platform_declared'], LogAttributes['ambit.grant_id'], LogAttributes['ambit.token_issuer'], LogAttributes['ambit.ingress']`

// Compression is the guard's records of one call's result: one part per compressed text, and the sums.
type Compression struct {
	Mode     string            `json:"mode"` // lossless or shorten
	Parts    []CompressionPart `json:"parts"`
	BytesIn  int64             `json:"bytesIn"`
	BytesOut int64             `json:"bytesOut"`
	Match    string            `json:"match"` // call-id: joined by the call ID
}

// CompressionPart is one compressed text of the result (its index in content[]).
type CompressionPart struct {
	Part     int    `json:"part"`
	Type     string `json:"type"`
	BytesIn  int64  `json:"bytesIn"`
	BytesOut int64  `json:"bytesOut"`
	Omitted  int64  `json:"omitted"`
}

// promptGuardSQL reads ambit.promptGuard (["request:regex:reject", ...], one "phase:guard:action" per guard) as the
// strongest action of any guard. One column: the alias c_pg is defined inside the expression.
const promptGuardSQL = `multiIf(has((arrayMap(x -> splitByChar(':', x)[-1], JSONExtract(LogAttributes['ambit.promptGuard'], 'Array(String)')) AS c_pg), 'reject'), 'reject',
               has(c_pg, 'audit'), 'audit', has(c_pg, 'mask'), 'mask', has(c_pg, 'allow'), 'allow', '')`

// routerOf is the router a call went through: the requested name when it differs from the answering model.
func routerOf(requested, model string) string {
	if requested == "" || requested == model {
		return ""
	}
	return requested
}

const status = auditsql.Status

var callsSQL = withClassify(`{classify}
SELECT Timestamp,
       -- the person asked for; for an agent or platform search the principal the gateway logged
       if({email:String} != '', {email:String}, LogAttributes['jwt.email']), LogAttributes['route'], LogAttributes['mcp.method.name'], LogAttributes['mcp.target'],
       LogAttributes['gen_ai.tool.name'],
       -- a model the gateway refused itself has no gen_ai.request.model; agentgateway v1.7.0 logs the name asked for
       if(LogAttributes['gen_ai.request.model'] != '', LogAttributes['gen_ai.request.model'], LogAttributes['agw.ai.original_model']),
       {status}, LogAttributes['error'],
       toInt64OrZero(LogAttributes['gen_ai.usage.input_tokens']), toInt64OrZero(LogAttributes['gen_ai.usage.output_tokens']),
       LogAttributes['ambit.router'], LogAttributes['ambit.route'], LogAttributes['ambit.routeReason'],
       LogAttributes['ambit.key_id'], LogAttributes['ambit.identity'], LogAttributes['jwt.sub'],
       LogAttributes['ambit.accessMode'], c_mark, LogAttributes['ambit.rulesVersion'], c_outcome, LogAttributes['url.path'],
       ` + promptGuardSQL + `, LogAttributes['ambit.call_id'], ` + agentColumns + `
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = '' -- gateway records only: guard and console records (ambit.kind set) are not calls
  AND ({email:String} = '' OR LogAttributes['jwt.email'] = {email:String}
       -- Some refusals (e.g. a model the user may not use) are logged with the token's subject but
       -- without the email attribute; match them through the subjects seen with this email.
       OR (LogAttributes['jwt.email'] = '' AND has({subs:Array(String)}, LogAttributes['jwt.sub'])))
  AND ({agent:String} = '' OR LogAttributes['ambit.agent'] = {agent:String})
  AND ({platform:String} = '' OR LogAttributes['ambit.platform_declared'] = {platform:String})
  AND ({ingress:String} = '' OR LogAttributes['ambit.ingress'] = {ingress:String})
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}`)

// invalidKeySQL lists requests made with a key that is wrong, revoked or expired: they have no person. Only these
// fields are read; the access log never holds a key or the Authorization header.
var invalidKeySQL = `SELECT Timestamp, LogAttributes['route'], LogAttributes['url.path'], ` + status + `,
       LogAttributes['error'], LogAttributes['client.address']
FROM otel_logs
WHERE LogAttributes['ambit.identity'] = 'invalid key' AND LogAttributes['ambit.kind'] = ''
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}`

const subsSQL = `SELECT DISTINCT LogAttributes['jwt.sub'] FROM otel_logs
WHERE LogAttributes['ambit.kind'] = '' AND LogAttributes['jwt.email'] = {email:String} AND LogAttributes['jwt.sub'] != '' LIMIT 100`

// CallFilter narrows Calls to one agent, one declared platform, or one Gateway (private or public). Empty: no filter.
type CallFilter struct{ Agent, Platform, Ingress string }

// Calls lists the gateway calls of one user, or of one agent or declared platform (email empty), in [from, to), newest first.
func (a *Audit) Calls(ctx context.Context, email string, from, to time.Time, limit int, f CallFilter) ([]Call, error) {
	// Two steps: ClickHouse 26.3 fails on IN (subquery) inside OR over map keys ("Not-ready Set").
	var subs []string
	if email != "" {
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
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"email": email, "subs": arrayLiteral(subs), "agent": f.Agent, "platform": f.Platform, "ingress": f.Ingress,
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
		var requested string
		if err := rows.Scan(&c.Time, &c.Email, &c.Route, &c.Method, &c.Server, &c.Tool, &c.Model, &c.Status, &c.Error, &c.InputTokens, &c.OutputTokens,
			&requested, &c.Label, &c.LabelReason, &c.KeyID, &c.Identity, &c.Subject, &c.AccessMode, &c.Mark, &c.RulesVersion, &c.Outcome, &c.Path, &c.PromptGuard, &c.CallID,
			&c.Agent, &c.AgentMode, &c.AgentOwnerAtIssue, &c.PlatformDeclared, &c.GrantID, &c.TokenIssuer, &c.Ingress); err != nil {
			return nil, err
		}
		c.Router = routerOf(requested, c.Model)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, a.addCompression(ctx, email, from, out)
}

// compressionSQL reads the guard's records of the given calls by the same person: one row per call and part, so a
// record delivered twice counts once. A record is written up to a few seconds after its call, so it is read from the
// period's start without an end; the call IDs bound the scan. With protection.compressionAudit.caller false the
// records have no email and are not shown.
// Every field comes from the same (the first) copy of a record.
const compressionSQL = `SELECT LogAttributes['ambit.call_id'] AS id, toInt64OrZero(LogAttributes['ambit.compress.part']) AS part,
       argMin(LogAttributes['ambit.compress.mode'], Timestamp), argMin(LogAttributes['ambit.compress.type'], Timestamp),
       argMin(toInt64OrZero(LogAttributes['ambit.compress.bytes_in']), Timestamp), argMin(toInt64OrZero(LogAttributes['ambit.compress.bytes_out']), Timestamp),
       argMin(toInt64OrZero(LogAttributes['ambit.compress.omitted']), Timestamp)
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = 'compression' AND ({email:String} = '' OR LogAttributes['jwt.email'] = {email:String})
  AND Timestamp >= {from:DateTime64(9)} AND has({ids:Array(String)}, LogAttributes['ambit.call_id'])
GROUP BY id, part
ORDER BY id, part`

// addCompression sets Compression on the calls that have records. Calls from before 0.5.0 have no call ID.
func (a *Audit) addCompression(ctx context.Context, email string, from time.Time, calls []Call) error {
	byID := map[string]*Call{}
	var ids []string
	for i := range calls {
		if id := calls[i].CallID; id != "" && byID[id] == nil {
			byID[id] = &calls[i]
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := a.DB.QueryContext(clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{"email": email, "ids": arrayLiteral(ids),
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000")})), compressionSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, mode string
		var p CompressionPart
		if err := rows.Scan(&id, &p.Part, &mode, &p.Type, &p.BytesIn, &p.BytesOut, &p.Omitted); err != nil {
			return err
		}
		c := byID[id]
		if c == nil {
			continue
		}
		if c.Compression == nil {
			c.Compression = &Compression{Mode: mode, Parts: []CompressionPart{}, Match: "call-id"}
		}
		c.Compression.Parts = append(c.Compression.Parts, p)
		c.Compression.BytesIn += p.BytesIn
		c.Compression.BytesOut += p.BytesOut
	}
	return rows.Err()
}

// InvalidKeyCalls lists the requests in [from, to) made with an invalid key, newest first. Identity is "invalid
// key"; Email and the other person fields stay empty.
func (a *Audit) InvalidKeyCalls(ctx context.Context, from, to time.Time, limit int) ([]Call, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000"),
		"limit": strconv.Itoa(limit)}))
	rows, err := a.DB.QueryContext(ctx, invalidKeySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		c := Call{Identity: "invalid key"}
		if err := rows.Scan(&c.Time, &c.Route, &c.Path, &c.Status, &c.Error, &c.ClientAddress); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const keysLastUsedSQL = `SELECT LogAttributes['ambit.key_id'], max(Timestamp) FROM otel_logs
WHERE Timestamp >= {from:DateTime64(9)} AND LogAttributes['ambit.kind'] = ''
  AND LogAttributes['ambit.key_id'] != '' AND has({ids:Array(String)}, LogAttributes['ambit.key_id'])
GROUP BY LogAttributes['ambit.key_id']`

// KeysLastUsed returns the time of each key's latest gateway call since from (the oldest key's creation, so the
// scan is bounded); keys never used are absent.
func (a *Audit) KeysLastUsed(ctx context.Context, ids []string, from time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := a.DB.QueryContext(clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{"ids": arrayLiteral(ids),
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000")})), keysLastUsedSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
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
SELECT toString(toDate(Timestamp, 'UTC')) AS day,
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

// usageSummarySQL reads the LLM daily summary (group rows only: grp ” is the all-groups row). It has the columns
// and the order of usageSQL.
const usageSummarySQL = `
SELECT toString(day), grp, model, sum(requests), sum(input_tokens), sum(output_tokens)
FROM ` + llmTable + `
WHERE grp != '' AND day >= {dfrom:Date} AND day <= {dto:Date}
GROUP BY day, grp, model
ORDER BY day DESC, grp, model`

func (a *Audit) usageRows(ctx context.Context, q string, params clickhouse.Parameters) ([]usageRow, error) {
	rows, err := a.DB.QueryContext(clickhouse.Context(ctx, clickhouse.WithParameters(params)), q)
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
	return all, rows.Err()
}

// Usage reads LLM use per group per day; prices maps the model name clients send to its price. Days the LLM summary
// holds completely come from it (tens of rows per day); older days, and everything while the summary does not exist,
// come from the scan of otel_logs. The summary is per UTC day, so the first day of the period counts whole.
func (a *Audit) Usage(ctx context.Context, from, to time.Time, prices map[string]v1.ModelPrice) ([]GroupUsage, error) {
	cov, err := a.summaryCover(ctx, llmTable, llmView)
	if err != nil {
		return a.usageScan(ctx, from, to, prices) // the summary is not there (yet)
	}
	sp := usageSplit(cov, from, to)
	var all []usageRow
	if sp.summary {
		all, err = a.usageRows(ctx, usageSummarySQL, clickhouse.Parameters{"dfrom": sp.sumFrom.Format("2006-01-02"), "dto": sp.sumTo.Format("2006-01-02")})
		if err != nil {
			return nil, err
		}
	}
	if sp.scan {
		old, err := a.usageRows(ctx, usageSQL, clickhouse.Parameters{"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": sp.scanTo.UTC().Format("2006-01-02 15:04:05.000000000")})
		if err != nil {
			return nil, err
		}
		all = append(all, old...) // older days after newer ones: the order of usageSQL (day DESC)
	}
	return aggregateUsage(all, prices), nil
}

// usagePlan says which part of the period the summary answers (whole UTC days sumFrom..sumTo) and which part the scan
// answers (from `from` until scanTo).
type usagePlan struct {
	summary, scan  bool
	sumFrom, sumTo time.Time
	scanTo         time.Time
}

func usageSplit(cov cover, from, to time.Time) usagePlan {
	day := func(t time.Time) time.Time {
		t = t.UTC()
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	fromDay, toDay := day(from), day(to.Add(-time.Nanosecond))
	p := usagePlan{sumFrom: fromDay, sumTo: toDay, scanTo: to}
	if cov.Full.After(p.sumFrom) {
		p.sumFrom = cov.Full
	}
	p.summary = !p.sumFrom.After(toDay)
	if cov.Full.After(fromDay) { // days before the summary is complete
		p.scan = true
		if cov.Full.Before(to) {
			p.scanTo = cov.Full
		}
	}
	return p
}

// usageScan is the whole answer from otel_logs: the fallback, and the reference the tests compare the summary to.
func (a *Audit) usageScan(ctx context.Context, from, to time.Time, prices map[string]v1.ModelPrice) ([]GroupUsage, error) {
	all, err := a.usageRows(ctx, usageSQL, clickhouse.Parameters{"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000")})
	if err != nil {
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
// agentOwnerNow reads the owners once and returns a lookup; nil when owners are not available. A failed read is
// logged and leaves the owner empty: the audit rows are still right.
func (a *Audit) agentOwnerNow(ctx context.Context) func(string) string {
	if a.AgentOwners == nil {
		return nil
	}
	owners, err := a.AgentOwners(ctx)
	if err != nil {
		slog.Warn("agent owners unknown", "err", err)
		return nil
	}
	return func(agent string) string { return owners[agent] }
}

// filterValue: an agent name or a declared platform in a query (empty allowed).
var filterValue = regexp.MustCompile(`^[a-z0-9._-]{0,64}$`)

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
		q := r.URL.Query()
		email, invalid, approvals := q.Get("email"), q.Get("invalidKey"), q.Get("approvals")
		f := CallFilter{Agent: q.Get("agent"), Platform: q.Get("platform_declared"), Ingress: q.Get("ingress")}
		if !filterValue.MatchString(f.Agent) || !filterValue.MatchString(f.Platform) || (f.Ingress != "" && f.Ingress != "private" && f.Ingress != "public") {
			writeErr(w, http.StatusBadRequest, "agent and platform_declared are names (letters, digits, . _ -); ingress is private or public")
			return
		}
		selective := email != "" || f.Agent != "" || f.Platform != ""
		if approvals != "" {
			if approvals != "true" || invalid != "" || f.Agent != "" || f.Platform != "" || f.Ingress != "" {
				writeErr(w, http.StatusBadRequest, "give approvals=true, with an optional email")
				return
			}
		} else if invalid != "" {
			if invalid != "true" || selective || f.Ingress != "" {
				writeErr(w, http.StatusBadRequest, "give invalidKey=true alone, or email, agent or platform_declared")
				return
			}
		} else if !selective {
			writeErr(w, http.StatusBadRequest, "give email, agent, platform_declared or invalidKey=true (ingress only narrows one of them)")
			return
		}
		from, to, err := period(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if approvals != "" { // MCP app requests, decisions and refused attempts; email narrows to one person's
			events, err := a.AppEvents(r.Context(), email, from, to, 1000)
			if err != nil {
				storeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"approvals": true, "email": email, "from": from, "to": to, "events": events, "limited": len(events) == 1000})
			return
		}
		if invalid != "" {
			calls, err := a.InvalidKeyCalls(r.Context(), from, to, 1000)
			if err != nil {
				storeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"invalidKey": true, "from": from, "to": to, "calls": calls, "limited": len(calls) == 1000})
			return
		}
		calls, err := a.Calls(r.Context(), email, from, to, 1000, f)
		if err != nil {
			storeErr(w, err)
			return
		}
		if owner := a.agentOwnerNow(r.Context()); owner != nil {
			for i := range calls {
				calls[i].AgentOwnerNow = owner(calls[i].Agent)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"email": email, "agent": f.Agent, "platform_declared": f.Platform, "ingress": f.Ingress,
			"from": from, "to": to, "calls": calls, "limited": len(calls) == 1000})
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
	mux.HandleFunc("GET /api/savings", admin(a.savings))
}

// storeErr reports a failed ClickHouse query with its cause (admins only see these pages), so an
// empty page is never mistaken for "no calls".
func storeErr(w http.ResponseWriter, err error) {
	slog.Error("audit store", "err", err)
	writeErr(w, http.StatusBadGateway, "the audit store (ClickHouse) did not answer, so no results are shown: "+err.Error())
}
