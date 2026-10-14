package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/kindx"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

type fakeKind struct {
	exists   bool
	created  []byte
	deleted  bool
	loaded   []string
	onCreate func() // e.g. simulate kind making kind-<name> current
	// createErr makes Create fail the way kind does: the cluster does not exist afterwards.
	createErr error
	// existsErr makes Exists fail like kind's docker ps does when Docker is down; existsCalls counts the calls.
	existsErr   error
	existsCalls int
	loadErr     error
	cpErr       error // ControlPlane
}

func (k *fakeKind) Exists(string) (bool, error) {
	k.existsCalls++
	return k.exists, k.existsErr
}
func (k *fakeKind) Create(name string, cfg []byte, _ string) error {
	if k.createErr != nil {
		return k.createErr
	}
	k.created, k.exists = cfg, true
	if k.onCreate != nil {
		k.onCreate()
	}
	return nil
}
func (k *fakeKind) Delete(string, string) error           { k.deleted, k.exists = true, false; return nil }
func (k *fakeKind) ControlPlane(n string) (string, error) { return n + "-control-plane", k.cpErr }
func (k *fakeKind) LoadArchive(_, path string) error {
	k.loaded = append(k.loaded, path)
	return k.loadErr
}

type fakeOps struct {
	name     string
	labels   bool
	calls    []string
	secret   bool
	applied  [][]byte
	password string
	// secretErr is returned by ApplySecret, e.g. a library error that dumps the request with the key in it.
	secretErr error
	// errs makes a method fail by its name; applyErr fails Apply for one manifest; onReady runs in WaitModelReady.
	errs     map[string]error
	applyErr func(manifest []byte) error
	onReady  func()
}

func (o *fakeOps) NodeLabels(context.Context) (map[string]map[string]string, error) {
	if !o.labels {
		return map[string]map[string]string{o.name + "-control-plane": {}}, nil
	}
	return map[string]map[string]string{o.name + "-control-plane": {kindx.Label: kindx.LabelValue}}, nil
}
func (o *fakeOps) ApplyCRDs(context.Context, []byte) error {
	o.calls = append(o.calls, "crds")
	return o.errs["ApplyCRDs"]
}
func (o *fakeOps) InstallCharts(_ context.Context, c Charts, vals map[string]any, _ time.Duration, progress func(string)) error {
	o.calls = append(o.calls, "charts "+c.Ref("ambit")+" "+vals["console"].(map[string]any)["url"].(string))
	progress("waiting for ambit-console-1")
	return nil
}
func (o *fakeOps) SecretExists(context.Context, string) (bool, error) {
	return o.secret, o.errs["SecretExists"]
}
func (o *fakeOps) ApplySecret(_ context.Context, name string, data map[string]string) error {
	o.calls = append(o.calls, "secret "+name)
	return o.secretErr
}
func (o *fakeOps) Apply(_ context.Context, m []byte) error {
	o.applied = append(o.applied, m)
	if o.applyErr != nil {
		return o.applyErr(m)
	}
	return nil
}
func (o *fakeOps) WaitModelReady(_ context.Context, name string, _ time.Duration) error {
	o.calls = append(o.calls, "ready "+name)
	if o.onReady != nil {
		o.onReady()
	}
	return nil
}
func (o *fakeOps) AdminPassword(context.Context) (string, error) {
	return o.password, o.errs["AdminPassword"]
}

// fakeGateway answers Dex and the chat API like a local install; refuse lists models whose calls get HTTP 429.
func fakeGateway(t *testing.T, refuse ...string) int {
	status := map[string]int{}
	for _, m := range refuse {
		status[m] = 429
	}
	return fakeGatewayStatus(t, status)
}

