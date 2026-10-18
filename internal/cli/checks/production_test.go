package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

func vals(all map[string]any) *fakeHelm { return &fakeHelm{all: all, user: map[string]any{}} }

func TestP1Gateway(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "ambit", Namespace: ns},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: gwLabels}}}
	two := map[string]any{"gateway": map[string]any{"replicas": 2}}
	want(t, one(t, "P1", fx{helm: vals(map[string]any{"gateway": map[string]any{"replicas": 1}})}), Fail, "gateway.replicas is 1")
	want(t, one(t, "P1", fx{helm: vals(two)}), Fail, "no PodDisruptionBudget")
	want(t, one(t, "P1", fx{helm: vals(two), ctrlObjs: []runtime.Object{pdb}, kubeObjs: []runtime.Object{deploy("ambit", gwLabels, 1)}}), Fail, "1 gateway replicas are Ready")
	want(t, one(t, "P1", fx{helm: vals(two), ctrlObjs: []runtime.Object{pdb}, kubeObjs: []runtime.Object{deploy("ambit", gwLabels, 2)}}), Pass, "2 gateway replicas Ready")
	want(t, one(t, "P1", fx{helm: vals(two), ctrlDenied: true}), Unknown, "cannot list poddisruptionbudgets")
}

func TestP2Bootstrap(t *testing.T) {
	want(t, one(t, "P2", fx{helm: vals(map[string]any{"bootstrapAdmin": map[string]any{"enabled": true}})}), Fail, "bootstrapAdmin.enabled is true")
	want(t, one(t, "P2", fx{helm: vals(map[string]any{"bootstrapAdmin": map[string]any{"enabled": false}})}), Pass, "is false")
}

func TestP3TLS(t *testing.T) {
	v := map[string]any{"console": map[string]any{"url": "https://console.example.com", "host": "console.example.com"},
		"llm":     map[string]any{"enabled": true, "host": "llm.example.com"},
		"gateway": map[string]any{"tls": map[string]any{"enabled": true, "certificateSecret": "ambit-tls"}}}
	good := secret("ambit-tls", map[string][]byte{"tls.crt": certPEM(t, now.Add(90*24*time.Hour), "console.example.com", "llm.example.com"), "tls.key": []byte("not read")})
	short := secret("ambit-tls", map[string][]byte{"tls.crt": certPEM(t, now.Add(5*24*time.Hour), "console.example.com", "llm.example.com")})
	narrow := secret("ambit-tls", map[string][]byte{"tls.crt": certPEM(t, now.Add(90*24*time.Hour), "console.example.com")})
	want(t, one(t, "P3", fx{helm: vals(map[string]any{"console": map[string]any{"url": "http://console.localtest.me:8080"}})}), Fail, "does not use HTTPS")
	want(t, one(t, "P3", fx{helm: vals(v), ctrlObjs: []runtime.Object{good}}), Pass, "valid until 2027-01-12")
	want(t, one(t, "P3", fx{helm: vals(v), ctrlObjs: []runtime.Object{short}}), Fail, "expires on 2026-10-19")
	want(t, one(t, "P3", fx{helm: vals(v), ctrlObjs: []runtime.Object{narrow}}), Fail, "does not cover llm.example.com")
	want(t, one(t, "P3", fx{helm: vals(v)}), Fail, "does not exist")
	want(t, one(t, "P3", fx{helm: vals(v), ctrlDenied: true}), Unknown, "cannot get secrets in ambit-system")
}

func TestP4Evidence(t *testing.T) {
	on := map[string]any{"evidence": map[string]any{"enabled": true}}
	pods := []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"}),
		readyPod("ch", map[string]string{"app.kubernetes.io/name": "ambit-clickhouse"})}
	want(t, one(t, "P4", fx{helm: vals(map[string]any{})}), Fail, "evidence.enabled is false")
	want(t, one(t, "P4", fx{helm: vals(on)}), Fail, "no Ready pod for app.kubernetes.io/name=ambit-otel-collector")
	want(t, one(t, "P4", fx{helm: vals(on), kubeObjs: pods}), Pass, "records not checked; --deep checks them")
	want(t, one(t, "P4", fx{helm: vals(on), kubeObjs: pods, deep: true}), Unknown, "set AMBIT_TOKEN")
}

