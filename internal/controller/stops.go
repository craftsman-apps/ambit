package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The stop list (docs/agents.md, "Stopping an agent"): the console writes the agents (and OAuth grants) that are
// stopped to the ConfigMap ambit-agent-stops; this controller turns the list into Require policies on every MCP and
// LLM route of every Gateway, and writes into ambit-agent-stops-ack which list generation each Gateway has accepted.
// The console reports an agent stopped only after every Gateway acknowledged the generation of its entry.

const (
	StopsConfigMap = "ambit-agent-stops"
	// StopsAckLabel marks the acknowledgment ConfigMap: admission lets only the controller write it.
	StopsAckLabel     = "ambitproject.dev/agent-stops-ack"
	StopsAckConfigMap = "ambit-agent-stops-ack"
	StopPolicyPrefix  = "ambit-agent-stops-"
	// StopsGenAnnotation: the list generation a policy was rendered from.
	StopsGenAnnotation = "ambitproject.dev/stops-gen"
	// MaxStopEntries is the most entries (agents and grants) one list holds; a longer list is not rendered. At most 150
	// names go into one policy, so at most 7 policies per route.
	MaxStopEntries = 1000
	StopChunk      = 150
	maxStopExpr    = 15000 // characters of one policy's expression; the CRD allows 16,384 (150 names of at most 40 characters, in a key clause and a token clause: about 13,500)
	// StopAckLag is how long the console waits before it says that a stop is not confirmed.
	StopAckLag = 60 * time.Second
)

// StopEntry is one stopped agent or grant. The console writes it; this controller reads Kind, Name, Gen and RequestedAt.
type StopEntry struct {
	Kind        string    `json:"kind"` // "agent" (a name) or "grant" (an OAuth grant ID)
	Name        string    `json:"name"`
	Gen         int64     `json:"gen"` // the list generation that added it
	RequestedAt time.Time `json:"requestedAt"`
	// Slot is the entry's place in the list (0 to MaxStopEntries-1), fixed for its life: it decides which policy lists the
	// name (Slot/chunk size), so that an entry never moves from one policy to another while the others change. The console
	// gives the lowest free slot.
	Slot int `json:"slot"`
	// Console steps (the controller ignores them): S2 keys deleted, S3 grants revoked, and when every Gateway first
	// acknowledged the entry.
	KeysDeletedAt   *time.Time `json:"keysDeletedAt,omitempty"`
	GrantsRevokedAt *time.Time `json:"grantsRevokedAt,omitempty"`
	AckedAt         *time.Time `json:"ackedAt,omitempty"`
}

// StopList is the content of ambit-agent-stops (key list.json).
type StopList struct {
	Gen     int64       `json:"gen"` // raised by every change of the set of entries
	Entries []StopEntry `json:"entries"`
}

// GatewayAck is one Gateway's acknowledgment.
type GatewayAck struct {
	AckedGen int64     `json:"ackedGen"` // the highest list generation every policy of the Gateway has accepted
	At       time.Time `json:"at"`       // when it became that
	// Ready: right now every policy of the list's generation is accepted and settled on this Gateway. A policy that is
	// edited, deleted or rejected afterwards sets it to false: the console completes or prunes a stop only while it is true.
	Ready bool `json:"ready"`
}

// StopAck is the content of ambit-agent-stops-ack (key ack.json).
type StopAck struct {
	ListUID string `json:"listUID"` // the UID of the stop list ConfigMap this answer is for: a recreated list is not acknowledged by an old answer
	Gen     int64  `json:"gen"`     // the list generation last rendered
	// Gateways: the Gateways that have a route a policy is attached to. A stop is complete only when each has the entry's generation.
	Gateways map[string]GatewayAck `json:"gateways"`
	// CheckedAt: when the controller last looked. The console treats an answer older than two minutes as no answer (the controller
	// asks again every 30 s while the list has entries), so a controller that is down cannot leave an old "ready" in force.
	CheckedAt time.Time `json:"checkedAt"`
	// Degraded: why the list could not be rendered (the last policies stay in force); empty when it could.
	Degraded string `json:"degraded,omitempty"`
}