// fakeGatewayStatus is fakeGateway with a chosen HTTP status per model; a model not in the map answers 200.
func fakeGatewayStatus(t *testing.T, status map[string]int) int {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dex/token":
			w.Write([]byte(`{"access_token":"eyJhbGciOiJSUzI1NiJ9.token-for-test"}`))
		case "/v1/chat/completions":
			var b struct{ Model string }
			json.NewDecoder(r.Body).Decode(&b)
			if st := status[b.Model]; st != 0 {
				w.WriteHeader(st)
				w.Write([]byte(`{"error":{"message":"refused for the test"}}`))
				return
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"You said: Say hello in five words."}}]}`))
		case "/models":
			w.Write([]byte(`{"data":[{"id":"google/gemma-4-31b-it:free","context_length":262144},{"id":"nvidia/nemotron-3-super-120b-a12b:free","context_length":262144}]}`))
		}
	}))
	t.Cleanup(s.Close)
	return s.Listener.Addr().(*net.TCPAddr).Port
}

type rig struct {
	env      *Env
	kind     *fakeKind
	ops      *fakeOps
	contexts []string
	out      *bytes.Buffer
	p        *out.Printer
	opts     UpOptions
}

func newRig(t *testing.T, tty bool, refuse ...string) *rig {
	return newRigOn(t, tty, fakeGateway(t, refuse...))
}

func newRigOn(t *testing.T, tty bool, port int) *rig {
	r := &rig{kind: &fakeKind{}, ops: &fakeOps{name: "ambit", labels: true, password: "Local-Admin-Password-1"}, out: &bytes.Buffer{}}
	kubeconfig := filepath.Join(t.TempDir(), "config")
	r.env = &Env{
		Store:       Store{Dir: t.TempDir()},
		Kind:        r.kind,
		ContainerID: func(string) (string, error) { return cid, nil },
		Docker:      func() DockerInfo { return ParseDockerInfo([]byte(desktopInfo)) },
		PortFree:    func(int) bool { return true },
		OpenOps: func(_, kctx string) (Ops, error) {
			r.contexts = append(r.contexts, kctx)
			return r.ops, nil
		},
		HTTP:         HTTPClient,
		ModelsClient: http.DefaultClient,
		ModelsURL:    "http://127.0.0.1:" + itoa(port) + "/models",
		CRDBundle:    []byte("kind: CustomResourceDefinition"),
		Kubeconfig:   kubeconfig,
		ModelWait:    time.Second, CallLimit: 2 * time.Second, CallEvery: 10 * time.Millisecond,
	}
	r.p = out.New(out.Options{Out: r.out, Err: r.out, TTY: tty}, &out.Redactor{})
	r.opts = UpOptions{Name: "ambit", HTTPPort: port, HTTPSPort: 8443, Timeout: time.Minute,
		Charts: Charts{Repo: "oci://ghcr.io/craftsman-apps/charts", Version: "0.2.0"}}
	return r
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestUpFreshWithTestModel(t *testing.T) {
	r := newRig(t, false)
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	if !bytes.Contains(r.kind.created, []byte(kindx.Label)) {
		t.Fatal("the kind config has no ownership label")
	}
	st, _ := r.env.Store.Read("ambit")
	if st == nil || st.Status != "ready" || st.ContainerID != cid {
		t.Fatalf("state = %+v", st)
	}
	o := r.out.String()
	for _, want := range []string{"✓ Preflight", "✓ Cluster ambit (created)", "✓ CRDs", "  waiting for ambit-console-1",
		"✓ Charts ambit-crds and ambit", "✓ Model local-test (test model)", "Using a test model that repeats your message",
		"✓ Rule everyone-local", "✓ Test call", "local/echo answered: You said: Say hello in five words.",
		"Console:  http://console.localtest.me:", "Password: run `ambit local password`"} {
		if !strings.Contains(o, want) {
			t.Errorf("output has no %q:\n%s", want, o)
		}
	}
	if strings.Contains(o, "Local-Admin-Password-1") || strings.Contains(o, "token-for-test") {
		t.Fatalf("a secret reached non-TTY output:\n%s", o)
	}
	if got := r.ops.calls[1]; got != "charts oci://ghcr.io/craftsman-apps/charts/ambit http://console.localtest.me:"+itoa(r.opts.HTTPPort) {
		t.Fatalf("charts call = %q", got)
	}
}

func TestUpUsesOnlyKindContext(t *testing.T) {
	r := newRig(t, false)
	r.opts.Name = "ambit-cli-test"
	r.ops.name = "ambit-cli-test"
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	if len(r.contexts) == 0 {
		t.Fatal("no cluster access recorded")
	}
	for _, c := range r.contexts {
		if c != "kind-ambit-cli-test" {
			t.Fatalf("Up used context %q; it must use only kind-ambit-cli-test", c)
		}
	}
}

func TestUpPrintsPasswordOnlyOnTTY(t *testing.T) {
	r := newRig(t, true)
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "Password: Local-Admin-Password-1") {
		t.Fatalf("TTY output has no password:\n%s", r.out)
	}
	r = newRig(t, true)
	r.opts.NoPassword = true
	Up(context.Background(), r.p, r.env, r.opts)
	if strings.Contains(r.out.String(), "Local-Admin-Password-1") {
		t.Fatal("--no-password printed the password")
	}
}

func TestUpRefusesForeignCluster(t *testing.T) {
	r := newRig(t, false)
	r.kind.exists = true // a cluster "ambit" exists, with no state file: not ours
	err := Up(context.Background(), r.p, r.env, r.opts)
	var e *out.Error
	if !errors.As(err, &e) || !strings.Contains(e.What, "was not created by ambit local up") {
		t.Fatalf("err = %v", err)
	}
	if r.kind.created != nil || r.kind.deleted || len(r.ops.calls) > 0 {
		t.Fatal("Up touched a cluster it does not own")
	}
}

func TestUpRepairsOwnedCluster(t *testing.T) {
	r := newRig(t, false)
	r.kind.exists = true
	r.env.Store.Write(State{Name: "ambit", Status: "creating", ContainerID: cid})
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	if r.kind.created != nil || !strings.Contains(r.out.String(), "✓ Cluster ambit (exists; repairing)") {
		t.Fatalf("repair created a cluster or said so wrongly:\n%s", r.out)
	}
}

func TestUpFailsWhenKindDidNotLabel(t *testing.T) {
	r := newRig(t, false)
	r.ops.labels = false
	err := Up(context.Background(), r.p, r.env, r.opts)
	var e *out.Error
	if !errors.As(err, &e) || e.What != "kind did not mark the cluster" {
		t.Fatalf("err = %v", err)
	}
}

func TestUpOpenRouterTriesNextCandidate(t *testing.T) {
	r := newRig(t, false, "nvidia/nemotron-3-super-120b-a12b:free")
	r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	o := r.out.String()
	if !strings.Contains(o, "nvidia/nemotron-3-super-120b-a12b:free refused the call (HTTP 429") ||
		!strings.Contains(o, "google/gemma-4-31b-it:free answered:") || strings.Contains(o, "0123456789abcdef") {
		t.Fatalf("output:\n%s", o)
	}
	if r.ops.calls[2] != "secret ambit-llm-openrouter" {
		t.Fatalf("calls = %v", r.ops.calls)
	}
}

// Task 9 ruling: a 404 that lasts until the settling window ends is a failed model (a removed provider model and the
// gateway's own settling 404 share that status), so the next candidate is tried.
func TestUpNotFoundUntilTimeoutTriesNextCandidate(t *testing.T) {
	port := fakeGatewayStatus(t, map[string]int{"nvidia/nemotron-3-super-120b-a12b:free": 404})
	r := newRigOn(t, false, port)
	r.env.CallLimit = 700 * time.Millisecond
	r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	o := r.out.String()
	if !strings.Contains(o, "nvidia/nemotron-3-super-120b-a12b:free did not answer") ||
		!strings.Contains(o, "google/gemma-4-31b-it:free answered:") {
		t.Fatalf("output:\n%s", o)
	}
}

// Task 9 ruling: 401 means our own token is wrong; no other model can fix that, so Up stops at once.
func TestUpUnauthorizedStopsAtOnce(t *testing.T) {
	port := fakeGatewayStatus(t, map[string]int{
		"nvidia/nemotron-3-super-120b-a12b:free": 401, "google/gemma-4-31b-it:free": 401})
	r := newRigOn(t, false, port)
	r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
	err := Up(context.Background(), r.p, r.env, r.opts)
	var e *out.Error
	if !errors.As(err, &e) || e.What != "the test call failed" || !strings.Contains(e.Why, "HTTP 401") {
		t.Fatalf("err = %v", err)
	}
	if o := r.out.String(); strings.Contains(o, "trying the next") || strings.Contains(o, "did not answer") {
		t.Fatalf("Up tried another model after a 401:\n%s", o)
	}
	readies := 0
	for _, c := range r.ops.calls {
		if strings.HasPrefix(c, "ready free-model") {
			readies++
		}
	}
	if readies != 1 {
		t.Fatalf("model prepared %d times, want 1: %v", readies, r.ops.calls)
	}
}

// Task 8 ruling: with --model the model list is not fetched and the given model is the only candidate.
func TestUpModelFlagSkipsFetch(t *testing.T) {
	r := newRig(t, false)
	r.env.ModelsURL = "http://127.0.0.1:1/never" // a fetch would fail
	r.opts.Key, r.opts.KeyGiven, r.opts.Model = "sk-or-v1-0123456789abcdef0123456789", true, "meta/own-pick:free"
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out.String(), "meta/own-pick:free answered:") {
		t.Fatalf("output:\n%s", r.out)
	}
}

// Spec 9.2a redaction with a fake OpenRouter key, here with mocks instead of in the cli-local job (ruling S14):
// the key reaches no output, in a step line or in a library error that quotes it raw, JSON-escaped or URL-encoded.
// (--verbose lines go through the same redactor: TestVerboseWriter in internal/cli/out.)
func TestUpFakeKeyNeverPrinted(t *testing.T) {
	const fake = "sk-or-v1-5d41402abc4b2a76b9719d911017c592ae2b7c3e9f1a4d5b"
	r := newRig(t, false)
	r.opts.Key, r.opts.KeyGiven = fake, true
	r.ops.secretErr = fmt.Errorf(`Secret "ambit-llm-openrouter" is invalid: request body {"stringData":{"Authorization":"Bearer %s"}}, form key=%s`,
		fake, url.QueryEscape(fake))
	err := Up(context.Background(), r.p, r.env, r.opts)
	if err == nil {
		t.Fatal("Up succeeded although ApplySecret failed")
	}
	r.p.Fail(&out.Error{What: "local up", Why: err.Error()}) // what cli.run prints for an error that is not an *out.Error
	o := r.out.String()
	if !strings.Contains(o, "[redacted]") {
		t.Fatalf("the error was not printed:\n%s", o)
	}
	for _, form := range []string{fake, url.QueryEscape(fake), fake[len(fake)-24:]} {
		if strings.Contains(o, form) {
			t.Fatalf("output contains %q:\n%s", form, o)
		}
	}
}

// Spec 4.1 step 2: kind fails the create when a port was taken after preflight; the CLI reports a busy port with
// the next command, and removes the state file of the cluster that kind did not keep.
func TestUpPortTakenAfterPreflight(t *testing.T) {
	r := newRig(t, false)
	r.kind.createErr = errors.New(`command "docker run --name ambit-control-plane ..." failed with error: exit status 125: Bind for 127.0.0.1:8443 failed: port is already allocated`)
	err := Up(context.Background(), r.p, r.env, r.opts)
	var e *out.Error
	if !errors.As(err, &e) || e.What != "a port was taken after preflight" || e.Next != "ambit local up --port "+itoa(r.opts.HTTPPort+10000)+" --https-port 18443" {
		t.Fatalf("err = %#v", err)
	}
	if st, _ := r.env.Store.Read("ambit"); st != nil {
		t.Fatalf("state file left after a failed create: %+v", st)
	}
}

// The next step keeps --name: without it the command would point at the default cluster `ambit`.
func TestUpPortTakenKeepsName(t *testing.T) {
	r := newRig(t, false)
	r.opts.Name = "ambit-cli-test"
	r.kind.createErr = errors.New("Bind for 127.0.0.1:8443 failed: port is already allocated")
	err := Up(context.Background(), r.p, r.env, r.opts)
	var e *out.Error
	want := "ambit local up --name ambit-cli-test --port " + itoa(r.opts.HTTPPort+10000) + " --https-port 18443"
	if !errors.As(err, &e) || e.Next != want {
		t.Fatalf("err = %#v, want next %q", err, want)
	}
}

// Every command Up suggests for a custom name carries --name.
func TestUpSuggestionsKeepName(t *testing.T) {
	r := newRig(t, false)
	r.opts.Name = "ambit-cli-test"
	r.ops.name = "ambit-cli-test"
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	if o := r.out.String(); !strings.Contains(o, "ambit local up --name ambit-cli-test --openrouter-key-file <file>") {
		t.Fatalf("test-model hint has no --name:\n%s", o)
	}
	if e := testCallError("ambit-cli-test", errors.New("x")); !strings.Contains(e.Next, "ambit local up --name ambit-cli-test again") ||
		!strings.Contains(e.Next, "ambit local up --name ambit-cli-test --no-key") {
		t.Fatalf("next = %q", e.Next)
	}
	if e := chartsError(r.p, "ambit-cli-test", errors.New("x")); !strings.Contains(e.Next, "ambit local up --name ambit-cli-test again") {
		t.Fatalf("next = %q", e.Next)
	}
}

// Spec 4.1: a repair keeps an existing OpenRouter Secret when no new key is given, and still uses OpenRouter.
func TestUpRepairKeepsOpenRouterSecret(t *testing.T) {
	r := newRig(t, false)
	r.kind.exists, r.ops.secret = true, true
	r.env.Store.Write(State{Name: "ambit", Status: "ready", ContainerID: cid})
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	for _, c := range r.ops.calls {
		if c == "secret ambit-llm-openrouter" {
			t.Fatalf("the existing Secret was replaced: calls %v", r.ops.calls)
		}
	}
	if o := r.out.String(); !strings.Contains(o, ":free answered:") || strings.Contains(o, "test model") {
		t.Fatalf("repair did not keep OpenRouter:\n%s", o)
	}
}

// Spec 4.1 step 6 and ruling S2: the rule is everyone-local, for every signed-in user, with the CLI's label.
func TestUpRuleCarriesLabel(t *testing.T) {
	r := newRig(t, false)
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatalf("Up: %v\n%s", err, r.out)
	}
	for _, m := range r.ops.applied {
		if bytes.Contains(m, []byte("kind: AccessRule")) {
			if !bytes.Contains(m, []byte("name: everyone-local")) || !bytes.Contains(m, []byte("allSignedIn: true")) ||
				!bytes.Contains(m, []byte(kindx.Label+": "+kindx.LabelValue)) {
				t.Fatalf("rule manifest:\n%s", m)
			}
			return
		}
	}
	t.Fatalf("no AccessRule applied (%d manifests)", len(r.ops.applied))
}

func TestUpRestoresCurrentContext(t *testing.T) {
	r := newRig(t, false)
	os.WriteFile(r.env.Kubeconfig, []byte(strings.ReplaceAll(testKubeconfig, "CURRENT", "prod")), 0o600)
	r.kind.onCreate = func() { // kind makes its context current
		os.WriteFile(r.env.Kubeconfig, []byte(strings.ReplaceAll(testKubeconfig, "CURRENT", "kind-ambit")), 0o600)
	}
	if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
		t.Fatal(err)
	}
	if c, _ := CurrentContext(r.env.Kubeconfig); c != "prod" {
		t.Fatalf("current context = %q, want prod", c)
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: CURRENT
clusters:
- {name: prod, cluster: {server: "https://prod.example.com"}}
- {name: kind-ambit, cluster: {server: "https://127.0.0.1:41234"}}
contexts:
- {name: prod, context: {cluster: prod, user: u}}
- {name: kind-ambit, context: {cluster: kind-ambit, user: u}}
users:
- {name: u, user: {token: x}}
`

