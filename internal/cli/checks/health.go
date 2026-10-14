package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// H1: both releases deployed, at the same version.
func h1Releases(_ context.Context, e *Env) Result {
	crds, err := e.Helm.Get("ambit-crds")
	if err != nil {
		return releaseErr(err, e.Namespace)
	}
	app, err := e.Helm.Get("ambit")
	if err != nil {
		return releaseErr(err, e.Namespace)
	}
	switch {
	case crds == nil || app == nil:
		return res(Fail, "live objects", "a release is missing in %s (ambit-crds: %v, ambit: %v)", e.Namespace, crds != nil, app != nil)
	case crds.Status != "deployed" || app.Status != "deployed":
		return res(Fail, "live objects", "ambit-crds is %s and ambit is %s", crds.Status, app.Status).
			next(e.helm() + " history ambit")
	case crds.ChartVersion != app.ChartVersion:
		return res(Fail, "live objects", "ambit-crds %s and ambit %s are at different versions", crds.ChartVersion, app.ChartVersion)
	}
	return res(Pass, "live objects", "ambit-crds and ambit %s are deployed", app.ChartVersion)
}

// ambitCRDNames are the CRDs of the ambit-crds chart. TestAmbitCRDNamesMatchChart keeps the list in step.
var ambitCRDNames = []string{
	"accessrules.ambitproject.dev", "budgets.ambitproject.dev", "connections.ambitproject.dev", "modelrouters.ambitproject.dev",
	"models.ambitproject.dev", "serverrequests.ambitproject.dev", "servers.ambitproject.dev",
}

// H2: the third-party CRDs and Ambit's CRDs are Established.
func h2CRDs(ctx context.Context, e *Env) Result {
	var list apiextensionsv1.CustomResourceDefinitionList
	if err := e.Ctrl.List(ctx, &list); err != nil {
		return unknownFor(err, "list", "customresourcedefinitions", "", "live objects")
	}
	have := map[string]bool{}
	ambit := 0
	for _, c := range list.Items {
		ok := false
		for _, cond := range c.Status.Conditions {
			ok = ok || (cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue)
		}
		have[c.Name] = ok
		if c.Spec.Group == ambitv1alpha1.GroupVersion.Group {
			ambit++
			if !ok {
				return res(Fail, "live objects", "CRD %s is not Established", c.Name)
			}
		}
	}
	names, err := kube.CRDNames(e.CRDBundle)
	if err != nil {
		return res(Fail, "live objects", "cannot read the third-party CRD bundle: %v", err)
	}
	var bad []string
	for _, n := range names {
		if !have[n] {
			bad = append(bad, n)
		}
	}
	if ambit == 0 {
		bad = append(bad, "the ambitproject.dev CRDs (release ambit-crds)")
	} else {
		for _, n := range ambitCRDNames { // some Ambit CRDs are not all of them
			if !have[n] {
				bad = append(bad, n)
			}
		}
	}
	if len(bad) > 0 {
		return res(Fail, "live objects", "missing or not Established: %s", strings.Join(bad, ", "))
	}
	return res(Pass, "live objects", "%d third-party and %d Ambit CRDs are Established", len(names), ambit)
}

// H3: every pod in the namespace is Ready, or the reason it is not.
func h3Pods(ctx context.Context, e *Env) Result {
	pods, err := e.Kube.CoreV1().Pods(e.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return unknownFor(err, "list", "pods", e.Namespace, "live objects")
	}
	var bad []string
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		n++
		ready := false
		for _, c := range p.Status.Conditions {
			ready = ready || (c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue)
		}
		if !ready {
			bad = append(bad, p.Name+": "+kube.PodReason(p, func(pod, c string) string { return lastLog(ctx, e, pod, c) }))
		}
	}
	switch {
	case n == 0:
		return res(Fail, "live objects", "no pods in %s", e.Namespace)
	case len(bad) > 0:
		return res(Fail, "live objects", "%d of %d pods not Ready: %s", len(bad), n, strings.Join(bad, "; ")).
			next(e.kubectl() + " describe pod " + strings.SplitN(bad[0], ":", 2)[0])
	}
	return res(Pass, "live objects", "%d pods Ready", n)
}