// Ruling S12: --deep makes one test call with AMBIT_TOKEN and watches the collector's counter of records sent to
// ClickHouse, before and after. No Secret is read, so an external ClickHouse is checked the same way.
func TestP4Deep(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer user-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	t.Cleanup(gw.Close)
	external := map[string]any{
		"console":  map[string]any{"url": gw.URL}, // the test gateway answers on the console's port
		"llm":      map[string]any{"host": "127.0.0.1"},
		"evidence": map[string]any{"enabled": true, "clickhouse": map[string]any{"external": map[string]any{"host": "ch.example.com"}}},
	}
	collector := []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"})}
	ready := []runtime.Object{model("local-test", "OpenAI", "ambit-llm-local", true)}
	// counter serves the counter: the first scrape (before the call) sees `first`, every later one `later`.
	counter := func(first, later string) http.HandlerFunc {
		var n atomic.Int32
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			v := later
			if n.Add(1) == 1 {
				v = first
			}
			fmt.Fprintln(w, "# TYPE otelcol_exporter_sent_log_records_total counter")
			if v != "" {
				fmt.Fprintf(w, "otelcol_exporter_sent_log_records_total{exporter=\"clickhouse\",service_name=\"otelcol-contrib\"} %s\n", v)
			}
			fmt.Fprintln(w, `otelcol_exporter_sent_log_records_total{exporter="debug"} 900`)
		}
	}
	base := fx{helm: vals(external), kubeObjs: collector, ctrlObjs: ready, deep: true, token: "user-token-1", deepWait: 200 * time.Millisecond}
	with := func(f func(*fx)) fx { c := base; f(&c); return c }

	want(t, one(t, "P4", with(func(f *fx) { f.forward = counter("41", "42") })), Pass, pinnedP4Line)
	want(t, one(t, "P4", with(func(f *fx) { f.forward = counter("", "1") })), Pass, pinnedP4Line) // nothing sent before: 0
	want(t, one(t, "P4", with(func(f *fx) { f.forward = counter("41", "41") })), Fail, "sent no record to ClickHouse within 200ms of a test call")
	want(t, one(t, "P4", with(func(f *fx) { f.forward = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) } })), Unknown, "metrics endpoint does not answer: HTTP 503")
	want(t, one(t, "P4", with(func(f *fx) { f.forwardErr = forbidden })), Unknown, "cannot create pods/portforward in ambit-system")
	want(t, one(t, "P4", with(func(f *fx) { f.forward = counter("41", "42"); f.token = "wrong" })), Fail, "test call for the audit check failed: HTTP 401")
	want(t, one(t, "P4", with(func(f *fx) { f.forward = counter("41", "42"); f.ctrlObjs = nil })), Unknown, "no Ready Model")
}

// Final review, Task 13 #1: Ctrl+C ends the wait for the audit record at once (it used to wait up to 60 s).
func TestP4DeepStopsOnCancel(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	t.Cleanup(gw.Close)
	f := fx{helm: vals(map[string]any{
		"console":  map[string]any{"url": gw.URL},
		"llm":      map[string]any{"host": "127.0.0.1"},
		"evidence": map[string]any{"enabled": true, "clickhouse": map[string]any{"external": map[string]any{"host": "ch.example.com"}}},
	}),
		kubeObjs: []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"})},
		ctrlObjs: []runtime.Object{model("local-test", "OpenAI", "ambit-llm-local", true)},
		deep:     true, token: "user-token-1", deepWait: 30 * time.Second,
		forward: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, `otelcol_exporter_sent_log_records_total{exporter="clickhouse"} 41`) // never grows
		}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	var chk Check
	for _, c := range Catalogue {
		if c.ID == "P4" {
			chk = c
		}
	}
	start := time.Now()
	r := runOne(ctx, f.env(t), chk)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("P4 took %s after the cancel", time.Since(start))
	}
	want(t, r, Unknown, "interrupted")
}

func TestSentLogRecords(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `# HELP otelcol_exporter_sent_log_records_total Number of log record successfully sent to destination.
otelcol_exporter_sent_log_records_total{exporter="clickhouse",service_instance_id="a"} 7
otelcol_exporter_sent_log_records_total{exporter="clickhouse",service_instance_id="b"} 3
otelcol_exporter_sent_log_records{exporter="clickhouse"} 5
otelcol_exporter_send_failed_log_records_total{exporter="clickhouse"} 100
otelcol_exporter_sent_log_records_total{exporter="debug"} 100
`)
	}))
	t.Cleanup(s.Close)
	if n, err := sentLogRecords(context.Background(), http.DefaultClient, s.URL); err != nil || n != 15 {
		t.Fatalf("sentLogRecords = %v, %v; want 15", n, err)
	}
}

