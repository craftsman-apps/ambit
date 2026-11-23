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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// AppRecordKind marks the console's records of MCP app requests and decisions in otel_logs (LogAttributes['ambit.kind']).
const AppRecordKind = "app-approval"

// Actions of an AppEvent. A refused action is an attempt the console answered with an error: who tried, on what, and why.
const (
	AppRequested         = "requested"
	AppApproved          = "approved"
	AppPullRequestOpened = "pull-request-opened"
	AppDenied            = "denied"
	AppToolsApproved     = "tools-approved"
	AppApproveRefused    = "approve-refused"
	AppDenyRefused       = "deny-refused"
	AppToolsRefused      = "tools-approve-refused"
)

// eventReason is the Kubernetes Event reason of each action.
var eventReason = map[string]string{
	AppRequested: "ServerRequested", AppApproved: "ServerRequestApproved", AppPullRequestOpened: "ServerRequestPullRequestOpened",
	AppDenied: "ServerRequestDenied", AppToolsApproved: "ToolsApproved", AppApproveRefused: "ServerRequestApproveRefused",
	AppDenyRefused: "ServerRequestDenyRefused", AppToolsRefused: "ToolsApproveRefused",
}

func refusedAction(action string) bool { return strings.HasSuffix(action, "-refused") }

// AppEvent is one step of an MCP app's request and approval, as the Audit page and otel_logs hold it.
type AppEvent struct {
	Time        time.Time `json:"time"`
	Action      string    `json:"action"`
	Actor       string    `json:"actor"` // email from the console sign-in
	Role        string    `json:"role"`  // the actor's console role; empty for a GitOps approval seen at the merge
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

var appSendFailures = promauto.NewCounter(prometheus.CounterOpts{Name: "ambit_console_audit_send_failures_total",
	Help: "MCP app request and approval records the console could not deliver to the collector."})

// AppRecorder sends AppEvents to the OpenTelemetry collector over OTLP/gRPC, the endpoint the gateway and the guard
// use, so they reach otel_logs with the same retention. Events are rare (people's decisions), so each is sent at once.
type AppRecorder struct {
	conn *grpc.ClientConn
	logs collogs.LogsServiceClient
}

// NewAppRecorder does not connect until the first event, so a collector that starts later is no problem.
func NewAppRecorder(endpoint string) (*AppRecorder, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &AppRecorder{conn: conn, logs: collogs.NewLogsServiceClient(conn)}, nil
}

func (r *AppRecorder) send(ctx context.Context, ev AppEvent) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := r.logs.Export(ctx, appLogRequest(ev))
	return err
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// appLogRequest is the OTLP record of ev. It never sets jwt.email, so no query of gateway calls counts it.
func appLogRequest(ev AppEvent) *collogs.ExportLogsServiceRequest {
	tools := ""
	if len(ev.Tools) > 0 {
		b, _ := json.Marshal(ev.Tools)
		tools = string(b)
	}
	attrs := []*commonpb.KeyValue{strAttr("ambit.kind", AppRecordKind), strAttr("ambit.app.action", ev.Action),
		strAttr("ambit.actor", ev.Actor), strAttr("ambit.actor.role", ev.Role), strAttr("mcp.target", ev.Server)}
	for _, kv := range [][2]string{{"ambit.app.request", ev.Request}, {"ambit.app.requested_by", ev.RequestedBy},
		{"ambit.app.image", ev.Image}, {"ambit.app.url", ev.URL}, {"ambit.app.comment", ev.Comment},
		{"ambit.app.pull_request", ev.PullRequest}, {"ambit.app.tools", tools}, {"ambit.app.refused_reason", ev.Reason}} {
		if kv[1] != "" {
			attrs = append(attrs, strAttr(kv[0], kv[1]))
		}
	}
	sev, sevText := logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO"
	if refusedAction(ev.Action) {
		sev, sevText = logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN"
	}
	now := uint64(time.Now().UnixNano())
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "ambit-console")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(ev.Time.UnixNano()), ObservedTimeUnixNano: now, SeverityNumber: sev, SeverityText: sevText,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: AppRecordKind}}, Attributes: attrs,
		}}}},
	}}}
}

// recordApp keeps one step: a record in otel_logs (when evidence is on) and a Kubernetes Event on obj (the
// ServerRequest or Server; nil when there is none, e.g. a refused attempt on a request that does not exist). A failure
// is logged and counted, never returned: the change already happened, and the ServerRequest object stays its record.
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
	if err := c.AppAudit.send(ctx, ev); err != nil {
		appSendFailures.Inc()
		slog.Error("app audit record not delivered to the collector", "action", ev.Action, "server", ev.Server, "request", ev.Request, "by", ev.Actor, "err", err)
	}
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