func lastLog(ctx context.Context, e *Env, pod, container string) string {
	tail := int64(1)
	b, err := e.Kube.CoreV1().Pods(e.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: true, TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		return "(no log)"
	}
	return string(b)
}

// probe sends GET path through a port-forward; it is one of the two active checks (spec 3, principle 5).
func probe(ctx context.Context, e *Env, selector string, port int, path, what string) Result {
	local, stop, err := e.Forward(ctx, selector, port)
	if err != nil {
		if denied(err) {
			return res(Unknown, "live objects", "cannot create pods/portforward in %s, so %s was not probed", e.Namespace, what)
		}
		return res(Fail, "live objects", "%s: no port-forward: %v", what, err)
	}
	defer stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", local, path), nil)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return res(Fail, "live objects", "%s does not answer: %v", what, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res(Fail, "live objects", "%s answered HTTP %d on %s", what, resp.StatusCode, path)
	}
	return res(Pass, "live objects", "%s answers (active probe through a port-forward)", what)
}

// ProbeTarget is the readiness probe of the first container that has an HTTP one, else port 8080 and "/".
func ProbeTarget(p *corev1.Pod) (int, string) {
	for _, c := range p.Spec.Containers {
		if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil {
			continue
		}
		g := c.ReadinessProbe.HTTPGet
		if g.Port.IntValue() > 0 {
			return g.Port.IntValue(), g.Path
		}
		for _, cp := range c.Ports {
			if cp.Name == g.Port.String() {
				return int(cp.ContainerPort), g.Path
			}
		}
	}
	return 8080, "/"
}

// H4: the gateway answers its own readiness endpoint.
func h4Gateway(ctx context.Context, e *Env) Result {
	pods, err := e.Kube.CoreV1().Pods(e.Namespace).List(ctx, metav1.ListOptions{LabelSelector: kube.GatewaySelector})
	if err != nil {
		return unknownFor(err, "list", "pods", e.Namespace, "live objects")
	}
	if len(pods.Items) == 0 {
		return res(Fail, "live objects", "no gateway pod (%s)", kube.GatewaySelector)
	}
	port, path := ProbeTarget(&pods.Items[0])
	return probe(ctx, e, kube.GatewaySelector, port, path, "the gateway")
}

// H5: Dex's discovery document.
func h5Dex(ctx context.Context, e *Env) Result {
	return probe(ctx, e, "app.kubernetes.io/name=dex", 5556, "/dex/.well-known/openid-configuration", "Dex")
}

// H6: the chart's ClickHouse accepts a ping when evidence is on.
func h6ClickHouse(ctx context.Context, e *Env) Result {
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values")
	}
	if !boolean(all, "evidence.enabled") {
		return res(NA, "values", "evidence.enabled is false")
	}
	if h := str(all, "evidence.clickhouse.external.host"); h != "" {
		return res(NA, "values", "external ClickHouse %s is not probed by the CLI", h)
	}
	return probe(ctx, e, "app.kubernetes.io/name=ambit-clickhouse", 8123, "/ping", "ClickHouse")
}

