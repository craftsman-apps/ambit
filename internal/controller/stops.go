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
	StopsConfigMap    = "ambit-agent-stops"
	StopsAckConfigMap = "ambit-agent-stops-ack"
	StopPolicyPrefix  = "ambit-agent-stops-"
	// StopsGenAnnotation: the list generation a policy was rendered from.
	StopsGenAnnotation = "ambitproject.dev/stops-gen"
	// MaxStopEntries is the most entries (agents and grants) one list holds; a longer list is not rendered. At most 150
	// names go into one policy, so at most 7 policies per route.
	MaxStopEntries = 1000
	StopChunk      = 150
	maxStopExpr    = 12000 // characters of one policy's expression; the CRD allows 16,384
	// StopAckLag is how long the console waits before it says that a stop is not confirmed.
	StopAckLag = 60 * time.Second
)

// StopEntry is one stopped agent or grant. The console writes it; this controller reads Kind, Name, Gen and RequestedAt.
type StopEntry struct {
	Kind        string    `json:"kind"` // "agent" (a name) or "grant" (an OAuth grant ID)
	Name        string    `json:"name"`
	Gen         int64     `json:"gen"` // the list generation that added it
	RequestedAt time.Time `json:"requestedAt"`
	// Console steps (the controller ignores them): S2 keys deleted, S3 grants revoked.
	KeysDeletedAt   *time.Time `json:"keysDeletedAt,omitempty"`
	GrantsRevokedAt *time.Time `json:"grantsRevokedAt,omitempty"`
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
}

// StopAck is the content of ambit-agent-stops-ack (key ack.json).
type StopAck struct {
	Gen int64 `json:"gen"` // the list generation last rendered
	// Gateways: the Gateways that have a route a policy is attached to. A stop is complete only when each has the entry's generation.
	Gateways map[string]GatewayAck `json:"gateways"`
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

// ParseStopAck reads ambit-agent-stops-ack; none is an empty answer.
func ParseStopAck(cm *corev1.ConfigMap) StopAck {
	var a StopAck
	if cm != nil {
		_ = json.Unmarshal([]byte(cm.Data["ack.json"]), &a)
	}
	return a
}

// stopPolicies renders the list as policies ambit-agent-stops-<k>, at most 150 agents and 150 grants each, for the
// given routes. An empty list is one policy that allows everyone, so a policy that once existed stays. A list over
// MaxStopEntries, or an expression over maxStopExpr characters, is an error: the caller keeps what is in force.
func stopPolicies(cm *corev1.ConfigMap, l StopList, routes []string, issuer string) ([]*unstructured.Unstructured, error) {
	var agents, grants []string
	for _, e := range l.Entries {
		switch e.Kind {
		case "agent":
			agents = append(agents, e.Name)
		case "grant":
			grants = append(grants, e.Name)
		default:
			return nil, fmt.Errorf("stop list entry %q has the unknown kind %q", e.Name, e.Kind)
		}
	}
	if len(agents)+len(grants) > MaxStopEntries {
		return nil, fmt.Errorf("the stop list has %d entries; at most %d are rendered", len(agents)+len(grants), MaxStopEntries)
	}
	sort.Strings(agents)
	agents = slices.Compact(agents)
	sort.Strings(grants)
	grants = slices.Compact(grants)
	chunks := max((len(agents)+StopChunk-1)/StopChunk, (len(grants)+StopChunk-1)/StopChunk, 1)
	var refs []any
	for _, r := range routes {
		refs = append(refs, map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": r})
	}
	var out []*unstructured.Unstructured
	for k := 0; k < chunks; k++ {
		part := func(l []string) []string { return l[min(k*StopChunk, len(l)):min((k+1)*StopChunk, len(l))] }
		expr := identity.StopList(part(agents), part(grants), issuer)
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

	mu   sync.Mutex
	seen map[string]time.Time // "<policy>/<generation>" -> first time seen accepted
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
	ack := ParseStopAck(&old)
	if ack.Gateways == nil {
		ack.Gateways = map[string]GatewayAck{}
	}
	list, err := ParseStopList(&cm)
	if err != nil {
		return r.degraded(ctx, ack, err.Error(), 10*time.Second)
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
		}
	}
	policies, err := stopPolicies(&cm, list, allRoutes, r.Issuer)
	if err != nil { // fail closed: what is applied stays, and the console and the alert say so
		logger.Error(err, "the stop list was not rendered; the last stop policies stay in force")
		return r.degraded(ctx, ack, err.Error(), 10*time.Second)
	}
	if len(allRoutes) > 0 {
		for _, p := range policies {
			if err := r.Patch(ctx, p, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
				return r.degraded(ctx, ack, "applying "+p.GetName()+": "+err.Error(), 5*time.Second)
			}
		}
		// Policies of chunks that are no longer needed (the list got shorter). Chunk 0 always stays.
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
	// Acknowledgment: per Gateway, every policy accepted at its current generation, rendered from this list generation.
	pending := false
	now := r.now()
	ack.Gen, ack.Degraded = list.Gen, ""
	for gw, routes := range gws {
		ok := len(allRoutes) > 0
		for _, p := range policies {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(AgentgatewayPolicyGVK)
			if err := r.Live.Get(ctx, client.ObjectKeyFromObject(p), live); err != nil {
				ok = false
				break
			}
			if live.GetAnnotations()[StopsGenAnnotation] != strconv.FormatInt(list.Gen, 10) || !ancestorsAccept(live, gw, routes) {
				ok = false
				break
			}
			key := fmt.Sprintf("%s/%d/%s", live.GetName(), live.GetGeneration(), gw)
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
		if ok && cur.AckedGen < list.Gen {
			cur = GatewayAck{AckedGen: list.Gen, At: now.UTC()}
		}
		if !ok {
			pending = true
		}
		ack.Gateways[gw] = cur
		// seconds the oldest unacknowledged request has waited
		wait := 0.0
		for _, e := range list.Entries {
			if e.Gen > cur.AckedGen {
				wait = max(wait, now.Sub(e.RequestedAt).Seconds())
			}
		}
		stopUnconfirmed.WithLabelValues(gw).Set(wait)
	}
	stopDegraded.Set(0)
	if err := r.writeAck(ctx, &old, ack); err != nil {
		return reconcile.Result{}, err
	}
	if pending {
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return reconcile.Result{}, nil
}

// degraded records why the list was not rendered. The policies in force stay.
func (r *StopReconciler) degraded(ctx context.Context, ack StopAck, why string, again time.Duration) (reconcile.Result, error) {
	stopDegraded.Set(1)
	ack.Degraded = why
	var old corev1.ConfigMap
	if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: StopsAckConfigMap}, &old); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	}
	if err := r.writeAck(ctx, &old, ack); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: again}, nil
}

func (r *StopReconciler) writeAck(ctx context.Context, old *corev1.ConfigMap, ack StopAck) error {
	b, _ := json.Marshal(ack)
	if old.Data["ack.json"] == string(b) {
		return nil
	}
	if old.Name == "" {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StopsAckConfigMap, Namespace: r.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": accessLogManager}}, Data: map[string]string{"ack.json": string(b)}}
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
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(AgentgatewayPolicyGVK)
	b = b.Watches(p, one, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool { return strings.HasPrefix(o.GetName(), StopPolicyPrefix) })))
	return b.Complete(r)
}
