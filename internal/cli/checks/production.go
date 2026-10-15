package checks

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/auditsql"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

const installDocs = "https://ambitproject.dev/docs/install#production-checklist"

// P1: gateway.replicas >= 2, a PodDisruptionBudget, and at least 2 Ready gateway replicas.
func p1Gateway(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, PDB, Deployment")
	}
	src := "values, PDB, Deployment"
	if r := number(all, "gateway.replicas"); r < 2 {
		return res(Fail, src, "gateway.replicas is %d; production needs 2 or more", r).next("set gateway.replicas: 2 (" + installDocs + ")")
	}
	var pdbs policyv1.PodDisruptionBudgetList
	if err := e.Ctrl.List(ctx, &pdbs, client.InNamespace(e.Namespace)); err != nil {
		return unknownFor(err, "list", "poddisruptionbudgets", e.Namespace, src)
	}
	found := false
	for _, p := range pdbs.Items {
		found = found || (p.Spec.Selector != nil && p.Spec.Selector.MatchLabels["gateway.networking.k8s.io/gateway-name"] == "ambit")
	}
	if !found {
		return res(Fail, src, "no PodDisruptionBudget for the gateway")
	}
	deps, err := e.Kube.AppsV1().Deployments(e.Namespace).List(ctx, metav1.ListOptions{LabelSelector: kube.GatewaySelector})
	if err != nil {
		return unknownFor(err, "list", "deployments", e.Namespace, src)
	}
	ready := int32(0)
	for _, d := range deps.Items {
		ready += d.Status.ReadyReplicas
	}
	if ready < 2 {
		return res(Fail, src, "%d gateway replicas are Ready; 2 or more are needed", ready)
	}
	return res(Pass, src, "%d gateway replicas Ready, with a PodDisruptionBudget", ready)
}

// P2: bootstrapAdmin.enabled is false.
func p2Bootstrap(_ context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values")
	}
	if boolean(all, "bootstrapAdmin.enabled") {
		return res(Fail, "values", "bootstrapAdmin.enabled is true; turn it off once company sign-in works").
			next("helm upgrade with bootstrapAdmin.enabled=false (" + installDocs + ")")
	}
	return res(Pass, "values", "bootstrapAdmin.enabled is false")
}

// P3: HTTPS everywhere, and a TLS certificate that covers the hosts and does not expire within 14 days.
func p3TLS(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, Secret")
	}
	src := "values, Secret"
	if !strings.HasPrefix(str(all, "console.url"), "https://") {
		return res(Fail, src, "console.url %s does not use HTTPS", str(all, "console.url")).next("see " + "https://ambitproject.dev/docs/install#https")
	}
	if !boolean(all, "gateway.tls.enabled") || str(all, "gateway.tls.certificateSecret") == "" {
		return res(Fail, src, "gateway.tls is not set (enabled and certificateSecret)").next("see https://ambitproject.dev/docs/install#https")
	}
	name := str(all, "gateway.tls.certificateSecret")
	var s corev1.Secret
	err = e.Ctrl.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return res(Fail, src, "TLS Secret %s does not exist", name)
	}
	if err != nil {
		return unknownFor(err, "get", "secrets", e.Namespace, src)
	}
	block, _ := pem.Decode(s.Data["tls.crt"]) // the certificate only; the key is never read
	if block == nil {
		return res(Fail, src, "TLS Secret %s has no PEM certificate in tls.crt", name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return res(Fail, src, "TLS Secret %s: %v", name, err)
	}
	hosts := []string{str(all, "console.host")}
	if boolean(all, "llm.enabled") {
		hosts = append(hosts, str(all, "llm.host"))
	}
	if boolean(all, "mcp.enabled") {
		hosts = append(hosts, str(all, "mcp.host"))
	}
	for _, h := range hosts {
		if err := cert.VerifyHostname(h); err != nil {
			return res(Fail, src, "the certificate in %s does not cover %s", name, h)
		}
	}
	if left := cert.NotAfter.Sub(e.Now()); left < 14*24*time.Hour {
		return res(Fail, src, "the certificate in %s expires on %s", name, cert.NotAfter.UTC().Format("2006-01-02"))
	}
	return res(Pass, src, "HTTPS with a certificate for %s, valid until %s", strings.Join(hosts, ", "), cert.NotAfter.UTC().Format("2006-01-02"))
}