// P4 --deep reads the collector's metrics on collectorMetricsPort; the chart's collector config must keep them.
func TestCollectorExposesMetrics(t *testing.T) {
	b, err := os.ReadFile("../../../charts/ambit/files/otel-collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fmt.Sprintf("prometheus: { host: 0.0.0.0, port: %d }", collectorMetricsPort)) {
		t.Fatalf("charts/ambit/files/otel-collector.yaml does not expose the collector's metrics on port %d", collectorMetricsPort)
	}
	d, err := os.ReadFile("../../../charts/ambit/templates/evidence/collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), "replicas: 1\n") {
		t.Fatal("the collector Deployment no longer has replicas: 1; P4 --deep reads one pod's counter and must sum every pod")
	}
}

func TestP5Limits(t *testing.T) {
	all := map[string]any{"limits": map[string]any{"enabled": true, "failureMode": "FailOpen"}}
	want(t, one(t, "P5", fx{helm: vals(map[string]any{})}), Fail, "limits.enabled is false")
	want(t, one(t, "P5", fx{helm: &fakeHelm{all: all, user: map[string]any{"limits": map[string]any{"enabled": true}}}}), Warn, "the effective value is FailOpen")
	// A value equal to the default still counts as a decision when the user wrote it (spec 4.3 P5).
	want(t, one(t, "P5", fx{helm: &fakeHelm{all: all, user: map[string]any{"limits": map[string]any{"failureMode": "FailOpen"}}}}), Pass, "failureMode FailOpen")
}

func TestP6Valkey(t *testing.T) {
	want(t, one(t, "P6", fx{helm: vals(map[string]any{})}), NA, "limits.enabled is false")
	want(t, one(t, "P6", fx{helm: vals(map[string]any{"limits": map[string]any{"enabled": true}})}), Warn, "chart's Valkey")
	want(t, one(t, "P6", fx{helm: vals(map[string]any{"limits": map[string]any{"enabled": true, "valkey": map[string]any{"external": map[string]any{"address": "valkey:6379"}}}})}), Pass, "external Valkey valkey:6379")
}

func TestP7Protection(t *testing.T) {
	srv := &ambitv1alpha1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: ns}}
	want(t, one(t, "P7", fx{helm: vals(map[string]any{})}), NA, "no MCP Server")
	want(t, one(t, "P7", fx{helm: vals(map[string]any{}), ctrlObjs: []runtime.Object{srv}}), Fail, "protection.enabled is false")
	want(t, one(t, "P7", fx{helm: vals(map[string]any{"protection": map[string]any{"enabled": true}}), ctrlObjs: []runtime.Object{srv}}), Pass, "protection is on")
}

func TestP8Alerts(t *testing.T) {
	prom := crd("prometheusrules.monitoring.coreos.com", "monitoring.coreos.com", true, nil, "v1")
	want(t, one(t, "P8", fx{helm: vals(map[string]any{})}), NA, "not installed")
	want(t, one(t, "P8", fx{helm: vals(map[string]any{}), ctrlObjs: []runtime.Object{prom}}), Fail, "alerts.enabled is false")
	want(t, one(t, "P8", fx{helm: vals(map[string]any{"alerts": map[string]any{"enabled": true}}), ctrlObjs: []runtime.Object{prom}}), Pass, "alert rules are on")
}

func TestP9Console(t *testing.T) {
	one1 := map[string]any{"console": map[string]any{"replicas": 1}}
	want(t, one(t, "P9", fx{helm: vals(map[string]any{"console": map[string]any{"replicas": 2}})}), Fail, "console.replicas is 2")
	want(t, one(t, "P9", fx{helm: vals(one1), kubeObjs: []runtime.Object{deploy("ambit-console", nil, 1)}}), Pass, "one console replica")
	want(t, one(t, "P9", fx{helm: vals(one1), kubeObjs: []runtime.Object{deploy("ambit-console", nil, 0)}}), Fail, "0 Ready replicas")
	want(t, one(t, "P9", fx{helm: vals(one1)}), Fail, "no ambit-console Deployment") // missing is a failure, not unknown
}

