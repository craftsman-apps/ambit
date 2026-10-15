package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// accessMode is the ConfigMap the controller publishes.
func accessMode(mode string, since time.Time) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ambit-access-mode", Namespace: ns},
		Data: map[string]string{"mode": mode, "modeSince": since.UTC().Format(time.RFC3339), "rulesVersion": "3f9a1c2b7d10"}}
}

func values(mode string) *fakeHelm {
	if mode == "" {
		return vals(map[string]any{})
	}
	return vals(map[string]any{"access": map[string]any{"mode": mode}})
}

func TestP11Mode(t *testing.T) {
	// enforce, set or by default: AccessRules decide.
	want(t, one(t, "P11", fx{helm: values("")}), Pass, "access.mode is enforce")
	want(t, one(t, "P11", fx{helm: values("enforce"), ctrlObjs: []runtime.Object{accessMode("enforce", now.Add(-72*time.Hour))}}), Pass, "access.mode is enforce")

	// audit: a warning with the days since the controller published the mode, and the next step.
	r := one(t, "P11", fx{helm: values("audit"), ctrlObjs: []runtime.Object{accessMode("audit", now.Add(-3*24*time.Hour-time.Hour))}})
	want(t, r, Warn, "access.mode is audit for 3 days (since 2026-10-11T11:00:00Z): AccessRules are recorded, not enforced")
	if !strings.Contains(r.Next, "access.mode: enforce") || !strings.Contains(r.Message, "records not checked; --deep checks them") {
		t.Errorf("%+v", r)
	}
	want(t, one(t, "P11", fx{helm: values("audit"), ctrlObjs: []runtime.Object{accessMode("audit", now.Add(-time.Hour))}}), Warn, "audit for 0 days")

	// The published mode is the truth; the values only say what the next switch is. Before the controller has
	// published anything, the values give the mode.
	want(t, one(t, "P11", fx{helm: values("enforce"), ctrlObjs: []runtime.Object{accessMode("audit", now.Add(-24*time.Hour))}}), Warn, "audit for 1 days")
	want(t, one(t, "P11", fx{helm: values("audit")}), Warn, "access.mode is audit (the controller has not published ambit-access-mode yet)")

	// Without permission it says which.
	want(t, one(t, "P11", fx{helm: values("audit"), ctrlDenied: true}), Unknown, "cannot get configmaps")
}

// --deep: delivery only. The same mechanism as P4 --deep: one test call, then the collector's counter must rise.
func TestP11Deep(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	t.Cleanup(gw.Close)
	v := vals(map[string]any{"console": map[string]any{"url": gw.URL}, "llm": map[string]any{"host": "127.0.0.1"},
		"access": map[string]any{"mode": "audit"}, "evidence": map[string]any{"enabled": true}})
	collector := []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"}),
		readyPod("ch", map[string]string{"app.kubernetes.io/name": "ambit-clickhouse"})}
	objs := []runtime.Object{model("local-test", "OpenAI", "ambit-llm-local", true), accessMode("audit", now.Add(-2*24*time.Hour))}
	rising := func() http.HandlerFunc {
		var n atomic.Int32
		return func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "otelcol_exporter_sent_log_records_total{exporter=\"clickhouse\"} %d\n", n.Add(1))
		}
	}
	flat := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `otelcol_exporter_sent_log_records_total{exporter="clickhouse"} 41`)
	}
	base := fx{helm: v, kubeObjs: collector, ctrlObjs: objs, deep: true, token: "t", deepWait: 200 * time.Millisecond}
	with := func(f func(*fx)) fx { c := base; f(&c); return c }

	r := one(t, "P11", with(func(f *fx) { f.forward = rising() }))
	want(t, r, Warn, "access.mode is audit for 2 days") // records arrive: still a warning, audit-only is on
	if !strings.Contains(r.Message, "the collector sent records to ClickHouse after the test call") {
		t.Errorf("%q", r.Message)
	}
	r = one(t, "P11", with(func(f *fx) { f.forward = flat }))
	want(t, r, Fail, "records do not reach the audit store")
	if !strings.Contains(r.Message, "sent no record to ClickHouse within 200ms of a test call") {
		t.Errorf("%q", r.Message)
	}
	want(t, one(t, "P11", with(func(f *fx) { f.token = "" })), Unknown, "set AMBIT_TOKEN")
	want(t, one(t, "P11", with(func(f *fx) { f.forward = rising(); f.helm = values("audit"); f.deep = false })), Warn, "records not checked")
	// Enforce mode has nothing to deliver: no test call, no failure.
	want(t, one(t, "P11", with(func(f *fx) {
		f.ctrlObjs = []runtime.Object{accessMode("enforce", now.Add(-72*time.Hour))}
		f.helm = values("enforce")
	})), Pass, "access.mode is enforce")
}