// P4: evidence is on and its pods are Ready; with --deep, a test call's audit record leaves the collector for
// ClickHouse (ruling S12).
func p4Evidence(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, pods")
	}
	src := "values, pods"
	if !boolean(all, "evidence.enabled") {
		return res(Fail, src, "evidence.enabled is false: calls are not recorded").next(installDocs)
	}
	selectors := []string{"app.kubernetes.io/name=ambit-otel-collector"}
	if str(all, "evidence.clickhouse.external.host") == "" {
		selectors = append(selectors, "app.kubernetes.io/name=ambit-clickhouse")
	}
	for _, sel := range selectors {
		pods, err := e.Kube.CoreV1().Pods(e.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return unknownFor(err, "list", "pods", e.Namespace, src)
		}
		ready := 0
		for _, p := range pods.Items {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					ready++
				}
			}
		}
		if ready == 0 {
			return res(Fail, src, "no Ready pod for %s", sel)
		}
	}
	if !e.Deep {
		return res(Pass, src, "evidence is on and its pods are Ready (records not checked; --deep checks them)")
	}
	if e.Token == "" {
		return res(Unknown, src, "evidence pods are Ready; no gateway token — set AMBIT_TOKEN to a token for the gateway")
	}
	return *e.deepRecords(ctx, all)
}

// deepRecords runs the --deep test call and the wait for its audit record once; P4 and P11 read the same result.
func (e *Env) deepRecords(ctx context.Context, all map[string]any) *Result {
	if e.records == nil {
		r := p4Records(ctx, e, all)
		e.records = &r
	}
	return e.records
}

// collectorMetricsPort is the port of the collector's own Prometheus metrics (charts/ambit/files/otel-collector.yaml,
// service.telemetry; TestCollectorExposesMetrics keeps the two in step).
const collectorMetricsPort = 8888

// p4Records makes one test call with the user's token and watches the collector's counter of log records the
// ClickHouse exporter sent. No Secret is read. The chart runs one collector pod (replicas: 1), so one
// port-forward sees every record.
func p4Records(ctx context.Context, e *Env, all map[string]any) Result {
	src := "values, pods, collector metrics"
	var models ambitv1alpha1.ModelList
	if err := e.Ctrl.List(ctx, &models, client.InNamespace(e.Namespace)); err != nil {
		return unknownFor(err, "list", "models.ambitproject.dev", e.Namespace, src)
	}
	model := ""
	for _, m := range models.Items {
		if meta.IsStatusConditionTrue(m.Status.Conditions, "Ready") {
			model = m.Spec.Model
			break
		}
	}
	if model == "" {
		return res(Unknown, src, "evidence pods are Ready; no Ready Model to make the test call with")
	}
	port, stop, err := e.Forward(ctx, "app.kubernetes.io/name=ambit-otel-collector", collectorMetricsPort)
	if err != nil {
		return unknownFor(err, "create", "pods/portforward", e.Namespace, src)
	}
	defer stop()
	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	before, err := sentLogRecords(ctx, e.HTTP, metricsURL)
	if err != nil {
		return res(Unknown, src, "the collector's metrics endpoint does not answer: %v", err)
	}
	if code, _, err := deepCall(ctx, e, all, model); err != nil || code != http.StatusOK {
		return res(Fail, src, "the test call for the audit check failed: HTTP %d %v", code, err)
	}
	wait := e.DeepWait
	if wait == 0 {
		wait = 60 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		after, err := sentLogRecords(ctx, e.HTTP, metricsURL)
		if err == nil && after > before {
			return res(Pass, src, "audit pipeline: the collector sent records to ClickHouse after the test call")
		}
		if time.Now().After(deadline) {
			return res(Fail, src, "the collector sent no record to ClickHouse within %s of a test call", wait).
				next(e.kubectl() + " logs deploy/ambit-otel-collector")
		}
		select {
		case <-ctx.Done():
			return res(Unknown, src, "interrupted while waiting for the collector to send records")
		case <-time.After(wait / 20):
		}
	}
}