// ParseStopList reads the list of ambit-agent-stops; no ConfigMap is an empty list.
func ParseStopList(cm *corev1.ConfigMap) (StopList, error) {
	var l StopList
	if cm == nil || cm.Data["list.json"] == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(cm.Data["list.json"]), &l); err != nil {
		return StopList{}, fmt.Errorf("the stop list is unreadable: %w", err)
	}
	return l, nil
}

// ParseStopAck reads ambit-agent-stops-ack; none, or one that does not parse, is an empty answer (ok false).
func ParseStopAck(cm *corev1.ConfigMap) (a StopAck, ok bool) {
	if cm == nil || json.Unmarshal([]byte(cm.Data["ack.json"]), &a) != nil || a.Gateways == nil {
		return StopAck{}, false
	}
	return a, true
}

// StopChunkSize is how many names one policy lists: always 150, so that an entry never changes its policy when a setting changes.
func StopChunkSize(string) int { return StopChunk }

// stopPolicies renders the list as policies ambit-agent-stops-<k> for the given routes. An entry is in the policy
// Slot/chunk size, so entries never move between policies (a change of one policy never takes a name out of the block
// before another policy has it). A chunk without entries renders as `true` (everyone passes), so that a policy that once
// existed stays while an entry exists. An empty list is one such policy. A list over MaxStopEntries, or an expression
// over maxStopExpr characters, is an error: the caller keeps what is in force.
func stopPolicies(cm *corev1.ConfigMap, l StopList, routes []string, issuer string) ([]*unstructured.Unstructured, error) {
	size := StopChunkSize(issuer)
	if len(l.Entries) > MaxStopEntries {
		return nil, fmt.Errorf("the stop list has %d entries; at most %d are rendered", len(l.Entries), MaxStopEntries)
	}
	type chunk struct{ agents, grants []string }
	var chunks []chunk
	used := map[int]string{}
	for _, e := range l.Entries {
		if e.Slot < 0 || e.Slot >= MaxStopEntries {
			return nil, fmt.Errorf("stop list entry %q has the slot %d", e.Name, e.Slot)
		}
		if other, ok := used[e.Slot]; ok && other != e.Name {
			return nil, fmt.Errorf("stop list entries %q and %q have the same slot %d", other, e.Name, e.Slot)
		}
		used[e.Slot] = e.Name
		k := e.Slot / size
		for len(chunks) <= k {
			chunks = append(chunks, chunk{})
		}
		switch e.Kind {
		case "agent":
			chunks[k].agents = append(chunks[k].agents, e.Name)
		case "grant":
			chunks[k].grants = append(chunks[k].grants, e.Name)
		default:
			return nil, fmt.Errorf("stop list entry %q has the unknown kind %q", e.Name, e.Kind)
		}
	}
	if len(chunks) == 0 {
		chunks = append(chunks, chunk{})
	}
	var refs []any
	for _, r := range routes {
		refs = append(refs, map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": r})
	}
	var out []*unstructured.Unstructured
	for k, c := range chunks {
		sort.Strings(c.agents)
		sort.Strings(c.grants)
		expr := identity.StopList(slices.Compact(c.agents), slices.Compact(c.grants), issuer)
		if len(expr) > maxStopExpr {
			return nil, fmt.Errorf("stop policy %d needs %d characters; at most %d fit", k, len(expr), maxStopExpr)
		}
		o := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", StopPolicyPrefix+strconv.Itoa(k), cm.Namespace, map[string]any{
			"targetRefs": refs,
			"traffic": map[string]any{"authorization": map[string]any{
				"action": "Require",
				"policy": map[string]any{"matchExpressions": []any{expr}},
			}},
		})
		o.SetAnnotations(map[string]string{StopsGenAnnotation: strconv.FormatInt(l.Gen, 10)})
		o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: cm.Name, UID: cm.UID}})
		out = append(out, o)
	}
	return out, nil
}

var httpRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

