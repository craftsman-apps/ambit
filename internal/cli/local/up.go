package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/checks"
	"github.com/craftsman-apps/ambit/internal/cli/kindx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// KindAPI is the part of kindx.Kind that the local commands use.
type KindAPI interface {
	Exists(name string) (bool, error)
	Create(name string, cfg []byte, kubeconfig string) error
	Delete(name, kubeconfig string) error
	ControlPlane(name string) (string, error)
	LoadArchive(name, path string) error
}

// Env is everything the local commands touch outside the process, so tests can replace it.
type Env struct {
	Store       Store
	Kind        KindAPI
	ContainerID func(container string) (string, error)
	Containers  func(cluster string) ([]kindx.Container, error)
	Docker      func() DockerInfo
	PortFree    func(port int) bool
	// OpenOps is always called with the context kind-<name>, never the user's current context.
	OpenOps      func(kubeconfig, kubeContext string) (Ops, error)
	HTTP         func(port int) *http.Client
	ModelsClient *http.Client
	ModelsURL    string
	CRDBundle    []byte
	Kubeconfig   string // --kubeconfig; empty: kind's and kubectl's default
	ModelWait    time.Duration
	CallLimit    time.Duration
	CallEvery    time.Duration
}

// UpOptions are the flags of `ambit local up` (spec 4.1). Key is the OpenRouter key when one was given; Prompt
// asks for one on a terminal (nil without a terminal).
type UpOptions struct {
	Name                string
	HTTPPort, HTTPSPort int
	SkipChecks          bool
	Timeout             time.Duration
	Charts              Charts
	Key                 string
	KeyGiven            bool
	NoKey               bool
	Prompt              func() (string, error)
	Model               string
	NoPassword          bool
	DevArchives         []string
	DevValues           []string
}

// KubeContext is the kubeconfig context kind creates for a cluster.
func KubeContext(name string) string { return "kind-" + name }

// clusterView adapts Env to the Cluster interface of the ownership check.
type clusterView struct{ e *Env }

func (v clusterView) Exists(n string) (bool, error)                  { return v.e.Kind.Exists(n) }
func (v clusterView) ControlPlane(n string) (string, error)          { return v.e.Kind.ControlPlane(n) }
func (v clusterView) ContainerID(c string) (string, error)           { return v.e.ContainerID(c) }
func (v clusterView) Containers(n string) ([]kindx.Container, error) { return v.e.Containers(n) }
func (v clusterView) NodeLabels(ctx context.Context, n string) (map[string]map[string]string, error) {
	ops, err := v.e.OpenOps(v.e.Kubeconfig, KubeContext(n))
	if err != nil {
		return nil, err
	}
	return ops.NodeLabels(ctx)
}

// Owner runs the one ownership check for name.
func (e *Env) Owner(ctx context.Context, name string) (Ownership, error) {
	return CheckOwnership(ctx, e.Store, clusterView{e}, name)
}