// sentLogRecords reads the collector's counter otelcol_exporter_sent_log_records (Prometheus adds _total) for
// the ClickHouse exporter. A collector that has sent nothing yet has no such line: that counts as 0.
func sentLogRecords(ctx context.Context, c *http.Client, url string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var sum float64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "otelcol_exporter_sent_log_records") || !strings.Contains(line, `exporter="clickhouse"`) {
			continue
		}
		f := strings.Fields(line)
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
			sum += v
		}
	}
	return sum, sc.Err()
}

// P5: limits are on, and limits.failureMode is set by the user (read from the user-supplied values).
func p5Limits(_ context.Context, e *Env) Result {
	user, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values (user-supplied)")
	}
	src := "values (user-supplied)"
	if !boolean(all, "limits.enabled") {
		return res(Fail, src, "limits.enabled is false: no budgets").next(installDocs)
	}
	if _, set := get(user, "limits.failureMode"); !set {
		return res(Warn, src, "limits.failureMode is not set explicitly; the effective value is %s", str(all, "limits.failureMode")).
			next("set limits.failureMode to FailOpen or FailClosed")
	}
	return res(Pass, src, "limits are on, failureMode %s", str(all, "limits.failureMode"))
}

// P6: the budget store is an external Valkey (the chart's has no persistence). A warning, not a failure.
func p6Valkey(_ context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values")
	}
	if !boolean(all, "limits.enabled") {
		return res(NA, "values", "limits.enabled is false")
	}
	if a := str(all, "limits.valkey.external.address"); a != "" {
		return res(Pass, "values", "budgets are kept in the external Valkey %s", a)
	}
	return res(Warn, "values", "budgets are kept in the chart's Valkey, which loses the counts on a restart").
		next("set limits.valkey.external.address to a Valkey or Redis with persistence")
}

// P7: protection is on when any MCP Server exists.
func p7Protection(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, objects")
	}
	var servers ambitv1alpha1.ServerList
	if err := e.Ctrl.List(ctx, &servers, client.InNamespace(e.Namespace)); err != nil {
		return unknownFor(err, "list", "servers.ambitproject.dev", e.Namespace, "values, objects")
	}
	if len(servers.Items) == 0 {
		return res(NA, "values, objects", "no MCP Server")
	}
	if !boolean(all, "protection.enabled") {
		return res(Fail, "values, objects", "%d MCP Servers exist, but protection.enabled is false", len(servers.Items)).next(installDocs)
	}
	return res(Pass, "values, objects", "protection is on for %d MCP Servers", len(servers.Items))
}

// P8: alerts are on when the Prometheus Operator CRDs exist.
func p8Alerts(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, CRDs")
	}
	var crd apiextensionsv1.CustomResourceDefinition
	err = e.Ctrl.Get(ctx, client.ObjectKey{Name: "prometheusrules.monitoring.coreos.com"}, &crd)
	if apierrors.IsNotFound(err) {
		return res(NA, "values, CRDs", "the Prometheus Operator is not installed")
	}
	if err != nil {
		return unknownFor(err, "get", "customresourcedefinitions", "", "values, CRDs")
	}
	if !boolean(all, "alerts.enabled") {
		return res(Fail, "values, CRDs", "the Prometheus Operator is installed, but alerts.enabled is false").
			next("set alerts.enabled: true (https://ambitproject.dev/docs/alerts)")
	}
	return res(Pass, "values, CRDs", "alert rules are on")
}

// P9: console.replicas is 1 and the console Deployment has 1 Ready replica.
func p9Console(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values, Deployment")
	}
	if r := number(all, "console.replicas"); r != 1 {
		return res(Fail, "values, Deployment", "console.replicas is %d; it must be 1", r)
	}
	d, err := e.Kube.AppsV1().Deployments(e.Namespace).Get(ctx, "ambit-console", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return res(Fail, "values, Deployment", "there is no ambit-console Deployment in namespace %s", e.Namespace).next(installDocs)
	}
	if err != nil {
		return unknownFor(err, "get", "deployments", e.Namespace, "values, Deployment")
	}
	if d.Status.ReadyReplicas != 1 {
		return res(Fail, "values, Deployment", "the console has %d Ready replicas, want 1", d.Status.ReadyReplicas)
	}
	return res(Pass, "values, Deployment", "one console replica, Ready")
}

