// Package apprecord is the audit record of one step of an MCP app (an MCP server): its request, approval, denial,
// tool approval, refused attempt or removal. The console writes most steps and the controller writes the removal, so
// both build the record here: one set of field names in otel_logs (LogAttributes['ambit.kind'] = 'app-approval').
package apprecord

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/otlplog"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// Kind marks these records in otel_logs (LogAttributes['ambit.kind']).
const Kind = "app-approval"

// Actions of an Event. A refused action is an attempt the console answered with an error: who tried, on what, and why.
const (
	Requested         = "requested"
	Approved          = "approved"
	PullRequestOpened = "pull-request-opened"
	Denied            = "denied"
	ToolsApproved     = "tools-approved"
	Removed           = "removed" // the Server object was deleted
	ApproveRefused    = "approve-refused"
	DenyRefused       = "deny-refused"
	ToolsRefused      = "tools-approve-refused"
)

// Annotations on a Server. The console sets requested-by and server-request when an approval creates the Server, and
// removed-by and removed-by-role just before it deletes the Server for a signed-in person, so the controller can name
// that person in the removal record.
const (
	AnnRequestedBy   = "ambitproject.dev/requested-by"
	AnnApprovedBy    = "ambitproject.dev/approved-by"
	AnnRequest       = "ambitproject.dev/server-request"
	AnnRemovedBy     = "ambitproject.dev/removed-by"
	AnnRemovedByRole = "ambitproject.dev/removed-by-role"
)

// Refused reports whether action is a refused attempt.
func Refused(action string) bool { return strings.HasSuffix(action, "-refused") }

// Event is one step of an MCP app's request and approval, as the Audit page and otel_logs hold it.
type Event struct {
	Time        time.Time `json:"time"`
	Action      string    `json:"action"`
	Actor       string    `json:"actor"` // email from the console sign-in; empty for a removal Ambit saw but did not do
	Role        string    `json:"role"`  // the actor's console role; empty for a GitOps approval seen at the merge, or a removal outside the console
	Server      string    `json:"server"`
	Request     string    `json:"request"` // ServerRequest name; empty for tool approvals
	RequestedBy string    `json:"requestedBy"`
	Image       string    `json:"image"`
	URL         string    `json:"url"`
	Comment     string    `json:"comment"` // the request's reason, or the decision's comment
	PullRequest string    `json:"pullRequest"`
	Tools       []string  `json:"tools"`  // tools-approved: the tools whose new definitions were approved
	Reason      string    `json:"reason"` // refused actions: why the console refused
}

// Record is the OTLP record of ev. It never sets jwt.email, so no query of gateway calls counts it.
func Record(ev Event) *logspb.LogRecord {
	tools := ""
	if len(ev.Tools) > 0 {
		b, _ := json.Marshal(ev.Tools)
		tools = string(b)
	}
	attrs := []*commonpb.KeyValue{otlplog.Str("ambit.kind", Kind), otlplog.Str("ambit.app.action", ev.Action),
		otlplog.Str("ambit.actor", ev.Actor), otlplog.Str("ambit.actor.role", ev.Role), otlplog.Str("mcp.target", ev.Server)}
	for _, kv := range [][2]string{{"ambit.app.request", ev.Request}, {"ambit.app.requested_by", ev.RequestedBy},
		{"ambit.app.image", ev.Image}, {"ambit.app.url", ev.URL}, {"ambit.app.comment", ev.Comment},
		{"ambit.app.pull_request", ev.PullRequest}, {"ambit.app.tools", tools}, {"ambit.app.refused_reason", ev.Reason}} {
		if kv[1] != "" {
			attrs = append(attrs, otlplog.Str(kv[0], kv[1]))
		}
	}
	rec := otlplog.Record(ev.Time, Kind, attrs...)
	if Refused(ev.Action) {
		rec.SeverityNumber, rec.SeverityText = logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN"
	}
	rec.ObservedTimeUnixNano = uint64(time.Now().UnixNano())
	return rec
}