func TestRestoreCurrentContext(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none")
	if c, err := CurrentContext(missing); c != "" || err != nil {
		t.Fatalf("no kubeconfig file: %q, %v", c, err)
	}
	if err := RestoreContext(missing, ""); err != nil {
		t.Fatalf("nothing to restore: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte(strings.ReplaceAll(testKubeconfig, "CURRENT", "kind-ambit")), 0o600)
	if err := RestoreContext(path, "prod"); err != nil {
		t.Fatal(err)
	}
	if c, _ := CurrentContext(path); c != "prod" {
		t.Fatalf("current = %q", c)
	}
}

func TestResolveKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	for _, c := range []struct {
		name  string
		src   KeySource
		key   string
		given bool
		code  int
	}{
		{"BOM and CRLF", KeySource{File: write("win.txt", "\xef\xbb\xbfsk-or-v1-abc\r\n")}, "sk-or-v1-abc", true, 0},
		{"trailing newline", KeySource{File: write("lf.txt", "sk-or-v1-abc\n")}, "sk-or-v1-abc", true, 0},
		{"file wins over env", KeySource{File: write("f.txt", "from-file"), Env: "from-env"}, "from-file", true, 0},
		{"env", KeySource{Env: " from-env "}, "from-env", true, 0},
		{"no key wins", KeySource{NoKey: true, Env: "from-env"}, "", false, 0},
		{"nothing", KeySource{}, "", false, 0},
		{"empty file", KeySource{File: write("empty.txt", "\r\n")}, "", false, out.ExitUsage},
		{"two lines", KeySource{File: write("two.txt", "a\nb\n")}, "", false, out.ExitUsage},
		{"missing file", KeySource{File: filepath.Join(dir, "missing")}, "", false, out.ExitUsage},
	} {
		k, given, err := ReadKey(c.src)
		var e *out.Error
		code := 0
		if errors.As(err, &e) {
			code = e.Code
		}
		if k != c.key || given != c.given || code != c.code {
			t.Errorf("%s: = %q %v %v", c.name, k, given, err)
		}
	}
}

func TestCurlExample(t *testing.T) {
	ps := curlExample("windows", 8080, "ambit", "local/echo")
	if !strings.Contains(ps, "Invoke-RestMethod") || strings.Contains(ps, "sed") || strings.Contains(ps, "curl") {
		t.Fatalf("PowerShell example:\n%s", ps)
	}
	sh := curlExample("linux", 18080, "ambit-cli-test", "local/echo")
	if !strings.Contains(sh, "http://llm.localtest.me:18080/v1/chat/completions") || !strings.Contains(sh, "ambit local password --name ambit-cli-test") {
		t.Fatalf("sh example:\n%s", sh)
	}
}

func TestDown(t *testing.T) {
	r := newRig(t, false)
	if err := Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, true); err != nil {
		t.Fatalf("absent: %v", err)
	}
	r.kind.exists = true // not ours: no state file
	err := Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, true)
	var e *out.Error
	if !errors.As(err, &e) || r.kind.deleted {
		t.Fatalf("foreign: err %v, deleted %v", err, r.kind.deleted)
	}
	r.env.Store.Write(State{Name: "ambit", Status: "ready", ContainerID: cid})
	if err := Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, false); err == nil {
		t.Fatal("no terminal and no --yes must refuse")
	}
	if err := Down(context.Background(), r.p, r.env, "ambit", strings.NewReader("y\n"), true, false); err != nil || !r.kind.deleted {
		t.Fatalf("owned: err %v, deleted %v", err, r.kind.deleted)
	}
	if st, _ := r.env.Store.Read("ambit"); st != nil {
		t.Fatal("state file left after delete")
	}
}

