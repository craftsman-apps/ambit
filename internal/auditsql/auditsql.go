// Package auditsql is the one definition of how an audit-mode access-log record is classified. The console (the
// Audit page) and the CLI (`ambit check --production`) both run it in ClickHouse, so they cannot disagree.
package auditsql

import "strings"

// Status reads the HTTP status under both names agentgateway has used: http.status (v1.5) and the
// OpenTelemetry name http.response.status_code (v1.6), so an upgrade does not blank the Audit page.
const Status = "if(LogAttributes['http.response.status_code'] != '', LogAttributes['http.response.status_code'], LogAttributes['http.status'])"

// The gateway writes a verdict (ambit.wouldRefuse, ambit.wouldRefuseTool) into each record when it serves a request,
// from the rules in force at that moment. Four outcomes, never mixed:
//
//	refused       the request got 401, 403 or 429 (sign-in, offboarding, unpriced model, budget): a real refusal
//	would-refuse  served (2xx) and the mark is refuse:model, refuse:router or refuse:tool
//	allow, other-model  served and the mark says so
//	mark-missing  a call (an LLM request with a model, or an MCP tools/call), served, and its field is absent or
//	              holds another value: the rule expression failed, or the fields were not active yet. A defect, never
//	              counted as "would be refused"
//	n/a           everything else: tools/list, initialize, a mark of n/a, an enforce-mode or older record
//
// classifySQL is a WITH clause; it defines c_outcome, c_mark, c_target, c_person and c_groups for otel_logs.
const classifySQL = `WITH {status} AS c_status,
     LogAttributes['mcp.method.name'] AS c_method,
     LogAttributes['gen_ai.request.model'] AS c_model,
     if(c_method != '',
        [LogAttributes['ambit.wouldRefuseTool']],
        arrayMap(k -> LogAttributes[k], arrayFilter(k -> startsWith(k, 'ambit.wouldRefuse') AND k != 'ambit.wouldRefuseTool', mapKeys(LogAttributes)))) AS c_marks,
     arrayFirst(x -> x NOT IN ('', 'n/a'), c_marks) AS c_first,
     multiIf(c_first != '', c_first, has(c_marks, 'n/a'), 'n/a', '') AS c_mark,
     if(c_method != '', c_method = 'tools/call', c_model != '') AS c_is_call,
     multiIf(
       LogAttributes['ambit.accessMode'] != 'audit', 'n/a',
       c_status IN ('401', '403', '429'), 'refused',
       NOT startsWith(c_status, '2'), 'n/a',
       c_mark IN ('refuse:model', 'refuse:router', 'refuse:tool'), 'would-refuse',
       c_mark = 'allow', 'allow',
       c_mark = 'other-model', 'other-model',
       c_mark = 'n/a' OR NOT c_is_call, 'n/a',
       'mark-missing') AS c_outcome,
     if(c_method != '', concat(LogAttributes['mcp.target'], ' · ', LogAttributes['gen_ai.tool.name']), c_model) AS c_target,
     if(LogAttributes['jwt.email'] != '', LogAttributes['jwt.email'], LogAttributes['jwt.sub']) AS c_person,
     JSONExtract(LogAttributes['jwt.groups'], 'Array(String)') AS c_groups`

// With puts the classification into q: {classify} becomes the WITH clause, {status} the status expression.
func With(q string) string {
	return strings.ReplaceAll(strings.ReplaceAll(q, "{classify}", classifySQL), "{status}", Status)
}

// MarkMissingLast24h counts the audit-mode call records of the last 24 hours with a missing or failed mark.
var MarkMissingLast24h = With(`{classify}
SELECT countIf(c_outcome = 'mark-missing') FROM otel_logs
WHERE Timestamp >= now() - INTERVAL 24 HOUR AND LogAttributes['ambit.accessMode'] = 'audit'`)
