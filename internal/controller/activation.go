package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// A Kubernetes write is not gateway activation: agentgateway's controller must translate the object and push it to
// every proxy. Each step of a mode switch or rules change waits until agentgateway reports the written generation
// accepted (and attached, for a policy), with no proxy NACK since the write, then a settle time for the push:
// agentgateway v1.6.0 exposes no ACK a controller can read.

const (
	agentgatewayController = "agentgateway.dev/agentgateway"
	nackReason             = "AgentGatewayNackError"
	// A stalled change is tried again after gateRetryMin, doubling up to gateRetryMax.
	gateRetryMin = 10 * time.Second
	gateRetryMax = 5 * time.Minute
)

// Activation configures the gate (--activation-timeout, --activation-settle). A Reconciler without one has no gate
// (tests that do not play agentgateway); no flag turns it off.
type Activation struct {
	Timeout time.Duration
	Settle  time.Duration
	Gateway string        // the Gateway whose NACK Events count; "": ambit
	Poll    time.Duration // 0: 1 s
	// Sleep waits (nil: a timer); tests move a fake clock.
	Sleep func(context.Context, time.Duration) error
}

// Check refuses a timeout of 0 or less (many tools read 0 as "no limit"; here it would be no gate) and a negative
// settle time.
func (a *Activation) Check() error {
	if a.Timeout <= 0 {
		return fmt.Errorf("--activation-timeout must be more than 0 (got %s)", a.Timeout)
	}
	if a.Settle < 0 {
		return fmt.Errorf("--activation-settle must not be negative (got %s)", a.Settle)
	}
	return nil
}

// gated is one object a step wrote, at the generation the write returned.
type gated struct {
	kind, name string
	generation int64
	changed    bool // the write created it or changed its spec
}

// stalledError is a step that agentgateway did not apply: the change stops there.
type stalledError struct{ msg string }

func (e *stalledError) Error() string { return e.msg }

func (r *Reconciler) sleep(ctx context.Context, d time.Duration) error {
	if r.Activation.Sleep != nil {
		return r.Activation.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// nackSeen is the NACK Events on the Gateway before a write, by name, with their count.
type nackSeen map[string]int32

// nacksSeen lists the NACK Events on the Gateway now; a gate after the write fails on any other one, or on a higher
// count (the recorder folds a repeated NACK into the same Event). Clocks are not compared: the recorder stores whole
// seconds, and a NACK in the same second as the write would look older than it.
// Best effort: the recorder's spam filter (per object, 25 at once, then one per 5 minutes) can drop a NACK Event.
func (r *Reconciler) nacksSeen(ctx context.Context) (nackSeen, error) {
	nacks, err := r.nacks(ctx)
	if err != nil {
		return nil, err
	}
	seen := nackSeen{}
	for _, e := range nacks {
		seen[e.Name] = e.Count
	}
	return seen, nil
}

func (r *Reconciler) nacks(ctx context.Context) ([]corev1.Event, error) {
	var events corev1.EventList
	if err := r.Live.List(ctx, &events, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}
	gw := r.Activation.Gateway
	if gw == "" {
		gw = "ambit"
	}
	var out []corev1.Event
	for _, e := range events.Items {
		if e.Type == corev1.EventTypeWarning && e.Reason == nackReason && e.InvolvedObject.Kind == "Gateway" && e.InvolvedObject.Name == gw {
			out = append(out, e)
		}
	}
	return out, nil
}

// gate waits until every object is active, polling, for up to Activation.Timeout; then the settle time. seen is
// the NACK Events before the write;
// timeout 0 checks once (the retry of a stopped step). Every poll first applies what must not wait (urgent).
func (r *Reconciler) gate(ctx context.Context, objs []gated, seen nackSeen, timeout time.Duration) error {
	if len(objs) == 0 {
		return nil
	}
	poll := r.Activation.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := r.now().Add(timeout)
	for {
		if err := r.urgent(ctx); err != nil {
			return err
		}
		waiting, err := r.activated(ctx, objs, seen)
		if err != nil {
			return err
		}
		if waiting == "" {
			log.FromContext(ctx).Info("agentgateway accepted the step; waiting the settle time", "settle", r.Activation.Settle.String())
			return r.sleep(ctx, r.Activation.Settle)
		}
		if !r.now().Before(deadline) {
			return &stalledError{waiting}
		}
		if err := r.sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// activated returns "" when every object is active, or what it waits for. A NACK or an Invalid verdict on the
// written generation is a *stalledError at once.
func (r *Reconciler) activated(ctx context.Context, objs []gated, seen nackSeen) (string, error) {
	nacks, err := r.nacks(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range nacks {
		if n, ok := seen[e.Name]; !ok || e.Count > n {
			return "", &stalledError{fmt.Sprintf("a gateway pod rejected the update (%s): %s", nackReason, e.Message)}
		}
	}
	first := ""
	for _, g := range objs {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(AgentgatewayPolicyGVK.GroupVersion().WithKind(g.kind))
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: g.name}, o); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", err
			}
			// Not in the cache. Gone from the API server too: deleted after the write (deleting a Model removes its
			// AgentgatewayModel), so there is nothing left to apply. Otherwise the cache has not caught up yet.
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(AgentgatewayPolicyGVK.GroupVersion().WithKind(g.kind))
			if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: g.name}, live); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return "", err
			}
			o = nil
		}
		waiting, invalid := activationOf(o, g)
		if invalid {
			return "", &stalledError{waiting}
		}
		if first == "" {
			first = waiting
		}
	}
	return first, nil
}