// Up runs `ambit local up` (spec 4.1). Every step prints one line with its duration.
func Up(ctx context.Context, p *out.Printer, e *Env, o UpOptions) error {
	kctx := KubeContext(o.Name)
	step := func(what string, f func() error) error {
		start := time.Now()
		err := f()
		p.Step(err == nil, what, time.Since(start))
		return err
	}

	// 1. Preflight.
	var own Ownership
	if err := step("Preflight", func() error {
		var err error
		if own, err = e.Owner(ctx, o.Name); err != nil {
			return err
		}
		var failed *checks.Result
		for _, r := range Preflight(PreflightInput{Name: o.Name, HTTPPort: o.HTTPPort, HTTPSPort: o.HTTPSPort,
			SkipChecks: o.SkipChecks, Docker: e.Docker(), Ownership: own, PortFree: e.PortFree}) {
			if r.Status == checks.Pass || r.Status == checks.NA {
				continue
			}
			p.Line("  %s %s", p.Mark(map[checks.Status]string{checks.Warn: "warn"}[r.Status]), r.Message)
			if r.Status == checks.Fail && failed == nil {
				r := r
				failed = &r
			}
		}
		if own.Verdict == NotOwned {
			return own.Refusal(o.Name)
		}
		if failed != nil {
			return &out.Error{What: "preflight", Why: failed.Message, Next: failed.Next}
		}
		return nil
	}); err != nil {
		return err
	}

	// 2. Cluster: create, or repair a cluster with both ownership proofs.
	what := "Cluster " + o.Name + " (created)"
	if own.Verdict == Owned {
		what = "Cluster " + o.Name + " (exists; repairing)"
	}
	if err := step(what, func() error {
		if own.Verdict == Owned {
			return nil
		}
		return createCluster(ctx, e, o)
	}); err != nil {
		return err
	}
	ops, err := e.OpenOps(e.Kubeconfig, kctx)
	if err != nil {
		return err
	}
	for _, a := range o.DevArchives {
		if err := e.Kind.LoadArchive(o.Name, a); err != nil {
			return err
		}
	}

	// 3. CRDs.
	if err := step("CRDs (Gateway API, agentgateway, ToolHive)", func() error { return ops.ApplyCRDs(ctx, e.CRDBundle) }); err != nil {
		return err
	}

	// 4. Charts.
	base, err := assets.QuickstartValues(ConsoleURL(o.HTTPPort))
	if err != nil {
		return err
	}
	vals, err := MergeValues(base, o.DevValues)
	if err != nil {
		return out.Usage("--dev-values", err.Error(), "")
	}
	if err := step("Charts ambit-crds and ambit", func() error {
		return ops.InstallCharts(ctx, o.Charts, vals, o.Timeout, func(l string) { p.Line("  %s", l) })
	}); err != nil {
		return chartsError(p, o.Name, err)
	}

	// 5-7. Model, rule and test call. The rule names the Model object, so it is applied once; with OpenRouter the
	// Model's spec.model changes until a candidate answers.
	key, useKey, err := chooseKey(ctx, ops, o)
	if err != nil {
		return err
	}
	if useKey != "" {
		p.Redactor().Add(useKey)
	}
	pw, err := ops.AdminPassword(ctx)
	if err != nil {
		return fmt.Errorf("read the bootstrap admin password: %w", err)
	}
	p.Redactor().Add(pw)
	httpc := e.HTTP(o.HTTPPort)
	call := func(model string) (string, error) {
		tok, err := DexToken(ctx, httpc, ConsoleURL(o.HTTPPort), pw)
		if err != nil {
			return "", err
		}
		p.Redactor().Add(tok)
		return TestCall(ctx, httpc, LLMURL(o.HTTPPort), tok, model, e.CallLimit, e.CallEvery)
	}

	var answer, chosen string
	if key == "" {
		chosen = "local/echo"
		if err := step("Model local-test (test model)", func() error {
			if err := ops.Apply(ctx, assets.TestModel()); err != nil {
				return err
			}
			return ops.WaitModelReady(ctx, "local-test", e.ModelWait)
		}); err != nil {
			return err
		}
		p.Line("  Using a test model that repeats your message. Add a real model later: ambit local up%s --openrouter-key-file <file>", nameFlag(o.Name))
		if err := step("Rule everyone-local", func() error { return ops.Apply(ctx, accessRule("local-test")) }); err != nil {
			return err
		}
		if err := step("Test call", func() error { answer, err = call(chosen); return err }); err != nil {
			return testCallError(o.Name, err)
		}
	} else {
		var cands []string
		if err := step("Model free-model (OpenRouter)", func() error {
			if useKey != "" {
				if err := ops.ApplySecret(ctx, "ambit-llm-openrouter", map[string]string{"Authorization": useKey}); err != nil {
					return err
				}
			}
			if o.Model != "" { // --model: no list is fetched; the given model is the only candidate
				cands = []string{o.Model}
				return nil
			}
			free, err := FetchFreeModels(ctx, e.ModelsClient, e.ModelsURL)
			if err != nil {
				return err
			}
			cands, err = Candidates(free, "")
			return err
		}); err != nil {
			return err
		}
		if err := step("Rule everyone-local", func() error { return ops.Apply(ctx, accessRule("free-model")) }); err != nil {
			return err
		}
		var fatal error // a failure that is not the model's: stop trying candidates
		if err := step("Test call", func() error {
			id, err := TryCandidates(cands, func(id string) error {
				if err := ops.Apply(ctx, openRouterModel(id)); err != nil {
					fatal = err
					return nil
				}
				if err := ops.WaitModelReady(ctx, "free-model", e.ModelWait); err != nil {
					fatal = err
					return nil
				}
				a, err := call(id)
				var pe *ProviderError
				var na *NoAnswerError
				switch {
				case errors.As(err, &pe):
					p.Line("  %s refused the call (%s); trying the next free model", id, pe.Error())
					return err
				case errors.As(err, &na) && na.LastStatus == http.StatusNotFound:
					// A removed provider model and the gateway's own settling 404 share this status; after the
					// whole window it is the model (spec 4.1.1).
					p.Line("  %s did not answer (HTTP 404 until the time limit); trying the next free model", id)
					return err
				}
				fatal, answer = err, a
				return nil
			})
			if fatal != nil {
				return fatal
			}
			chosen = id
			return err
		}); err != nil {
			return testCallError(o.Name, err)
		}
	}
	p.Line("  %s answered: %s", chosen, answer)

	// 8. Summary.
	st, err := e.Store.Read(o.Name)
	if err != nil || st == nil {
		return fmt.Errorf("state file %s: %v", e.Store.Path(o.Name), err)
	}
	st.Status = "ready"
	if err := e.Store.Write(*st); err != nil {
		return err
	}
	summary(p, o, chosen, pw)
	return nil
}