var (
	stopUnconfirmed = promauto.With(metrics.Registry).NewGaugeVec(prometheus.GaugeOpts{Name: "ambit_agent_stop_unconfirmed_seconds",
		Help: "Seconds the oldest stop request has waited for this Gateway to accept the stop policy; 0 when none waits."}, []string{"gateway"})
	stopDegraded = promauto.With(metrics.Registry).NewGauge(prometheus.GaugeOpts{Name: "ambit_agent_stop_policy_degraded",
		Help: "1 when the stop list could not be rendered into policies (the last policies stay in force)."})
	stopEntries = promauto.With(metrics.Registry).NewGauge(prometheus.GaugeOpts{Name: "ambit_agent_stop_entries",
		Help: "Entries in the stop list."})
)

// StopReconciler keeps the stop policies and the acknowledgment in step with ambit-agent-stops. It is event driven
// (the list, the policies' status) and asks again every 2 s while an acknowledgment is missing.
type StopReconciler struct {
	client.Client
	Live      client.Reader
	Namespace string
	// Routes are the HTTPRoutes that carry credentials and get the stop policies (the MCP and LLM routes); a route that
	// does not exist is left out. The public MCP route is added when it exists (ambit-mcp-public).
	Routes []string
	// Issuer is the issuer whose tokens may carry agent claims ("" in 0.6.0 before OAuth).
	Issuer string
	// Settle is how long a policy accepted by agentgateway waits before it counts as applied by every proxy (agentgateway
	// has no acknowledgment a controller can read); --activation-settle.
	Settle time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	seen     map[string]time.Time // "<policy>/<uid>/<generation>/<gateway>" -> first time seen accepted
	nackBase map[string]int32     // the NACK Events of the Gateways before the first write of list generation nackGen
	nackGen  int64
}

func (r *StopReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// gatewayRoutes maps each Gateway to the stop routes attached to it, from the live HTTPRoutes.
func (r *StopReconciler) gatewayRoutes(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	names := append(slices.Clone(r.Routes), "ambit-mcp-public")
	for _, name := range slices.Compact(slices.Sorted(slices.Values(names))) {
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(httpRouteGVK)
		if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, route); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
		for _, p := range parents {
			if pm, _ := p.(map[string]any); pm != nil {
				if gw, _ := pm["name"].(string); gw != "" {
					out[gw] = append(out[gw], name)
				}
			}
		}
	}
	return out, nil
}

// ancestorFor reports whether agentgateway accepted and attached the policy at generation gen for every one of routes
// (or for the Gateway itself): status.ancestors, entries of agentgateway's controller.
func ancestorsAccept(o *unstructured.Unstructured, gateway string, routes []string) bool {
	entries, _, _ := unstructured.NestedSlice(o.Object, "status", "ancestors")
	good := map[string]bool{}
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if em["controllerName"] != agentgatewayController {
			continue
		}
		ref, _ := em["ancestorRef"].(map[string]any)
		name, _ := ref["name"].(string)
		conds, _ := em["conditions"].([]any)
		ok := true
		for _, typ := range []string{"Accepted", "Attached"} {
			c := findCond(conds, typ)
			if c == nil || c["status"] != "True" || observed(c) < o.GetGeneration() {
				ok = false
			}
		}
		if ok {
			good[name] = true
		}
	}
	if good[gateway] {
		return true
	}
	for _, r := range routes {
		if !good[r] {
			return false
		}
	}
	return len(routes) > 0
}

// currentNacks: the NACK Events of the given Gateways now, by Event name with their count (see Reconciler.nacksSeen).
func (r *StopReconciler) currentNacks(ctx context.Context, gateways map[string][]string) (map[string]int32, error) {
	var events corev1.EventList
	if err := r.Live.List(ctx, &events, client.InNamespace(r.Namespace), client.MatchingFields{"reason": nackReason}); err != nil {
		return nil, err
	}
	out := map[string]int32{}
	for _, e := range events.Items {
		if _, ok := gateways[e.InvolvedObject.Name]; ok && e.Type == corev1.EventTypeWarning && e.Reason == nackReason && e.InvolvedObject.Kind == "Gateway" {
			out[e.Name] = e.Count
		}
	}
	return out, nil
}