// When P4 --deep and P11 --deep both run, the test call is made once and both read its result.
func TestP11ReusesP4DeepResult(t *testing.T) {
	var calls atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, `{"choices":[]}`) }))
	t.Cleanup(gw.Close)
	var n atomic.Int32
	f := fx{helm: vals(map[string]any{"console": map[string]any{"url": gw.URL}, "llm": map[string]any{"host": "127.0.0.1"},
		"access": map[string]any{"mode": "audit"}, "evidence": map[string]any{"enabled": true}}),
		kubeObjs: []runtime.Object{readyPod("c", map[string]string{"app.kubernetes.io/name": "ambit-otel-collector"}),
			readyPod("ch", map[string]string{"app.kubernetes.io/name": "ambit-clickhouse"})},
		ctrlObjs: []runtime.Object{model("local-test", "OpenAI", "ambit-llm-local", true), accessMode("audit", now.Add(-time.Hour))},
		deep:     true, token: "t", deepWait: time.Second,
		forward: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "otelcol_exporter_sent_log_records_total{exporter=\"clickhouse\"} %d\n", n.Add(1))
		}}
	got := map[string]Result{}
	for _, r := range Run(context.Background(), f.env(t), map[string]bool{GroupProduction: true}) {
		got[r.ID] = r
	}
	if got["P4"].Status != Pass || got["P11"].Status != Warn || calls.Load() != 1 {
		t.Fatalf("P4 %+v, P11 %+v, %d test calls (want 1)", got["P4"], got["P11"], calls.Load())
	}
}