func createCluster(ctx context.Context, e *Env, o UpOptions) error {
	cfg, err := kindx.Config(assets.KindClusterYAML(), o.Name, o.HTTPPort, o.HTTPSPort)
	if err != nil {
		return err
	}
	created := time.Now().UTC()
	if err := e.Store.Write(State{Name: o.Name, Status: "creating", Created: created}); err != nil {
		return err
	}
	prev, err := CurrentContext(e.Kubeconfig)
	if err != nil {
		return err
	}
	if err := e.Kind.Create(o.Name, cfg, e.Kubeconfig); err != nil {
		e.Store.Remove(o.Name) // kind deleted the cluster whose create failed
		if strings.Contains(err.Error(), "port is already allocated") || strings.Contains(err.Error(), "address already in use") {
			return &out.Error{What: "a port was taken after preflight", Why: err.Error(),
				Next: fmt.Sprintf("ambit local up%s --port %d --https-port %d", nameFlag(o.Name), o.HTTPPort+10000, o.HTTPSPort+10000)}
		}
		return err
	}
	if err := RestoreContext(e.Kubeconfig, prev); err != nil {
		return fmt.Errorf("restore your kubectl context %q: %w", prev, err)
	}
	cp, err := e.Kind.ControlPlane(o.Name)
	if err != nil {
		return err
	}
	id, err := e.ContainerID(cp)
	if err != nil {
		return err
	}
	if err := e.Store.Write(State{Name: o.Name, Status: "creating", ContainerID: id, Created: created}); err != nil {
		return err
	}
	// The partial-creation test (spec 9.2a) depends on the label existing as soon as create returns.
	own, err := e.Owner(ctx, o.Name)
	if err != nil {
		return err
	}
	if own.Verdict != Owned {
		return &out.Error{What: "kind did not mark the cluster", Why: own.Reason,
			Next: "report this at https://github.com/craftsman-apps/ambit/issues (kind's nodes[].labels did not apply)"}
	}
	return nil
}

// chooseKey decides between OpenRouter and the test model. useKey is a new key to store; key is non-empty when
// OpenRouter is used (it may be the key already in the cluster, which the CLI does not read back).
func chooseKey(ctx context.Context, ops Ops, o UpOptions) (key, useKey string, err error) {
	if o.NoKey {
		return "", "", nil
	}
	if o.KeyGiven {
		return o.Key, o.Key, nil
	}
	exists, err := ops.SecretExists(ctx, "ambit-llm-openrouter")
	if err != nil {
		return "", "", err
	}
	if exists {
		return "existing", "", nil // repair keeps the existing OpenRouter Secret (spec 4.1)
	}
	if o.Prompt == nil {
		return "", "", nil
	}
	k, err := o.Prompt()
	if err != nil {
		return "", "", err
	}
	k = strings.TrimSpace(k)
	return k, k, nil
}

func accessRule(model string) []byte {
	return mustYAML(map[string]any{"apiVersion": "ambitproject.dev/v1alpha1", "kind": "AccessRule",
		"metadata": map[string]any{"name": "everyone-local", "labels": map[string]string{kindx.Label: kindx.LabelValue}},
		"spec":     map[string]any{"allSignedIn": true, "models": []string{model}}})
}

func openRouterModel(id string) []byte {
	return mustYAML(map[string]any{"apiVersion": "ambitproject.dev/v1alpha1", "kind": "Model",
		"metadata": map[string]any{"name": "free-model", "labels": map[string]string{kindx.Label: kindx.LabelValue}},
		"spec": map[string]any{"displayName": "Free model (OpenRouter)", "provider": "Openrouter", "model": id,
			"credentialsRef": map[string]string{"name": "ambit-llm-openrouter"}}})
}