// activationOf checks agentgateway's verdict on one object: every entry of its controller in status.ancestors (a
// policy: Accepted and Attached) or status.parents (a model: Accepted) true at the written generation or later.
func activationOf(o *unstructured.Unstructured, g gated) (waiting string, invalid bool) {
	prefix := fmt.Sprintf("waiting for agentgateway to apply %s/%s generation %d: ", g.kind, g.name, g.generation)
	if o == nil {
		return prefix + "the object is missing", false
	}
	field, types := "ancestors", []string{"Accepted", "Attached"}
	if g.kind == "AgentgatewayModel" {
		field, types = "parents", []string{"Accepted"}
	}
	entries, _, _ := unstructured.NestedSlice(o.Object, "status", field)
	seen := false
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if em["controllerName"] != agentgatewayController {
			continue
		}
		seen = true
		conds, _ := em["conditions"].([]any)
		for _, typ := range types {
			c := findCond(conds, typ)
			var detail string
			switch {
			case c == nil:
				detail = typ + " is missing"
			case observed(c) < g.generation:
				detail = fmt.Sprintf("%s is %v (%v: %v) for generation %d", typ, c["status"], c["reason"], c["message"], observed(c))
			case c["status"] != "True":
				detail = fmt.Sprintf("%s is %v (%v: %v)", typ, c["status"], c["reason"], c["message"])
				if c["status"] == "False" && c["reason"] == "Invalid" {
					return prefix + detail, true
				}
			default:
				continue
			}
			if waiting == "" {
				waiting = prefix + detail
			}
		}
	}
	if !seen {
		return prefix + types[0] + " is missing", false
	}
	return waiting, false
}

// lagging: agentgateway reported this object, but for an older generation (not translated yet).
func lagging(o *unstructured.Unstructured) bool {
	for _, field := range []string{"ancestors", "parents"} {
		entries, _, _ := unstructured.NestedSlice(o.Object, "status", field)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			conds, _ := em["conditions"].([]any)
			if em["controllerName"] != agentgatewayController {
				continue
			}
			for _, c := range conds {
				if cm, _ := c.(map[string]any); cm != nil && observed(cm) < o.GetGeneration() {
					return true
				}
			}
		}
	}
	return false
}

func findCond(conds []any, typ string) map[string]any {
	for _, c := range conds {
		if cm, _ := c.(map[string]any); cm != nil && cm["type"] == typ {
			return cm
		}
	}
	return nil
}