// The mark-missing line (spec 6): a line under P11, not a check of its own. It counts the audit-mode call records of
// the last 24 h with a missing or failed mark, with the query the console uses.
func TestMarkMissingLine(t *testing.T) {
	var ran []string
	exec := func(out string, err error) func(string, []string) (string, error) {
		return func(sel string, cmd []string) (string, error) {
			ran = append(ran, sel+" "+strings.Join(cmd, " "))
			return out, err
		}
	}
	audit := []runtime.Object{accessMode("audit", now.Add(-48*time.Hour))}
	line := func(r Result) Line {
		t.Helper()
		if len(r.Lines) != 1 {
			t.Fatalf("lines = %+v", r.Lines)
		}
		return r.Lines[0]
	}

	r := one(t, "P11", fx{helm: values("audit"), ctrlObjs: audit, exec: exec("0\n", nil)})
	if l := line(r); l.Status != Pass || l.Message != "audit-mode call records with a missing or failed mark (last 24 h): 0" {
		t.Errorf("%+v", l)
	}
	r2 := one(t, "P11", fx{helm: values("audit"), ctrlObjs: audit, exec: exec("17\n", nil)})
	if l := line(r2); l.Status != Warn || !strings.Contains(l.Message, "(last 24 h): 17") || !strings.Contains(l.Next, "a rule expression failed or the audit fields were not active yet") {
		t.Errorf("%+v", l)
	}
	if r.Status != r2.Status { // P11 states delivery and the mode; it never judges marks
		t.Errorf("P11 changed with the marks: %s, then %s", r.Status, r2.Status)
	}
	// The command asks the chart's ClickHouse pod, with the pod's own credentials; no password reaches the CLI.
	last := ran[len(ran)-1]
	if !strings.HasPrefix(last, "app.kubernetes.io/name=ambit-clickhouse sh -c ") || !strings.Contains(last, "clickhouse-client") ||
		!strings.Contains(last, `"$CLICKHOUSE_PASSWORD"`) || !strings.Contains(last, "'mark-missing'") {
		t.Errorf("exec: %s", last)
	}

	// ClickHouse out of reach: unknown, with the reason.
	for _, c := range []struct {
		name string
		f    fx
		has  string
	}{
		{"exec fails", fx{helm: values("audit"), ctrlObjs: audit, exec: exec("", errors.New("container not found"))}, "container not found"},
		{"no permission", fx{helm: values("audit"), ctrlObjs: audit, exec: exec("", forbidden)}, "cannot create pods/exec in ambit-system"},
		{"odd answer", fx{helm: values("audit"), ctrlObjs: audit, exec: exec("Code: 60. DB::Exception", nil)}, "Code: 60"},
		{"external ClickHouse", fx{helm: vals(map[string]any{"access": map[string]any{"mode": "audit"}, "evidence": map[string]any{"clickhouse": map[string]any{"external": map[string]any{"host": "ch.example.com"}}}}),
			ctrlObjs: audit, exec: exec("0", nil)}, "external ClickHouse ch.example.com is not reached by the CLI"},
	} {
		if l := line(one(t, "P11", c.f)); l.Status != Unknown || !strings.Contains(l.Message, c.has) {
			t.Errorf("%s: %+v", c.name, l)
		}
	}

	// Printed when audit mode is on or was on in the last 24 h; not otherwise, and then nothing is run.
	ran = nil
	r = one(t, "P11", fx{helm: values("enforce"), ctrlObjs: []runtime.Object{accessMode("enforce", now.Add(-30*time.Hour))}, exec: exec("9", nil)})
	if len(r.Lines) != 0 || len(ran) != 0 || r.Status != Pass {
		t.Errorf("off for 30 h: %+v ran %v", r, ran)
	}
	r = one(t, "P11", fx{helm: values("enforce"), ctrlObjs: []runtime.Object{accessMode("enforce", now.Add(-2*time.Hour))}, exec: exec("0", nil)})
	if len(r.Lines) != 1 || r.Lines[0].Status != Pass || r.Status != Pass {
		t.Errorf("switched back 2 h ago: %+v", r)
	}
	// Marks still missing after the switch back are a warning, which shows in the check's own status.
	r = one(t, "P11", fx{helm: values("enforce"), ctrlObjs: []runtime.Object{accessMode("enforce", now.Add(-2*time.Hour))}, exec: exec("2", nil)})
	if r.Status != Warn || len(r.Lines) != 1 || r.Lines[0].Status != Warn {
		t.Errorf("switched back 2 h ago, marks missing: %+v", r)
	}
	// No exec at all (an environment without it) is unknown, not a crash.
	if l := line(one(t, "P11", fx{helm: values("audit"), ctrlObjs: audit})); l.Status != Unknown {
		t.Errorf("%+v", l)
	}
}

func TestLinesArePrintedAndCountedForExit(t *testing.T) {
	var b bytes.Buffer
	p := out.New(out.Options{Out: &b, Err: &b}, &out.Redactor{})
	rs := []Result{{ID: "P11", Status: Warn, Message: "access.mode is audit for 3 days", Next: "set access.mode: enforce", Source: "values, ambit-access-mode",
		Lines: []Line{{Status: Warn, Message: "audit-mode call records with a missing or failed mark (last 24 h): 17", Next: "run it again"}}}}
	Print(p, rs)
	want := "! P11 access.mode is audit for 3 days [values, ambit-access-mode]\n      next: set access.mode: enforce\n" +
		"      ! audit-mode call records with a missing or failed mark (last 24 h): 17\n          next: run it again\n" +
		"0 passed, 1 warning, 0 failed\n" // the line is not a check: the count stays by check
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	rs[0].Lines = []Line{{Status: Unknown, Message: "x"}}
	if ExitCode(rs, false) != out.ExitOK || ExitCode(rs, true) != out.ExitFailed {
		t.Errorf("exit with an unknown line: %d, %d", ExitCode(rs, false), ExitCode(rs, true))
	}
}