func TestPassword(t *testing.T) {
	r := newRig(t, false)
	r.kind.exists = true
	r.env.Store.Write(State{Name: "ambit", Status: "ready", ContainerID: cid})
	if err := Password(context.Background(), r.p, r.env, "ambit"); err != nil || r.out.String() != "Local-Admin-Password-1\n" {
		t.Fatalf("err %v, output %q", err, r.out)
	}
	r.env.Store.Remove("ambit")
	if err := Password(context.Background(), r.p, r.env, "ambit"); err == nil {
		t.Fatal("printed the password of a cluster without both proofs")
	}
}

func TestMergeValues(t *testing.T) {
	f := filepath.Join(t.TempDir(), "dev.yaml")
	os.WriteFile(f, []byte("console:\n  image: {repository: ambit-console, tag: dev, pullPolicy: Never}\n"), 0o600)
	v, err := MergeValues(map[string]any{"console": map[string]any{"url": "http://x"}}, []string{f})
	c := v["console"].(map[string]any)
	if err != nil || c["url"] != "http://x" || c["image"].(map[string]any)["tag"] != "dev" {
		t.Fatalf("= %v, %v", v, err)
	}
}

// No suggestion may drop the name: on every path, each output line that names an `ambit local` command also carries
// --name for the cluster. (The refusal of a foreign cluster is left out on purpose: it suggests another name.)
func TestUpEverySuggestionKeepsName(t *testing.T) {
	const name = "ambit-cli-test"
	port401 := fakeGatewayStatus(t, map[string]int{
		"nvidia/nemotron-3-super-120b-a12b:free": 401, "google/gemma-4-31b-it:free": 401})
	for _, c := range []struct {
		label string
		setup func(*rig)
	}{
		{"fresh", func(*rig) {}},
		{"fresh, no password, on a terminal", func(r *rig) { r.opts.NoPassword = true }},
		{"repair", func(r *rig) {
			r.kind.exists = true
			r.env.Store.Write(State{Name: name, Status: "ready", ContainerID: cid})
		}},
		{"busy port after preflight", func(r *rig) {
			r.kind.createErr = errors.New("Bind for 127.0.0.1:8443 failed: port is already allocated")
		}},
		{"test call refused", func(r *rig) {
			r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
			r.opts.HTTPPort = port401
			r.env.ModelsURL = "http://127.0.0.1:" + itoa(port401) + "/models"
			r.env.HTTP = func(int) *http.Client { return HTTPClient(port401) }
		}},
	} {
		r := newRig(t, c.label != "fresh")
		r.opts.Name, r.ops.name = name, name
		c.setup(r)
		if err := Up(context.Background(), r.p, r.env, r.opts); err != nil {
			var e *out.Error
			if !errors.As(err, &e) {
				e = &out.Error{What: "local up", Why: err.Error()}
			}
			r.p.Fail(e)
		}
		seen := 0
		for _, l := range strings.Split(r.out.String(), "\n") {
			if strings.Contains(l, "ambit local ") {
				seen++
				if !strings.Contains(l, "--name "+name) {
					t.Errorf("%s: a suggestion drops the name: %q", c.label, l)
				}
			}
		}
		if seen == 0 {
			t.Errorf("%s: no line mentions ambit local; the test checks nothing\n%s", c.label, r.out)
		}
	}
}