func mustYAML(v any) []byte {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func chartsError(p *out.Printer, name string, err error) *out.Error {
	why := err.Error()
	var nr *kube.NotReadyError
	if errors.As(err, &nr) {
		for _, l := range nr.Pods {
			p.Line("  %s", l)
		}
		why = fmt.Sprintf("pods not ready after %s (listed above)", nr.Timeout)
	}
	return &out.Error{What: "the charts did not become ready", Why: why,
		Next: fmt.Sprintf("kubectl --context %s -n %s get pods; then run ambit local up%s again (it repairs)", KubeContext(name), Namespace, nameFlag(name))}
}

func testCallError(name string, err error) *out.Error {
	return &out.Error{What: "the test call failed", Why: err.Error(),
		Next: fmt.Sprintf("run ambit local up%[1]s again (it repairs), or ambit local up%[1]s --no-key for the test model", nameFlag(name))}
}

func summary(p *out.Printer, o UpOptions, model, pw string) {
	console := ConsoleURL(o.HTTPPort)
	p.Line("")
	p.Line("Ambit is running.")
	p.Line("  Console:  %s", console)
	p.Line("  Sign in:  %s", AdminEmail)
	if o.NoPassword || !p.RevealOnTTY("  Password", pw) {
		p.Line("  Password: run `ambit local password%s`", nameFlag(o.Name))
	}
	p.Line("  kubectl:  kubectl --context %s -n %s get pods", KubeContext(o.Name), Namespace)
	p.Line("  Remove:   ambit local down%s", nameFlag(o.Name))
	p.Line("")
	p.Line("Call the model from your shell:")
	for _, l := range strings.Split(curlExample(runtime.GOOS, o.HTTPPort, o.Name, model), "\n") {
		p.Line("  %s", l)
	}
}

func nameFlag(name string) string {
	if name == "ambit" {
		return ""
	}
	return " --name " + name
}

// curlExample is a copy-ready call for the user's shell: PowerShell on Windows, POSIX sh elsewhere (ruling S10).
func curlExample(goos string, port int, name, model string) string {
	c, l, n := ConsoleURL(port), LLMURL(port), nameFlag(name)
	if goos == "windows" {
		return fmt.Sprintf(`$pw = ambit local password%s
$tok = (Invoke-RestMethod -Method Post %s/dex/token -Body @{grant_type='password'; client_id='ambit-cli'; scope='openid email profile'; username='%s'; password=$pw}).access_token
Invoke-RestMethod -Method Post %s/v1/chat/completions -Headers @{Authorization="Bearer $tok"} -ContentType 'application/json' -Body '{"model":"%s","messages":[{"role":"user","content":"Hello"}]}'`, n, c, AdminEmail, l, model)
	}
	return fmt.Sprintf(`TOKEN=$(curl -s %s/dex/token -d grant_type=password -d client_id=ambit-cli -d scope="openid email profile" \
  --data-urlencode username=%s --data-urlencode password="$(ambit local password%s)" | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
curl -s %s/v1/chat/completions -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"%s","messages":[{"role":"user","content":"Hello"}]}'`, c, AdminEmail, n, l, model)
}

// KeySource is where an OpenRouter key can come from: --openrouter-key-file, OPENROUTER_API_KEY, or --no-key.
type KeySource struct {
	File, Env string
	NoKey     bool
}

// ReadKey returns the key and whether one was given. A key file may have a UTF-8 byte order mark and a CRLF or
// LF line end (Notepad, echo); both are removed. An empty or unreadable file is a usage error.
func ReadKey(s KeySource) (string, bool, error) {
	if s.NoKey {
		return "", false, nil
	}
	if s.File != "" {
		b, err := os.ReadFile(s.File)
		if err != nil {
			return "", false, out.Usage("--openrouter-key-file", err.Error(), "check the path")
		}
		k := strings.TrimSpace(string(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))))
		if k == "" || !utf8.ValidString(k) || strings.ContainsAny(k, "\r\n") {
			return "", false, out.Usage("--openrouter-key-file", s.File+" must hold one key on one line", "")
		}
		return k, true, nil
	}
	if k := strings.TrimSpace(s.Env); k != "" {
		return k, true, nil
	}
	return "", false, nil
}