func TestP10Credentials(t *testing.T) {
	irsa := map[string]any{"gateway": map[string]any{"serviceAccount": map[string]any{"annotations": map[string]any{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/x"}}}}
	want(t, one(t, "P10", fx{helm: vals(map[string]any{})}), NA, "no Model")
	want(t, one(t, "P10", fx{helm: vals(map[string]any{}), ctrlObjs: []runtime.Object{model("m", "OpenAI", "ambit-llm-x", true)}}), Fail, "Secret ambit-llm-x does not exist")
	want(t, one(t, "P10", fx{helm: vals(map[string]any{}), ctrlObjs: []runtime.Object{model("m", "OpenAI", "ambit-llm-x", true), secret("ambit-llm-x", nil)}}), Pass, "1 Models use Secrets")
	want(t, one(t, "P10", fx{helm: vals(map[string]any{}), ctrlObjs: []runtime.Object{model("b", "Bedrock", "", true)}}), Warn, "has no credentialsRef")
	want(t, one(t, "P10", fx{helm: vals(irsa), ctrlObjs: []runtime.Object{model("b", "Bedrock", "", true)}}), Pass, "cloud identity")
}

// The pass line claims only what the counter shows: other traffic can also raise it (ruling S12).
const pinnedP4Line = "audit pipeline: the collector sent records to ClickHouse after the test call"

func TestP4DeepMessageClaim(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"choices":[]}`) }))
	t.Cleanup(gw.Close)
	var n atomic.Int32
	f := fx{helm: vals(map[string]any{"console": map[string]any{"url": gw.URL}, "llm": map[string]any{"host": "127.0.0.1"},
		"evidence": map[string]any{"enabled": true}}),
		kubeObjs: []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"}),
			readyPod("ch", map[string]string{"app.kubernetes.io/name": "ambit-clickhouse"})},
		ctrlObjs: []runtime.Object{model("local-test", "OpenAI", "ambit-llm-local", true)}, deep: true, token: "t", deepWait: time.Second,
		forward: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "otelcol_exporter_sent_log_records_total{exporter=\"clickhouse\"} %d\n", n.Add(1))
		}}
	if r := one(t, "P4", f); r.Status != Pass || r.Message != pinnedP4Line {
		t.Fatalf("P4 = %s %q; want pass %q", r.Status, r.Message, pinnedP4Line)
	}
}

// #134: a provider's 429 or 5xx on the first Ready Model moves the test call to the next one (at most 3, in name
// order); a 401 or 403 from Ambit itself is a finding and is not retried.
func TestP4DeepTriesNextModel(t *testing.T) {
	// status answers per model id (the request body's "model").
	run := func(t *testing.T, status map[string]int, models ...runtime.Object) (Result, []string) {
		var asked []string
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body.Model)
			w.WriteHeader(status[body.Model])
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		}))
		t.Cleanup(gw.Close)
		var n atomic.Int32
		f := fx{helm: vals(map[string]any{"console": map[string]any{"url": gw.URL}, "llm": map[string]any{"host": "127.0.0.1"},
			"evidence": map[string]any{"enabled": true}}),
			kubeObjs: []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"}),
				readyPod("ch", map[string]string{"app.kubernetes.io/name": "ambit-clickhouse"})},
			ctrlObjs: models, deep: true, token: "t", deepWait: 200 * time.Millisecond,
			forward: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "otelcol_exporter_sent_log_records_total{exporter=\"clickhouse\"} %d\n", n.Add(1))
			}}
		return one(t, "P4", f), asked
	}
	four := []runtime.Object{model("a", "OpenAI", "k", true), model("b", "OpenAI", "k", true), model("c", "OpenAI", "k", true), model("d", "OpenAI", "k", true)}

	r, asked := run(t, map[string]int{"a-id": 429, "b-id": 200}, four...)
	want(t, r, Pass, pinnedP4Line)
	if !strings.Contains(r.Message, "models tried: a-id: HTTP 429, b-id answered") || strings.Join(asked, ",") != "a-id,b-id" {
		t.Errorf("message %q, asked %v", r.Message, asked)
	}

	r, asked = run(t, map[string]int{"a-id": 502, "b-id": 503, "c-id": 429, "d-id": 200}, four...)
	want(t, r, Unknown, "every Ready Model tried answered 429 or 5xx")
	if !strings.Contains(r.Message, "a-id: HTTP 502, b-id: HTTP 503, c-id: HTTP 429") || len(asked) != 3 {
		t.Errorf("message %q, asked %v (want 3 models, d-id not asked)", r.Message, asked)
	}

	r, asked = run(t, map[string]int{"a-id": 403, "b-id": 200}, four...)
	want(t, r, Fail, "test call for the audit check failed: HTTP 403")
	if len(asked) != 1 {
		t.Errorf("a 403 was retried: asked %v", asked)
	}

	// A model that is not Ready is skipped; the first Ready one answers and the pass line is unchanged.
	r, asked = run(t, map[string]int{"b-id": 200}, model("a", "OpenAI", "k", false), model("b", "OpenAI", "k", true))
	if r.Status != Pass || r.Message != pinnedP4Line || strings.Join(asked, ",") != "b-id" {
		t.Errorf("%s %q asked %v", r.Status, r.Message, asked)
	}
}