// I1 of the final review: Docker is read before the ownership check, so kind's docker ps never runs against an
// unreachable daemon, and the user gets the L1 line with its next step.
func TestLocalCommandsReadDockerFirst(t *testing.T) {
	for name, run := range map[string]func(r *rig) error{
		"up": func(r *rig) error { return Up(context.Background(), r.p, r.env, r.opts) },
		"down": func(r *rig) error {
			return Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, true)
		},
		"password": func(r *rig) error { return Password(context.Background(), r.p, r.env, "ambit") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, false)
			r.kind.existsErr = errors.New(`failed to list clusters: exec: "docker": executable file not found in $PATH`)
			r.env.Docker = func() DockerInfo { return DockerInfo{Err: "the docker command is not installed"} }
			err := run(r)
			var e *out.Error
			if !errors.As(err, &e) || e.What != "Docker is not running" || !strings.Contains(e.Next, "start Docker") ||
				!strings.Contains(e.Why, "the docker command is not installed") {
				t.Fatalf("err = %#v", err)
			}
			if r.kind.existsCalls != 0 {
				t.Fatal("the ownership check ran against an unreachable Docker")
			}
		})
	}
}

// An ownership check that fails for another reason (Docker answers, but kind cannot list clusters) also ends with
// a next step in up, down and password.
func TestOwnerErrorHasNextStep(t *testing.T) {
	for name, run := range map[string]func(r *rig) error{
		"up": func(r *rig) error { return Up(context.Background(), r.p, r.env, r.opts) },
		"down": func(r *rig) error {
			return Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, true)
		},
		"password": func(r *rig) error { return Password(context.Background(), r.p, r.env, "ambit") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, false)
			r.kind.existsErr = errors.New("failed to list clusters: boom")
			err := run(r)
			var e *out.Error
			if !errors.As(err, &e) || e.Next == "" || !strings.Contains(e.Why, "boom") {
				t.Fatalf("err = %#v", err)
			}
		})
	}
}