func (r *StopReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	var cm corev1.ConfigMap
	if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: StopsConfigMap}, &cm); err != nil {
		stopEntries.Set(0)
		stopDegraded.Set(0)
		return reconcile.Result{}, client.IgnoreNotFound(err) // no list yet: nothing is stopped
	}
	var old corev1.ConfigMap
	if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: StopsAckConfigMap}, &old); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}
	ack, _ := ParseStopAck(&old)
	if ack.Gateways == nil {
		ack.Gateways = map[string]GatewayAck{}
	}
	if ack.ListUID != string(cm.UID) { // an answer for another list (it was recreated) says nothing about this one
		ack = StopAck{Gateways: map[string]GatewayAck{}}
	}
	ack.ListUID = string(cm.UID)
	list, err := ParseStopList(&cm)
	if err != nil {
		return r.degraded(ctx, &old, ack, err.Error(), 10*time.Second)
	}
	stopEntries.Set(float64(len(list.Entries)))
	gws, err := r.gatewayRoutes(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	var allRoutes []string
	for _, rs := range gws {
		for _, route := range rs {
			if !slices.Contains(allRoutes, route) {
				allRoutes = append(allRoutes, route)
			}
		}
	}
	slices.Sort(allRoutes)
	for gw := range ack.Gateways { // a Gateway that has no route any more is not waited for
		if _, ok := gws[gw]; !ok {
			delete(ack.Gateways, gw)
			stopUnconfirmed.DeleteLabelValues(gw)
		}
	}
	policies, err := stopPolicies(&cm, list, allRoutes, r.Issuer)
	if err != nil { // fail closed: what is applied stays, and the console and the alert say so
		logger.Error(err, "the stop list was not rendered; the last stop policies stay in force")
		return r.degraded(ctx, &old, ack, err.Error(), 10*time.Second)
	}
	if len(allRoutes) > 0 {
		// One baseline of the NACK Events per list generation, taken before its first write and kept for the whole rollout:
		// a rejection of any of its policies is then never mistaken for an old one.
		r.mu.Lock()
		newRollout := r.nackGen != list.Gen || r.nackBase == nil
		r.mu.Unlock()
		if newRollout {
			base, err := r.currentNacks(ctx, gws)
			if err != nil {
				return reconcile.Result{}, err
			}
			r.mu.Lock()
			r.nackBase, r.nackGen = base, list.Gen
			r.mu.Unlock()
		}
		for _, p := range policies {
			if err := r.Patch(ctx, p, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
				return r.degraded(ctx, &old, ack, "applying "+p.GetName()+": "+err.Error(), 5*time.Second)
			}
		}
		// Policies of chunks that are no longer used (the highest slots are free again). Chunk 0 always stays. An entry never
		// moves between policies, so a deleted chunk lists nobody that is still listed.
		var all unstructured.UnstructuredList
		all.SetGroupVersionKind(AgentgatewayPolicyGVK)
		if err := r.Live.List(ctx, &all, client.InNamespace(r.Namespace)); err != nil {
			return reconcile.Result{}, err
		}
		keep := map[string]bool{}
		for _, p := range policies {
			keep[p.GetName()] = true
		}
		for i := range all.Items {
			o := &all.Items[i]
			if strings.HasPrefix(o.GetName(), StopPolicyPrefix) && !keep[o.GetName()] && ownedByAmbit(o) {
				if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
					return reconcile.Result{}, err
				}
			}
		}
	}
	// Acknowledgment: per Gateway, every policy accepted at its current generation, rendered from this list generation,
	// not rejected by a proxy since it was written, and settled. Ready says so for now; AckedGen only ever rises.
	pending := false
	now := r.now()
	ack.Gen, ack.Degraded, ack.CheckedAt = list.Gen, "", now.UTC()
	nacks, err := r.currentNacks(ctx, gws)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.mu.Lock()
	nacked := false
	for name, n := range nacks {
		if base, ok := r.nackBase[name]; !ok || n > base {
			nacked = true
		}
	}
	r.mu.Unlock()
	seenNow := map[string]bool{}
	for gw, routes := range gws {
		ok := len(allRoutes) > 0 && !nacked
		for _, p := range policies {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(AgentgatewayPolicyGVK)
			if err := r.Live.Get(ctx, client.ObjectKeyFromObject(p), live); err != nil {
				ok = false
				break
			}
			if live.GetAnnotations()[StopsGenAnnotation] != strconv.FormatInt(list.Gen, 10) || !ancestorsAccept(live, gw, routes) {
				ok = false
				continue
			}
			key := fmt.Sprintf("%s/%s/%d/%s", live.GetName(), live.GetUID(), live.GetGeneration(), gw)
			seenNow[key] = true
			r.mu.Lock()
			if r.seen == nil {
				r.seen = map[string]time.Time{}
			}
			first, had := r.seen[key]
			if !had {
				r.seen[key], first = now, now
			}
			r.mu.Unlock()
			if now.Sub(first) < r.Settle {
				ok = false
			}
		}
		cur := ack.Gateways[gw]
		cur.Ready = ok
		if ok && cur.AckedGen < list.Gen {
			cur.AckedGen, cur.At = list.Gen, now.UTC()
		}
		if !ok {
			pending = true
		}
		ack.Gateways[gw] = cur
		// seconds the oldest request this Gateway has not confirmed has waited
		wait := 0.0
		for _, e := range list.Entries {
			if e.Gen > cur.AckedGen || (e.Gen == list.Gen && !cur.Ready) {
				wait = max(wait, now.Sub(e.RequestedAt).Seconds())
			}
		}
		stopUnconfirmed.WithLabelValues(gw).Set(wait)
	}
	r.mu.Lock() // a policy that lost its acceptance starts its settle time again
	for k := range r.seen {
		if !seenNow[k] {
			delete(r.seen, k)
		}
	}
	r.mu.Unlock()
	stopDegraded.Set(0)
	if err := r.writeAck(ctx, &old, ack); err != nil {
		return reconcile.Result{}, err
	}
	switch {
	case pending:
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	case len(list.Entries) > 0:
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil // a safety net: routes and Gateways are watched too
	}
	return reconcile.Result{}, nil
}