func observed(c map[string]any) int64 {
	switch v := c["observedGeneration"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// step is one write of a mode switch or rules change.
type step struct {
	name  string
	write func() ([]gated, error)
}

// stalledStep remembers what a stopped step wrote, so the retry waits for it although it writes nothing new.
type stalledStep struct {
	step string
	seen nackSeen
	objs []gated
}

// pending is what ambit-access-mode shows while a change is not active yet.
type pending struct {
	Mode         string `json:"mode"`
	RulesVersion string `json:"rulesVersion"`
	Step         string `json:"step"`
	Error        string `json:"error,omitempty"`
}

// runSteps writes each step in order. When changing (the mode or the rules version differs from the published
// one), each step waits for the gate before the next one runs. It returns the stopped step's error, or nil when
// every step ran (the caller then publishes); ran tells which steps ran.
func (r *Reconciler) runSteps(ctx context.Context, steps []step, changing bool, target pending) (stall *stalledError, ran map[string]bool, err error) {
	ran = map[string]bool{}
	if changing {
		if err := r.urgent(ctx); err != nil {
			return nil, ran, err
		}
	}
	for _, s := range steps {
		var seen nackSeen
		if changing {
			if seen, err = r.nacksSeen(ctx); err != nil {
				return nil, ran, err
			}
		}
		objs, err := s.write()
		var notWritten *stalledError
		if err != nil && !errors.As(err, &notWritten) {
			return nil, ran, err
		}
		ran[s.name] = true
		if !changing {
			continue
		}
		var wait []gated
		remembered := map[string]bool{}
		if r.stalled != nil && r.stalled.step == s.name {
			for _, g := range r.stalled.objs {
				remembered[g.kind+"/"+g.name] = true
			}
		}
		anyChanged := false
		for _, g := range objs {
			switch {
			case g.changed:
				anyChanged = true
			case remembered[g.kind+"/"+g.name]:
			default:
				o := &unstructured.Unstructured{}
				o.SetGroupVersionKind(AgentgatewayPolicyGVK.GroupVersion().WithKind(g.kind))
				if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: g.name}, o); err != nil || !lagging(o) {
					continue
				}
			}
			wait = append(wait, g)
		}
		timeout := r.Activation.Timeout
		if !anyChanged && r.stalled != nil && r.stalled.step == s.name {
			seen = r.stalled.seen // a NACK seen for the stopped write still counts
			// A retry checks once: waiting the whole timeout again on every pass would hold up the rest of the pass.
			timeout = 0
		}
		target.Step, target.Error = s.name, ""
		if err := r.setPending(ctx, &target); err != nil {
			return nil, ran, err
		}
		log.FromContext(ctx).Info("change step written; waiting for agentgateway", "step", s.name, "mode", target.Mode, "rulesVersion", target.RulesVersion)
		if notWritten != nil { // a write the API server refused stops the change like a gate that does not pass
			err = notWritten
		} else {
			err = r.gate(ctx, wait, seen, timeout)
		}
		if err != nil {
			var st *stalledError
			if !errors.As(err, &st) {
				return nil, ran, err
			}
			r.stalled = &stalledStep{step: s.name, seen: seen, objs: wait}
			target.Error = st.msg
			if err := r.setPending(ctx, &target); err != nil {
				return nil, ran, err
			}
			return st, ran, nil
		}
		log.FromContext(ctx).Info("change step active in the gateway", "step", s.name, "mode", target.Mode, "rulesVersion", target.RulesVersion)
	}
	r.stalled = nil
	return nil, ran, nil
}

// retryStalled is how long until a stopped change is tried again (doubling, up to gateRetryMax).
func (r *Reconciler) retryStalled() time.Duration {
	r.gateRetry = min(max(2*r.gateRetry, gateRetryMin), gateRetryMax)
	return r.gateRetry
}

// setPending writes (nil: removes) the pending entry of ambit-access-mode.
func (r *Reconciler) setPending(ctx context.Context, p *pending) error {
	return r.updateMode(ctx, func(d map[string]string) {
		delete(d, "pending")
		if p != nil {
			b, _ := json.Marshal(p)
			d["pending"] = string(b)
		}
	})
}