// I2 of the final review: every step of local up that fails ends with an *out.Error that has a next step, and the
// next step keeps --name.
func TestUpEveryStepErrorHasNextStep(t *testing.T) {
	const name = "ambit-cli-test"
	boom := errors.New("boom")
	badURL := "http://127.0.0.1:1/models" // nothing listens: the model list cannot be fetched
	for _, c := range []struct {
		step  string
		setup func(*rig)
	}{
		{"open the cluster", func(r *rig) {
			r.env.OpenOps = func(_, kctx string) (Ops, error) {
				if len(r.contexts) > 0 { // the first call is the ownership check of a fresh cluster: no label read
					return nil, boom
				}
				r.contexts = append(r.contexts, kctx)
				return r.ops, nil
			}
		}},
		{"kind create", func(r *rig) { r.kind.createErr = boom }},
		{"control plane", func(r *rig) { r.kind.cpErr = boom }},
		{"state file", func(r *rig) {
			d := t.TempDir()
			os.Chmod(d, 0o500) // reading works, writing the state file does not
			t.Cleanup(func() { os.Chmod(d, 0o700) })
			r.env.Store = Store{Dir: d}
		}},
		{"load archive", func(r *rig) { r.opts.DevArchives, r.kind.loadErr = []string{"console.tar"}, boom }},
		{"CRDs", func(r *rig) { r.ops.errs = map[string]error{"ApplyCRDs": boom} }},
		{"key lookup", func(r *rig) { r.ops.errs = map[string]error{"SecretExists": boom} }},
		{"admin password", func(r *rig) { r.ops.errs = map[string]error{"AdminPassword": boom} }},
		{"test model", func(r *rig) {
			r.opts.NoKey = true
			r.ops.applyErr = func([]byte) error { return boom }
		}},
		{"rule", func(r *rig) {
			r.opts.NoKey = true
			r.ops.applyErr = func(m []byte) error {
				if strings.Contains(string(m), "AccessRule") {
					return boom
				}
				return nil
			}
		}},
		{"OpenRouter secret", func(r *rig) {
			r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
			r.ops.secretErr = boom
		}},
		{"OpenRouter model list", func(r *rig) {
			r.opts.Key, r.opts.KeyGiven = "sk-or-v1-0123456789abcdef0123456789", true
			r.env.ModelsURL = badURL
		}},
		{"final state file", func(r *rig) {
			r.opts.NoKey = true
			r.ops.onReady = func() { r.env.Store.Remove(name) }
		}},
	} {
		t.Run(c.step, func(t *testing.T) {
			if c.step == "state file" && runtime.GOOS == "windows" { // needs POSIX directory permissions
				t.Skip("Windows does not honour a read-only directory mode; Linux and macOS cover this step")
			}
			r := newRig(t, false)
			r.opts.Name, r.ops.name = name, name
			c.setup(r)
			err := Up(context.Background(), r.p, r.env, r.opts)
			var e *out.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %#v, want *out.Error", err)
			}
			if e.Next == "" || !strings.Contains(e.Next, "--name "+name) {
				t.Errorf("next step %q is empty or drops the name", e.Next)
			}
		})
	}
}
