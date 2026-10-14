package checks

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// Env is what the checks read. Checks only read; the two active probes (Forward for the health probes, and
// --deep calls) create and change no object (spec 3, principle 5).
type Env struct {
	Kube      kubernetes.Interface
	Ctrl      client.Client
	Helm      helmx.Client
	Namespace string
	// ValuesFile is `check -f values.yaml` for the pre group; nil when not given.
	ValuesFile map[string]any
	CRDBundle  []byte
	// Forward opens a port-forward to a pod that matches selector and returns the local port (kube.Forward).
	Forward func(ctx context.Context, selector string, port int) (int, func(), error)
	Deep    bool
	Token   string // AMBIT_TOKEN, for --deep (ruling S4)
	HTTP    *http.Client
	Now     func() time.Time
	// DeepWait is how long P4 --deep waits for the collector to send the test call's record (0: 60 s).
	DeepWait time.Duration

	loaded            bool
	userVals, allVals map[string]any
	valsErr           error
}

// Check is one entry of the catalogue.
type Check struct {
	ID, Group string
	Run       func(ctx context.Context, e *Env) Result
}

// Catalogue is every check, in output order. IDs are stable: Release 2's install runs the same catalogue.
var Catalogue = []Check{
	{"R1", GroupPre, r1KubeVersion}, {"R2", GroupPre, r2Permissions}, {"R3", GroupPre, r3Installed},
	{"R4", GroupPre, r4StorageClass}, {"R5", GroupPre, r5TLSSecret}, {"R6", GroupPre, r6ThirdPartyCRDs},
	{"H1", GroupHealth, h1Releases}, {"H2", GroupHealth, h2CRDs}, {"H3", GroupHealth, h3Pods},
	{"H4", GroupHealth, h4Gateway}, {"H5", GroupHealth, h5Dex}, {"H6", GroupHealth, h6ClickHouse},
	{"H7", GroupHealth, h7Objects}, {"H8", GroupHealth, h8Models},
	{"P1", GroupProduction, p1Gateway}, {"P2", GroupProduction, p2Bootstrap}, {"P3", GroupProduction, p3TLS},
	{"P4", GroupProduction, p4Evidence}, {"P5", GroupProduction, p5Limits}, {"P6", GroupProduction, p6Valkey},
	{"P7", GroupProduction, p7Protection}, {"P8", GroupProduction, p8Alerts}, {"P9", GroupProduction, p9Console},
	{"P10", GroupProduction, p10Credentials},
}

// Run runs the checks of the given groups. A check that panics becomes an unknown result, so `ambit check`
// never crashes on an unexpected cluster (spec 9.2a, restricted permissions).
func Run(ctx context.Context, e *Env, groups map[string]bool) []Result {
	var rs []Result
	for _, c := range Catalogue {
		if !groups[c.Group] {
			continue
		}
		rs = append(rs, runOne(ctx, e, c))
	}
	return rs
}

func runOne(ctx context.Context, e *Env, c Check) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			r = Result{Status: Unknown, Message: fmt.Sprintf("the check stopped with an internal error: %v", p),
				Next: "report it at https://github.com/craftsman-apps/ambit/issues"}
		}
		r.ID, r.Group = c.ID, c.Group
	}()
	return c.Run(ctx, e)
}

// Summary is the last line, e.g. "8 passed, 1 warning, 1 failed".
func Summary(rs []Result) string {
	n := map[Status]int{}
	for _, r := range rs {
		n[r.Status]++
	}
	parts := []string{fmt.Sprintf("%d passed", n[Pass]), plural(n[Warn], "warning", "warnings"), fmt.Sprintf("%d failed", n[Fail])}
	if n[Unknown] > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", n[Unknown]))
	}
	if n[NA] > 0 {
		parts = append(parts, fmt.Sprintf("%d n/a", n[NA]))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Print writes one line per result and the summary.
func Print(p *out.Printer, rs []Result) {
	for _, r := range rs {
		mark := map[Status]string{Pass: p.Mark("ok"), Warn: p.Mark("warn"), Fail: p.Mark("fail"), Unknown: "?", NA: "-"}[r.Status]
		line := fmt.Sprintf("%s %-3s %s", mark, r.ID, r.Message)
		if r.Source != "" {
			line += " [" + r.Source + "]"
		}
		p.Line("%s", line)
		if r.Next != "" && r.Status != Pass && r.Status != NA {
			p.Line("      next: %s", r.Next)
		}
	}
	p.Line("%s", Summary(rs))
}

// ExitCode is 1 when a check failed, or (with failOnUnknown) could not look; else 0 (spec 4, 4.3).
func ExitCode(rs []Result, failOnUnknown bool) int {
	for _, r := range rs {
		if r.Status == Fail || (failOnUnknown && r.Status == Unknown) {
			return out.ExitFailed
		}
	}
	return out.ExitOK
}

// --- helpers shared by the checks ---

func res(s Status, source, msg string, a ...any) Result {
	return Result{Status: s, Source: source, Message: fmt.Sprintf(msg, a...)}
}

func (r Result) next(n string) Result { r.Next = n; return r }

// denied reports whether err means "no permission" (also Helm's and the port-forward's wrapped errors).
func denied(err error) bool {
	return err != nil && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || strings.Contains(err.Error(), "forbidden"))
}

// unknownFor turns an error into an unknown result that names the missing permission or the failure.
func unknownFor(err error, verb, resource, ns, source string) Result {
	where := ""
	if ns != "" {
		where = " in " + ns
	}
	if denied(err) {
		return res(Unknown, source, "cannot %s %s%s", verb, resource, where).next("ask for read access; the CLI reference lists the permissions")
	}
	return res(Unknown, source, "could not %s %s%s: %v", verb, resource, where, err)
}

// values returns the user-supplied and the computed values of the ambit release (helm get values, with and
// without --all). Helm stores them in Secrets, so reading needs `get secrets` in the namespace.
func (e *Env) values() (user, all map[string]any, err error) {
	if !e.loaded {
		e.loaded = true
		if e.Helm == nil {
			e.valsErr = fmt.Errorf("no Helm client")
		} else if e.userVals, e.valsErr = e.Helm.Values("ambit", false); e.valsErr == nil {
			e.allVals, e.valsErr = e.Helm.Values("ambit", true)
		}
	}
	return e.userVals, e.allVals, e.valsErr
}

func (e *Env) valuesUnknown(source string) Result {
	r := unknownFor(e.valsErr, "get", "secrets", e.Namespace, source)
	if denied(e.valsErr) {
		r.Message += " (Helm stores the release values there)"
	}
	return r
}

func get(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func str(m map[string]any, path string) string   { v, _ := get(m, path); s, _ := v.(string); return s }
func boolean(m map[string]any, path string) bool { v, _ := get(m, path); b, _ := v.(bool); return b }
func number(m map[string]any, path string) int {
	v, _ := get(m, path)
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
