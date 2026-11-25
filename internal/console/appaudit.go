package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/craftsman-apps/ambit/internal/apprecord"
	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// AppRecordKind marks the records of MCP app requests and decisions in otel_logs (LogAttributes['ambit.kind']).
const AppRecordKind = apprecord.Kind

// Actions of an AppEvent (see apprecord).
const (
	AppRequested         = apprecord.Requested
	AppApproved          = apprecord.Approved
	AppPullRequestOpened = apprecord.PullRequestOpened
	AppDenied            = apprecord.Denied
	AppToolsApproved     = apprecord.ToolsApproved
	AppApproveRefused    = apprecord.ApproveRefused
	AppDenyRefused       = apprecord.DenyRefused
	AppToolsRefused      = apprecord.ToolsRefused
)

// eventReason is the Kubernetes Event reason of each action.
var eventReason = map[string]string{
	AppRequested: "ServerRequested", AppApproved: "ServerRequestApproved", AppPullRequestOpened: "ServerRequestPullRequestOpened",
	AppDenied: "ServerRequestDenied", AppToolsApproved: "ToolsApproved", AppApproveRefused: "ServerRequestApproveRefused",
	AppDenyRefused: "ServerRequestDenyRefused", AppToolsRefused: "ToolsApproveRefused",
}

func refusedAction(action string) bool { return apprecord.Refused(action) }

// AppEvent is one step of an MCP app's request and approval, as the Audit page and otel_logs hold it.
type AppEvent = apprecord.Event

// appSendFailures counts records not delivered to the collector, by reason (queue_full, send_failed, closed).
var appSendFailures = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_console_audit_send_failures_total",
	Help: "MCP app request and approval records the console could not deliver to the collector, by reason."}, []string{"reason"})

// AppRecorder sends AppEvents to the OpenTelemetry collector over OTLP/gRPC, the endpoint the gateway and the guard
// use, so they reach otel_logs with the same retention. Send queues the record and returns at once (the same queue as
// the guard's), so a collector that is down or slow never delays a decision; a record that cannot be delivered is
// counted in appSendFailures.
type AppRecorder struct{ exp *otlplog.Exporter }

// NewAppRecorder does not connect until the first event, so a collector that starts later is no problem.
func NewAppRecorder(endpoint string) (*AppRecorder, error) {
	exp, err := otlplog.New(endpoint, "ambit-console", appSendFailures)
	if err != nil {
		return nil, err
	}
	return &AppRecorder{exp: exp}, nil
}

func (r *AppRecorder) send(ev AppEvent) { r.exp.Send(apprecord.Record(ev)) }

// Flush sends what is queued (tests); Close sends it and stops, within otlplog.Timeout.
func (r *AppRecorder) Flush() { r.exp.Flush() }
func (r *AppRecorder) Close() { r.exp.Close() }

// recordApp keeps one step: a record in otel_logs (when evidence is on) and a Kubernetes Event on obj (the
// ServerRequest or Server; nil when there is none, e.g. a refused attempt on a request that does not exist). A failure
// is counted (appSendFailures) and logged, never returned: the change already happened, and the ServerRequest object stays its record.
func (c *Catalog) recordApp(ctx context.Context, ev AppEvent, obj client.Object) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	ev.Time = ev.Time.UTC()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second) // the browser may leave; the record stays
	defer cancel()
	if refusedAction(ev.Action) {
		slog.Warn("app action refused", "action", ev.Action, "server", ev.Server, "request", ev.Request, "by", ev.Actor, "role", ev.Role, "reason", ev.Reason)
	}
	if obj != nil {
		c.kubeEvent(ctx, ev, obj)
	}
	if c.AppAudit == nil {
		return
	}
	c.AppAudit.send(ev)
}

func (c *Catalog) kubeEvent(ctx context.Context, ev AppEvent, obj client.Object) {
	gvk, err := apiutil.GVKForObject(obj, c.Client.Scheme())
	if err != nil {
		slog.Error("kubernetes event not recorded", "action", ev.Action, "err", err)
		return
	}
	typ := corev1.EventTypeNormal
	if refusedAction(ev.Action) {
		typ = corev1.EventTypeWarning
	}
	msg := fmt.Sprintf("%s: server %s, by %s", ev.Action, ev.Server, ev.Actor)
	if ev.Role != "" {
		msg += " (" + ev.Role + ")"
	}
	if ev.Reason != "" {
		msg += ": " + ev.Reason
	}
	at := metav1.NewTime(ev.Time)
	e := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: obj.GetName() + ".", Namespace: obj.GetNamespace()},
		InvolvedObject: corev1.ObjectReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Namespace: obj.GetNamespace(),
			Name: obj.GetName(), UID: obj.GetUID(), ResourceVersion: obj.GetResourceVersion()},
		Reason: eventReason[ev.Action], Message: msg, Type: typ, Source: corev1.EventSource{Component: "ambit-console"},
		FirstTimestamp: at, LastTimestamp: at, Count: 1,
	}
	if err := c.Client.Create(ctx, e); err != nil {
		slog.Error("kubernetes event not recorded", "action", ev.Action, "object", obj.GetName(), "err", err)
	}
}

// appEventsSelect reads the app events of a period, optionally of one person; scanAppEvent reads one row of it.
const appEventsSelect = `SELECT Timestamp, LogAttributes['ambit.app.action'], LogAttributes['ambit.actor'], LogAttributes['ambit.actor.role'],
       LogAttributes['mcp.target'], LogAttributes['ambit.app.request'], LogAttributes['ambit.app.requested_by'],
       LogAttributes['ambit.app.image'], LogAttributes['ambit.app.url'], LogAttributes['ambit.app.comment'],
       LogAttributes['ambit.app.pull_request'], LogAttributes['ambit.app.tools'], LogAttributes['ambit.app.refused_reason']
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = '` + AppRecordKind + `'
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
  AND ({email:String} = '' OR lower(LogAttributes['ambit.actor']) = lower({email:String})
       OR lower(LogAttributes['ambit.app.requested_by']) = lower({email:String}))`

var appEventsSQL = appEventsSelect + `
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}`

func scanAppEvent(rows *sql.Rows) (AppEvent, error) {
	var e AppEvent
	var tools string
	if err := rows.Scan(&e.Time, &e.Action, &e.Actor, &e.Role, &e.Server, &e.Request, &e.RequestedBy, &e.Image, &e.URL,
		&e.Comment, &e.PullRequest, &tools, &e.Reason); err != nil {
		return e, err
	}
	e.Tools = []string{}
	if tools != "" {
		_ = json.Unmarshal([]byte(tools), &e.Tools)
	}
	return e, nil
}

// AppEvents lists MCP app requests, decisions and refused attempts in [from, to), newest first. A non-empty email keeps
// the events that person did or that concern their request.
func (a *Audit) AppEvents(ctx context.Context, email string, from, to time.Time, limit int) ([]AppEvent, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{"email": strings.TrimSpace(email),
		"from": from.UTC().Format("2006-01-02 15:04:05.000000000"), "to": to.UTC().Format("2006-01-02 15:04:05.000000000"),
		"limit": fmt.Sprint(limit)}))
	rows, err := a.DB.QueryContext(ctx, appEventsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppEvent{}
	for rows.Next() {
		e, err := scanAppEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