// H7: every Model, Server and AccessRule is Ready.
func h7Objects(ctx context.Context, e *Env) Result {
	type obj struct {
		kind, name string
		conds      []metav1.Condition
	}
	var objs []obj
	var models ambitv1alpha1.ModelList
	var servers ambitv1alpha1.ServerList
	var rules ambitv1alpha1.AccessRuleList
	for _, l := range []struct {
		list client.ObjectList
		res  string
	}{{&models, "models.ambitproject.dev"}, {&servers, "servers.ambitproject.dev"}, {&rules, "accessrules.ambitproject.dev"}} {
		if err := e.Ctrl.List(ctx, l.list, client.InNamespace(e.Namespace)); err != nil {
			return unknownFor(err, "list", l.res, e.Namespace, "live objects")
		}
	}
	for _, m := range models.Items {
		objs = append(objs, obj{"Model", m.Name, m.Status.Conditions})
	}
	for _, s := range servers.Items {
		objs = append(objs, obj{"Server", s.Name, s.Status.Conditions})
	}
	for _, r := range rules.Items {
		objs = append(objs, obj{"AccessRule", r.Name, r.Status.Conditions})
	}
	var bad []string
	for _, o := range objs {
		if !meta.IsStatusConditionTrue(o.conds, "Ready") {
			msg := "no Ready condition yet"
			if c := meta.FindStatusCondition(o.conds, "Ready"); c != nil {
				msg = c.Message
			}
			bad = append(bad, fmt.Sprintf("%s %s: %s", o.kind, o.name, msg))
		}
	}
	if len(bad) > 0 {
		return res(Fail, "live objects", "not Ready: %s", strings.Join(bad, "; "))
	}
	return res(Pass, "live objects", "%d Models, %d Servers and %d AccessRules are Ready", len(models.Items), len(servers.Items), len(rules.Items))
}

// H8: with --deep, one test call per Ready Model through the gateway (costs a few tokens each).
func h8Models(ctx context.Context, e *Env) Result {
	if !e.Deep {
		return res(NA, "live objects", "provider answers not checked; --deep sends one test call per model (a few tokens each)")
	}
	if e.Token == "" {
		return res(Unknown, "live objects", "no gateway token — set AMBIT_TOKEN to a token for the gateway")
	}
	_, all, err := e.values()
	if err != nil {
		return e.valuesUnknown("values")
	}
	var models ambitv1alpha1.ModelList
	if err := e.Ctrl.List(ctx, &models, client.InNamespace(e.Namespace)); err != nil {
		return unknownFor(err, "list", "models.ambitproject.dev", e.Namespace, "live objects")
	}
	var bad []string
	n := 0
	for _, m := range models.Items {
		if !meta.IsStatusConditionTrue(m.Status.Conditions, "Ready") {
			continue
		}
		n++
		if code, body, err := deepCall(ctx, e, all, m.Spec.Model); err != nil || code != http.StatusOK {
			bad = append(bad, fmt.Sprintf("%s (%s): %s", m.Name, m.Spec.Model, deepFailure(e, code, body, err)))
		}
	}
	switch {
	case n == 0:
		return res(NA, "live objects", "no Ready Model to call")
	case len(bad) > 0:
		return res(Fail, "live objects", "test call failed: %s", strings.Join(bad, "; "))
	}
	return res(Pass, "live objects", "%d Models answered a test call (active)", n)
}

// LLMURL is the gateway's LLM address: llm.host with the scheme and port of console.url.
func LLMURL(all map[string]any) string {
	u, err := url.Parse(str(all, "console.url"))
	if err != nil || u.Scheme == "" {
		return "https://" + str(all, "llm.host")
	}
	host := str(all, "llm.host")
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return u.Scheme + "://" + host
}

// deepFailure names the cause of a failed test call: no answer, our own refusal (401, 403), an unknown or not
// ready model (404), or a provider error (429, 5xx), with a short body excerpt without the token.
func deepFailure(e *Env, code int, body string, err error) string {
	if err != nil {
		return fmt.Sprintf("no answer from the gateway: %v", err)
	}
	var cause string
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		cause = "the gateway rejected the token or a rule"
	case code == http.StatusNotFound:
		cause = "model not found or not ready"
	default:
		cause = "provider error"
	}
	body = strings.Join(strings.Fields(strings.ReplaceAll(body, e.Token, "[redacted]")), " ")
	if len(body) > 120 {
		body = body[:120]
	}
	return fmt.Sprintf("%s (HTTP %d) %s", cause, code, body)
}

func deepCall(ctx context.Context, e *Env, all map[string]any, model string) (int, string, error) {
	b, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 5, "messages": []map[string]string{{"role": "user", "content": "Say OK."}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, LLMURL(all)+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	sb.Write(buf[:n])
	return resp.StatusCode, sb.String(), nil
}