// P10: provider credentials are in Secrets or the gateway's cloud identity.
func p10Credentials(ctx context.Context, e *Env) Result {
	src := "objects, Secret existence, values"
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown(src)
	}
	var models ambitv1alpha1.ModelList
	if err := e.Ctrl.List(ctx, &models, client.InNamespace(e.Namespace)); err != nil {
		return unknownFor(err, "list", "models.ambitproject.dev", e.Namespace, src)
	}
	if len(models.Items) == 0 {
		return res(NA, src, "no Model")
	}
	cloudIdentity := false
	if a, ok := get(all, "gateway.serviceAccount.annotations"); ok {
		m, _ := a.(map[string]any)
		cloudIdentity = len(m) > 0
	}
	var fails, warns []string
	for _, m := range models.Items {
		if m.Spec.CredentialsRef == nil {
			if !cloudIdentity {
				warns = append(warns, fmt.Sprintf("%s (%s) has no credentialsRef and gateway.serviceAccount.annotations is empty", m.Name, m.Spec.Provider))
			}
			continue
		}
		var s metav1.PartialObjectMetadata // existence only: the Secret's values are never read
		s.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		err := e.Ctrl.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: m.Spec.CredentialsRef.Name}, &s)
		switch {
		case apierrors.IsNotFound(err):
			fails = append(fails, fmt.Sprintf("%s: Secret %s does not exist", m.Name, m.Spec.CredentialsRef.Name))
		case err != nil:
			return unknownFor(err, "get", "secrets", e.Namespace, src)
		}
	}
	switch {
	case len(fails) > 0:
		return res(Fail, src, "%s", strings.Join(fails, "; "))
	case len(warns) > 0:
		return res(Warn, src, "%s", strings.Join(warns, "; ")).next("https://ambitproject.dev/docs/providers")
	}
	return res(Pass, src, "%d Models use Secrets or cloud identity", len(models.Items))
}

// P11: audit-only mode (access.mode: audit) is a trial, not a way to run in production: AccessRules are recorded,
// not enforced. The check states the mode, how long it has been on, and (with --deep) that the records reach the
// audit store; it never judges marks. A line under it counts the records of the last 24 h with a missing or failed
// mark.
func p11AuditMode(ctx context.Context, e *Env) Result {
	src := "values, ambit-access-mode"
	var cm corev1.ConfigMap
	err := e.Ctrl.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: "ambit-access-mode"}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return unknownFor(err, "get", "configmaps", e.Namespace, src)
	}
	published := err == nil
	mode := cm.Data["mode"]
	_, all, verr := e.values()
	if mode == "" {
		if verr != nil {
			return e.valuesUnknown(src)
		}
		mode = str(all, "access.mode")
	}
	var since time.Time
	if t, err := time.Parse(time.RFC3339, cm.Data["modeSince"]); err == nil {
		since = t
	}
	var r Result
	if mode != "audit" {
		r = res(Pass, src, "access.mode is enforce: AccessRules decide")
	} else {
		msg := "access.mode is audit (the controller has not published ambit-access-mode yet): AccessRules are recorded, not enforced"
		if published && !since.IsZero() {
			msg = fmt.Sprintf("access.mode is audit for %d days (since %s): AccessRules are recorded, not enforced",
				int(e.Now().Sub(since).Hours()/24), since.UTC().Format(time.RFC3339))
		}
		r = res(Warn, src, "%s", msg).next("set access.mode: enforce and run helm upgrade (https://ambitproject.dev/docs/guides/audit-only)")
		switch {
		case !e.Deep:
			r.Message += "; records not checked; --deep checks them"
		case e.Token == "":
			r = res(Unknown, src, "access.mode is audit; no gateway token — set AMBIT_TOKEN to a token for the gateway")
		case verr != nil:
			r = e.valuesUnknown(src)
		default:
			// Audit-only allows every request, so a record that does not reach the store leaves no evidence at all.
			switch rec := e.deepRecords(ctx, all); rec.Status {
			case Pass:
				r.Message += "; " + rec.Message
			case Fail:
				r = res(Fail, src, "access.mode is audit and its records do not reach the audit store: %s", rec.Message).next(rec.Next)
			default:
				r = res(Unknown, src, "access.mode is audit; delivery of records not checked: %s", rec.Message).next(rec.Next)
			}
		}
	}
	if l, ok := pendingLine(cm.Data["pending"], mode); ok {
		r.Lines = append(r.Lines, l)
		if r.Status == Pass {
			r.Status = Warn
		}
	}
	// The mark-missing line: while audit mode is on, and for 24 h after it was on.
	if mode == "audit" || (!since.IsZero() && e.Now().Sub(since) < 24*time.Hour) {
		l := markMissingLine(ctx, e, all, verr)
		r.Lines = append(r.Lines, l)
		if l.Status == Warn && r.Status == Pass {
			r.Status = Warn
		}
	}
	return r
}