// degraded records why the list was not rendered. The policies in force stay, and no Gateway is ready.
func (r *StopReconciler) degraded(ctx context.Context, old *corev1.ConfigMap, ack StopAck, why string, again time.Duration) (reconcile.Result, error) {
	stopDegraded.Set(1)
	ack.Degraded = why
	for g, v := range ack.Gateways {
		v.Ready = false
		ack.Gateways[g] = v
	}
	if err := r.writeAck(ctx, old, ack); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: again}, nil
}

func (r *StopReconciler) writeAck(ctx context.Context, old *corev1.ConfigMap, ack StopAck) error {
	if ack.CheckedAt.IsZero() {
		ack.CheckedAt = r.now().UTC()
	}
	b, _ := json.Marshal(ack)
	if prev, ok := ParseStopAck(old); ok && old.Data["ack.json"] != "" {
		same := prev
		same.CheckedAt = ack.CheckedAt
		sb, _ := json.Marshal(same)
		if string(sb) == string(b) && ack.CheckedAt.Sub(prev.CheckedAt) < 20*time.Second {
			return nil // nothing changed, and the last look is recent enough
		}
	}
	if old.Name == "" {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StopsAckConfigMap, Namespace: r.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": accessLogManager, StopsAckLabel: "true"}}, Data: map[string]string{"ack.json": string(b)}}
		return r.Create(ctx, cm)
	}
	next := old.DeepCopy()
	next.Data = maps.Clone(next.Data)
	if next.Data == nil {
		next.Data = map[string]string{}
	}
	next.Data["ack.json"] = string(b)
	return r.Update(ctx, next)
}

// SetupWithManager watches the list and the stop policies' status.
func (r *StopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "stops"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).Named("ambit-agent-stops").
		Watches(&corev1.ConfigMap{}, one, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == StopsConfigMap })))
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(httpRouteGVK)
	b = b.Watches(rt, one) // a route that gains or loses a parent Gateway changes who must acknowledge
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(AgentgatewayPolicyGVK)
	b = b.Watches(p, one, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool { return strings.HasPrefix(o.GetName(), StopPolicyPrefix) })))
	return b.Complete(r)
}