// pendingLine reports a change that agentgateway has not applied (the controller's "pending" entry of
// ambit-access-mode). The controller's error already names the rejected object, the gateway's reason and the next
// step, so the line shows it as it is, the same text as the console banner and the AccessRule condition.
func pendingLine(raw, published string) (Line, bool) {
	if raw == "" {
		return Line{}, false
	}
	var p struct{ Mode, RulesVersion, Step, Error string }
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Line{Status: Unknown, Message: "ambit-access-mode has a pending entry that is not JSON; read the controller log"}, true
	}
	what := "rules change " + p.RulesVersion
	if p.Mode != published {
		what = "mode switch to " + p.Mode
	}
	cause := p.Error
	if cause == "" {
		cause = "waiting for agentgateway"
	}
	return Line{Status: Warn, Message: fmt.Sprintf("%s is not active (step %s): %s", what, p.Step, cause),
		Next: "the old rules stay in force until agentgateway accepts the change (https://ambitproject.dev/docs/guides/audit-only)"}, true
}

// markMissingLine runs the console's classification in the chart's ClickHouse pod and counts the audit-mode call
// records of the last 24 h whose mark is missing or failed. The pod's own credentials are used: the CLI never
// reads the ClickHouse password.
func markMissingLine(ctx context.Context, e *Env, all map[string]any, verr error) Line {
	const what = "audit-mode call records with a missing or failed mark (last 24 h)"
	if verr == nil {
		if h := str(all, "evidence.clickhouse.external.host"); h != "" {
			return Line{Status: Unknown, Message: fmt.Sprintf("%s: external ClickHouse %s is not reached by the CLI; the Audit page shows the count", what, h)}
		}
	}
	if e.Exec == nil {
		return Line{Status: Unknown, Message: what + ": the CLI cannot run a query here"}
	}
	cmd := []string{"sh", "-c", `clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --database "$CLICKHOUSE_DB" --query "$1"`, "sh", auditsql.MarkMissingLast24h}
	outp, err := e.Exec(ctx, "app.kubernetes.io/name=ambit-clickhouse", cmd)
	if err != nil {
		if denied(err) {
			return Line{Status: Unknown, Message: fmt.Sprintf("%s: cannot create pods/exec in %s", what, e.Namespace), Next: "ask for access to run a query in the ClickHouse pod, or read the count on the Audit page"}
		}
		return Line{Status: Unknown, Message: fmt.Sprintf("%s: ClickHouse did not answer: %v", what, err)}
	}
	n, err := strconv.Atoi(strings.TrimSpace(outp))
	if err != nil {
		return Line{Status: Unknown, Message: fmt.Sprintf("%s: ClickHouse answered %q", what, strings.TrimSpace(outp))}
	}
	if n > 0 {
		return Line{Status: Warn, Message: fmt.Sprintf("%s: %d", what, n),
			Next: "a rule expression failed or the audit fields were not active yet; run ambit check --production again in a few minutes, and report the rule if the count stays above zero"}
	}
	return Line{Status: Pass, Message: fmt.Sprintf("%s: 0", what)}
}
