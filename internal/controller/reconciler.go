package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/pricing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	AgentgatewayModelGVK   = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayModel"}
	AgentgatewayBackendGVK = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayBackend"}
	AgentgatewayPolicyGVK  = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayPolicy"}
	MCPServerGVK           = schema.GroupVersionKind{Group: "toolhive.stacklok.dev", Version: "v1beta1", Kind: "MCPServer"}
)

// Every kind the controller generates; each is watched so manual edits are reverted.
var generatedGVKs = []schema.GroupVersionKind{AgentgatewayModelGVK, MCPServerGVK, AgentgatewayBackendGVK, AgentgatewayPolicyGVK}

const fieldOwner = client.FieldOwner("ambit-controller")

// Reconciler recomputes the whole namespace on every change. ponytail: fine for hundreds of
// models, servers and rules; switch to per-object keys if a namespace ever holds thousands.
type Reconciler struct {
	client.Client
	// Live reads Ambit objects from the API server, not the cache. Status writes are skipped when
	// nothing changed, and a stale cached status once made that skip keep a wrong Pending forever.
	Live      client.Reader
	Namespace string
	Route     RouteRef
	// ListTools reads a running server's tools into Server.status.tools; nil turns this off.
	ListTools ToolLister
	// ToolsRefresh is how often tools are read again (0: 10 minutes).
	ToolsRefresh time.Duration
	// RateLimit is where Budgets are enforced; an empty Service turns budgets off.
	RateLimit RateLimit

	toolsRead   map[types.UID]time.Time // when each Server's tools were last read (single worker, no lock)
	toolsFailed map[types.UID]time.Time // when a Server's tools last failed to be read
	admitAt     map[types.UID]time.Time // when to ask a server that has not answered its MCP introduction again
	admitWait   map[types.UID]time.Duration
	// ArgumentLimits is true when ambit-guard runs (protection on) and enforces argument limits.
	ArgumentLimits bool
	// Classifier is ambit-router, which labels prompts for routers of type LLM.
	Classifier Classifier
	// Now is the clock (nil: time.Now); tests move it.
	Now func() time.Time
}

// How often the tools of a running server are read again by default: servers may add or change tools.
const toolsRefresh = 10 * time.Minute

// How long a server whose tools could not be read waits before it is asked again.
const toolsRetry = time.Minute

func (r *Reconciler) refresh() time.Duration {
	if r.ToolsRefresh > 0 {
		return r.ToolsRefresh
	}
	return toolsRefresh
}

// pinTools pins a server's tools the first time they are read (trust on first use) and reports every
// tool whose definition no longer matches its pin, or that has none. ambit-guard hides and refuses those.
func pinTools(s *v1.Server) {
	if len(s.Status.Tools) == 0 {
		return
	}
	if len(s.Status.ApprovedTools) == 0 {
		for _, t := range s.Status.Tools {
			s.Status.ApprovedTools = append(s.Status.ApprovedTools, v1.ToolPin{Name: t.Name, Fingerprint: t.Fingerprint})
		}
	}
	pinned := map[string]string{}
	for _, p := range s.Status.ApprovedTools {
		pinned[p.Name] = p.Fingerprint
	}
	var changed []string
	for _, t := range s.Status.Tools {
		if pinned[t.Name] != t.Fingerprint {
			changed = append(changed, t.Name)
		}
	}
	c := metav1.Condition{Type: "ToolsPinned", Status: metav1.ConditionTrue, Reason: "Pinned",
		Message: fmt.Sprintf("%d tools match their approved definitions", len(s.Status.Tools)), ObservedGeneration: s.Generation}
	if len(changed) > 0 {
		c.Status, c.Reason = metav1.ConditionFalse, "ToolsChanged"
		c.Message = "changed or new since approval, hidden until an admin approves: " + strings.Join(changed, ", ")
	}
	meta.SetStatusCondition(&s.Status.Conditions, c)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "all"}}}
	})
	offboarded := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == OffboardedConfigMap })
	b := ctrl.NewControllerManagedBy(mgr).Named("ambit").
		Watches(&corev1.ConfigMap{}, all, builder.WithPredicates(offboarded)).
		// Only spec changes of Ambit objects: our own status writes must not trigger another pass.
		Watches(&v1.Model{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.Server{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.AccessRule{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.Budget{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.ModelRouter{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.Connection{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if r.Classifier.Deployment != "" { // availability of ambit-router sets classifierState
		b = b.Watches(&appsv1.Deployment{}, all)
	}
	// Generated objects: status changes (agentgateway or ToolHive accepting them) and manual edits both matter.
	for _, gvk := range generatedGVKs {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		b = b.Watches(u, all)
	}
	return b.Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	var models v1.ModelList
	var servers v1.ServerList
	var rules v1.AccessRuleList
	for _, l := range []client.ObjectList{&models, &servers, &rules} {
		if err := r.Live.List(ctx, l, client.InNamespace(r.Namespace)); err != nil {
			return reconcile.Result{}, err
		}
	}
	var conns v1.ConnectionList
	if err := r.Live.List(ctx, &conns, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	saved := servers.DeepCopy() // the stored statuses: markRunning changes servers in memory before compiling
	admitRetry, err := r.markRunning(ctx, servers.Items)
	if err != nil {
		return reconcile.Result{}, err
	}
	boundServers, refusedServers := BindServers(servers.Items, conns.Items)
	var routers v1.ModelRouterList
	if err := r.Live.List(ctx, &routers, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	var routerNames []string
	for _, mr := range routers.Items {
		routerNames = append(routerNames, mr.Name)
	}
	compiled := rules.Items
	var dropped map[string][]string
	if !r.ArgumentLimits {
		compiled, dropped = WithoutArgumentLimits(rules.Items)
	}
	res := Compile(models.Items, boundServers, compiled, r.Route, routerNames...)
	SetBrokerPaths(res.Objects, boundServers, conns.Items)
	ApplyDropped(&res, dropped)
	var budgets v1.BudgetList
	if err := r.Live.List(ctx, &budgets, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	prices := pricing.ByModel(models.Items)
	refused := refuseUnpricedIfEnforced(r.RateLimit, &res, models.Items, prices, budgets.Items)
	dep, err := r.routerDeployment(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	keys, err := r.readKeys(ctx, routers.Items, models.Items)
	if err != nil {
		return reconcile.Result{}, err
	}
	// After RefuseUnpriced: each router line repeats its target's final access expression.
	routerConds, routerCfg := CompileRouters(routers.Items, models.Items, &res, r.Route, refused, ClassifierInput{Installed: dep != nil, HasKey: hasKeys(keys)})
	classify := dep != nil && len(routerCfg.Routers) > 0
	if dep != nil {
		res.Objects = append(res.Objects, configMapObject(r.Namespace, routerCfg))
	}
	if classify { // pods first, then the policy: a policy without ready pods fails open
		if err := r.scale(ctx, dep, r.Classifier.Replicas); err != nil {
			return reconcile.Result{}, err
		}
		res.Objects = append(res.Objects, classifyPolicy(r.Namespace, r.Route.Name, r.Classifier.Deployment, routerCfg))
	}
	budgetPolicy, usage := false, false
	if r.RateLimit.Service != "" {
		cfg, p := compileBudgets(budgets.Items, prices, r.Namespace, r.RateLimit, r.Route.Name)
		res.Objects = append(res.Objects, rateLimitConfigMap(r.Namespace, cfg))
		if p != nil {
			res.Objects = append(res.Objects, p)
			budgetPolicy = true
		}
		if u := usagePolicy(budgets.Items, r.Namespace, r.Route.Name); u != nil {
			res.Objects = append(res.Objects, u)
			usage = true
		}
	}
	var cm corev1.ConfigMap
	switch err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: OffboardedConfigMap}, &cm); {
	case err == nil:
		emails, rejected := OffboardedEmails(&cm)
		if len(rejected) > 0 {
			logger.Error(nil, "offboarded list has entries that are not plain emails; they are not blocked", "entries", rejected)
		}
		res.Objects = append(res.Objects, offboardedPolicy(&cm, emails, []string{"ambit-mcp", r.Route.Name}))
	case !apierrors.IsNotFound(err):
		return reconcile.Result{}, err
	}

	// Ready condition per Ambit object that controls a generated object, keyed "Kind/name".
	conds := map[string]metav1.Condition{}
	applyFailed := false
	for _, desired := range res.Objects {
		name, kind := desired.GetName(), desired.GetKind()
		owner := controllerOwner(desired) // "" for the shared MCP backend and policy
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(desired.GroupVersionKind())
		err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, current)
		switch {
		case err == nil && !ownedByAmbit(current):
			msg := fmt.Sprintf("a %s named %q already exists and was not created by Ambit", kind, name)
			if owner == "" {
				logger.Error(nil, msg+"; MCP traffic stays on the existing object until it is removed")
				continue
			}
			conds[owner] = cond(metav1.ConditionFalse, "NameConflict", msg+"; rename the "+strings.Split(owner, "/")[0])
			continue
		case err != nil && !apierrors.IsNotFound(err):
			return reconcile.Result{}, err
		}
		// Server-side apply keeps the generated object exactly as compiled; a manual edit is overwritten.
		if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
			logger.Error(err, "apply generated object", "kind", kind, "name", name)
			if owner == "" {
				return reconcile.Result{}, err
			}
			conds[owner] = cond(metav1.ConditionFalse, "ApplyFailed", err.Error())
			applyFailed = true
			continue
		}
		switch kind {
		case "AgentgatewayModel":
			conds[owner] = acceptedCondition(current)
		case "MCPServer":
			conds[owner] = mcpServerCondition(current)
		}
	}
	// Objects of refused Servers: no general prune exists, and owner references only collect on Server deletion.
	for name := range refusedServers {
		if err := r.deleteGenerated(ctx, "AgentgatewayPolicy", "ambit-conn-"+name); err != nil {
			return reconcile.Result{}, err
		}
	}
	if len(servers.Items) > 0 { // every Server refused, or none running: no shared backend (or policy) either
		for _, o := range [][2]string{{"AgentgatewayBackend", MCPBackendName}, {"AgentgatewayPolicy", MCPBackendName + "-access"}} {
			if generates(res.Objects, o[0], o[1]) {
				continue
			}
			if err := r.deleteGenerated(ctx, o[0], o[1]); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	// No budgets (or budgets off): stop asking the ratelimit service on every request. No dollar limit: stop
	// rewriting requests for usage.
	for name, wanted := range map[string]bool{BudgetPolicyName: budgetPolicy, UsagePolicyName: usage, ClassifyPolicyName: classify} {
		if wanted {
			continue
		}
		stale := &unstructured.Unstructured{}
		stale.SetGroupVersionKind(AgentgatewayPolicyGVK)
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, stale); err == nil && ownedByAmbit(stale) {
			if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
				return reconcile.Result{}, err
			}
		}
	}
	if dep != nil {
		if !classify { // the policy is gone (above), then the pods; they finish held requests on SIGTERM
			if err := r.scale(ctx, dep, 0); err != nil {
				return reconcile.Result{}, err
			}
		}
		cred, err := credentialsObject(r.Namespace, routerCfg, keys)
		if err != nil {
			return reconcile.Result{}, err
		}
		if err := r.applyCredentials(ctx, cred); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Virtual models of routers that are gone or now invalid. Read from the API server, not the cache: right after
	// the apply loop the cache can still show a router's label on an object that a Model has just taken over (a
	// Model named router-<name>).
	desiredRouters := map[string]bool{}
	for _, o := range res.Objects {
		if o.GetLabels()[RouterLabel] != "" {
			desiredRouters[o.GetName()] = true
		}
	}
	var vms unstructured.UnstructuredList
	vms.SetGroupVersionKind(AgentgatewayModelGVK)
	if err := r.Live.List(ctx, &vms, client.InNamespace(r.Namespace), client.HasLabels{RouterLabel}); err != nil {
		return reconcile.Result{}, err
	}
	for i := range vms.Items {
		vm := &vms.Items[i]
		if desiredRouters[vm.GetName()] || controllerOwnerKind(vm) != "ModelRouter" {
			continue
		}
		uid := vm.GetUID() // delete exactly this object, not one that replaced it meanwhile
		if err := r.Delete(ctx, vm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return reconcile.Result{}, err
		}
	}
	// Look again while agentgateway or ToolHive has not decided yet: their status events can race
	// with our own apply (seen when the route's listener changed), and a missed one would leave
	// a Model or Server Pending for good.
	result := reconcile.Result{}
	for _, c := range conds {
		if c.Status == metav1.ConditionUnknown {
			result.RequeueAfter = 5 * time.Second
		}
	}
	// A failed apply is retried on its own; otherwise it waits for an unrelated change, and meanwhile the
	// gateway keeps the old object (e.g. without the refusal of an unpriced model to capped groups).
	if applyFailed && result.RequeueAfter == 0 {
		result.RequeueAfter = 10 * time.Second
	}

	for i := range models.Items {
		m := &models.Items[i]
		a := res.Access[m.Name]
		next := m.DeepCopy()
		next.Status.AllowedGroups, next.Status.AllowAllSignedIn = a.Groups, a.AllSignedIn
		c := conds["Model/"+m.Name]
		if gs := refused[m.Name]; len(gs) > 0 && c.Status == metav1.ConditionTrue {
			c.Message = strings.TrimPrefix(c.Message+"; "+unpricedMessage(gs), "; ")
		}
		setReady(&next.Status.Conditions, c, m.Generation)
		if err := r.updateStatus(ctx, m.Status, next.Status, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	for i := range servers.Items {
		s := &servers.Items[i]
		next := s.DeepCopy()
		c, ok := conds["Server/"+s.Name]
		if !ok && s.Spec.URL != "" { // remote: no ToolHive object to report on
			c = cond(metav1.ConditionTrue, "Remote", "served from "+s.Spec.URL+" through the gateway")
		}
		if msg, ok := refusedServers[s.Name]; ok {
			c = cond(metav1.ConditionFalse, "ConnectionBound", msg)
		}
		setReady(&next.Status.Conditions, c, s.Generation)
		admitted := s.Spec.URL != "" || meta.IsStatusConditionTrue(s.Status.Conditions, "Serving")
		if c.Status == metav1.ConditionTrue && r.ListTools != nil && admitted { // admission reads the others' tools
			var tc *metav1.Condition
			next.Status.Tools, tc = r.readTools(ctx, *s)
			if tc != nil {
				tc.ObservedGeneration = s.Generation
				meta.SetStatusCondition(&next.Status.Conditions, *tc)
			} else {
				meta.RemoveStatusCondition(&next.Status.Conditions, "ToolsRead")
			}
			if result.RequeueAfter == 0 {
				result.RequeueAfter = r.refresh()
			}
			pinTools(next)
		} else {
			meta.RemoveStatusCondition(&next.Status.Conditions, "ToolsRead")
		}
		if err := r.updateStatus(ctx, saved.Items[i].Status, next.Status, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	if admitRetry > 0 && (result.RequeueAfter == 0 || result.RequeueAfter > admitRetry) {
		result.RequeueAfter = admitRetry // a running server that does not answer yet: try it again soon
	}
	first := map[string]string{} // group -> the Budget that governs it (first by name)
	sort.Slice(budgets.Items, func(i, j int) bool { return budgets.Items[i].Name < budgets.Items[j].Name })
	for i := range budgets.Items {
		b := &budgets.Items[i]
		next := b.DeepCopy()
		c := cond(metav1.ConditionTrue, "Enforced", "shared across every gateway replica")
		switch {
		case r.RateLimit.Service == "":
			c = cond(metav1.ConditionFalse, "LimitsOff", "budgets are off in this install (limits.enabled=false)")
		case first[b.Spec.Group] != "":
			c = cond(metav1.ConditionFalse, "DuplicateGroup", "Budget "+first[b.Spec.Group]+" already covers group "+b.Spec.Group+"; this one is ignored")
		default:
			first[b.Spec.Group] = b.Name
		}
		setReady(&next.Status.Conditions, c, b.Generation)
		if err := r.updateStatus(ctx, b.Status, next.Status, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	if classify && (result.RequeueAfter == 0 || result.RequeueAfter > time.Minute) {
		// ponytail: re-reads the classifier Models' Secrets every minute instead of watching all Secrets; also
		// notices a start that never finishes.
		result.RequeueAfter = time.Minute
	}
	classified := map[string]bool{}
	for _, rt := range routerCfg.Routers {
		classified[rt.Name] = true
	}
	for i := range routers.Items {
		mr := &routers.Items[i]
		next := mr.DeepCopy()
		c := routerConds[mr.Name]
		next.Status.ClassifierState = ""
		if classified[mr.Name] {
			var since time.Time
			if cr := meta.FindStatusCondition(mr.Status.Conditions, "ClassifierReady"); cr != nil && cr.Status == metav1.ConditionFalse && cr.Reason == "Starting" {
				since = cr.LastTransitionTime.Time
			}
			state, cc := classifierState(dep, true, since, r.now())
			next.Status.ClassifierState = state
			ready := metav1.Condition{Type: "ClassifierReady", Status: metav1.ConditionTrue, Reason: "Ready", Message: "ambit-router is available",
				ObservedGeneration: mr.Generation, LastTransitionTime: metav1.NewTime(r.now())}
			if state != "Ready" {
				ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, state, "ambit-router is "+strings.ToLower(state)
			}
			meta.SetStatusCondition(&next.Status.Conditions, ready) // keeps the time unless the status flips
			if cc.Reason != "" {
				if c.Status == metav1.ConditionTrue {
					c = cc
				} else {
					c.Message += "; " + cc.Message
				}
			}
		} else {
			meta.RemoveStatusCondition(&next.Status.Conditions, "ClassifierReady")
		}
		// The gateway's verdict replaces "Compiled"; a validation warning (UnknownModel, DefaultNotAllowed) stays
		// unless applying failed.
		if applied, ok := conds["ModelRouter/"+mr.Name]; ok &&
			(c.Status == metav1.ConditionTrue || applied.Reason == "NameConflict" || applied.Reason == "ApplyFailed") {
			c = applied
		}
		setReady(&next.Status.Conditions, c, mr.Generation)
		if err := r.updateStatus(ctx, mr.Status, next.Status, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	for i := range rules.Items {
		ar := &rules.Items[i]
		next := ar.DeepCopy()
		meta.SetStatusCondition(&next.Status.Conditions, res.RuleConditions[ar.Name])
		if err := r.updateStatus(ctx, ar.Status, next.Status, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	return result, nil
}

// readTools returns the server's tools, reading them again when they are older than toolsRefresh.
// On failure it keeps the last known list: a slow server must not make its tools vanish from the console.
// For per-user servers it also returns the ToolsRead condition; nil means the condition does not apply.
func (r *Reconciler) readTools(ctx context.Context, s v1.Server) ([]v1.Tool, *metav1.Condition) {
	if r.toolsRead == nil {
		r.toolsRead = map[types.UID]time.Time{}
	}
	if t, ok := r.toolsRead[s.UID]; ok && time.Since(t) < r.refresh() && len(s.Status.Tools) > 0 {
		return s.Status.Tools, meta.FindStatusCondition(s.Status.Conditions, "ToolsRead").DeepCopy()
	}
	// A server read directly that could not be read is asked again only after toolsRetry: each try can wait up to
	// the read timeout, and a down vendor must not make every pass (and every access change) slow. Per-user
	// servers are read through the console, which answers at once.
	if t, ok := r.toolsFailed[s.UID]; ok && s.Spec.Connection == "" && r.now().Before(t.Add(toolsRetry)) {
		return s.Status.Tools, meta.FindStatusCondition(s.Status.Conditions, "ToolsRead").DeepCopy()
	}
	tools, err := r.ListTools(ctx, s)
	if r.toolsFailed == nil {
		r.toolsFailed = map[types.UID]time.Time{}
	}
	if err != nil {
		r.toolsFailed[s.UID] = r.now()
	} else {
		delete(r.toolsFailed, s.UID)
	}
	if errors.Is(err, errToolsNotRead) {
		return s.Status.Tools, nil
	}
	if errors.Is(err, ErrNoReader) {
		c := cond(metav1.ConditionFalse, "ReaderUnavailable", "no admin's account is set to read this server's tools; choose one on the Servers page")
		c.Type = "ToolsRead"
		return s.Status.Tools, &c
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "read tools", "server", s.Name)
		if s.Spec.Connection == "" {
			return s.Status.Tools, nil
		}
		c := cond(metav1.ConditionFalse, "ReadFailed", "the last attempt to read this server's tools failed; the list shown is from an earlier read")
		c.Type = "ToolsRead"
		return s.Status.Tools, &c
	}
	r.toolsRead[s.UID] = time.Now()
	if s.Spec.Connection == "" {
		return tools, nil
	}
	c := cond(metav1.ConditionTrue, "Read", fmt.Sprintf("%d tools", len(tools)))
	c.Type = "ToolsRead"
	return tools, &c
}

func setReady(conds *[]metav1.Condition, c metav1.Condition, generation int64) {
	c.ObservedGeneration = generation
	meta.SetStatusCondition(conds, c)
}

// updateStatus writes only when something changed, so a pass with no news writes nothing.
func (r *Reconciler) updateStatus(ctx context.Context, before, after any, obj client.Object) error {
	if equality.Semantic.DeepEqual(before, after) {
		return nil
	}
	if c := meta.FindStatusCondition(conditionsOf(obj), "Ready"); c != nil {
		log.FromContext(ctx).Info("status", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName(), "ready", c.Status, "reason", c.Reason)
	}
	return r.Status().Update(ctx, obj)
}

func conditionsOf(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *v1.Model:
		return o.Status.Conditions
	case *v1.Server:
		return o.Status.Conditions
	case *v1.AccessRule:
		return o.Status.Conditions
	case *v1.ModelRouter:
		return o.Status.Conditions
	}
	return nil
}

// controllerOwner returns "Kind/name" of the Ambit object that controls o, or "" if none does.
func controllerOwner(o *unstructured.Unstructured) string {
	for _, ref := range o.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + "/" + ref.Name
		}
	}
	return ""
}

func ownedByAmbit(o *unstructured.Unstructured) bool {
	if o.GetLabels()["app.kubernetes.io/managed-by"] == "ambit" {
		return true
	}
	return slices.ContainsFunc(o.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ref.APIVersion == v1.GroupVersion.String()
	})
}

func cond(s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: "Ready", Status: s, Reason: reason, Message: msg}
}

// A running server that does not answer its first MCP introduction is asked again after admissionRetry, doubling
// up to admissionRetryMax, so a broken server is neither asked every few seconds forever nor forgotten.
const (
	admissionRetry    = 3 * time.Second
	admissionRetryMax = time.Minute
	admissionTimeout  = 3 * time.Second
)

// markRunning sets each image Server's Ready condition, in memory, from ToolHive's live verdict on its MCPServer,
// before compiling. A running server that has not answered yet is asked once (its tools, through the same proxy
// Service the gateway uses); when it answers, Serving becomes true and stays true. Only Serving image servers join
// the shared backend (compileServers): agentgateway introduces every new session to every target, so a booting
// server would make every user wait, and any later change to the target list breaks resuming open sessions on the
// other gateway replica, so a restart does not remove it. Servers are asked in parallel, each for at most admissionTimeout, so a broken one cannot hold up the
// pass. It returns when to try the servers that have not answered yet (0: none).
func (r *Reconciler) markRunning(ctx context.Context, servers []v1.Server) (time.Duration, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(MCPServerGVK.GroupVersion().WithKind(MCPServerGVK.Kind + "List"))
	if err := r.Live.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil && !meta.IsNoMatchError(err) {
		return 0, err
	}
	verdict := map[string]metav1.Condition{}
	owner := map[string]types.UID{}
	for i := range list.Items {
		o := &list.Items[i]
		if o.GetDeletionTimestamp() != nil { // a deleted Server's object, still being removed: its pod is not ours
			continue
		}
		verdict[o.GetName()] = mcpServerCondition(o)
		for _, ref := range o.GetOwnerReferences() {
			if ref.Kind == "Server" {
				owner[o.GetName()] = ref.UID
			}
		}
	}
	now := r.now()
	var ask []int
	var retry time.Duration
	for i := range servers {
		s := &servers[i]
		if s.Spec.URL != "" {
			continue
		}
		c, ok := verdict[s.Name]
		if ok && owner[s.Name] != s.UID { // left by an earlier Server of the same name
			ok = false
		}
		if !ok {
			c = cond(metav1.ConditionUnknown, "Pending", "waiting for ToolHive to start the server")
		}
		c.ObservedGeneration = s.Generation // as setReady writes it: no status write on an idle pass
		meta.SetStatusCondition(&s.Status.Conditions, c)
		switch {
		case meta.IsStatusConditionTrue(s.Status.Conditions, "Serving"):
		case c.Status != metav1.ConditionTrue:
		case r.ListTools == nil:
			setServing(s, "Running", "running in ToolHive")
		case now.Before(r.admitAt[s.UID]):
			if w := r.admitAt[s.UID].Sub(now); retry == 0 || w < retry {
				retry = w
			}
		default:
			ask = append(ask, i)
		}
	}
	type answer struct {
		tools []v1.Tool
		err   error
	}
	answers := make([]answer, len(ask))
	var wg sync.WaitGroup
	for k, i := range ask {
		wg.Add(1)
		go func(k int, s v1.Server) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, admissionTimeout)
			defer cancel()
			answers[k].tools, answers[k].err = r.ListTools(actx, s)
		}(k, servers[i])
	}
	wg.Wait()
	if r.admitAt == nil {
		r.admitAt, r.admitWait = map[types.UID]time.Time{}, map[types.UID]time.Duration{}
	}
	for k, i := range ask {
		s := &servers[i]
		if err := answers[k].err; err != nil {
			w := min(max(2*r.admitWait[s.UID], admissionRetry), admissionRetryMax)
			r.admitWait[s.UID], r.admitAt[s.UID] = w, now.Add(w)
			if retry == 0 || w < retry {
				retry = w
			}
			log.FromContext(ctx).Info("server does not answer its MCP introduction yet", "server", s.Name, "retryIn", w.String(), "err", err.Error())
			continue
		}
		delete(r.admitAt, s.UID)
		delete(r.admitWait, s.UID)
		s.Status.Tools = answers[k].tools
		if r.toolsRead == nil {
			r.toolsRead = map[types.UID]time.Time{}
		}
		r.toolsRead[s.UID] = time.Now()
		setServing(s, "Answered", "answered its first MCP introduction")
	}
	return retry, nil
}

func setServing(s *v1.Server, reason, msg string) {
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: "Serving", Status: metav1.ConditionTrue,
		Reason: reason, Message: msg + "; listed for every session from now on", ObservedGeneration: s.Generation})
}

// mcpServerCondition mirrors ToolHive's verdict on the generated MCPServer into the Server.
func mcpServerCondition(o *unstructured.Unstructured) metav1.Condition {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		cm := c.(map[string]any)
		if cm["type"] != "Ready" {
			continue
		}
		if cm["status"] == "True" {
			return cond(metav1.ConditionTrue, "Running", "running in ToolHive and reachable through the gateway")
		}
		return cond(metav1.ConditionFalse, "NotRunning", fmt.Sprint(cm["message"]))
	}
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	return cond(metav1.ConditionUnknown, "Pending", "waiting for ToolHive to start the server (phase "+phase+")")
}

// acceptedCondition mirrors agentgateway's verdict on the generated object into the Model.
func acceptedCondition(o *unstructured.Unstructured) metav1.Condition {
	parents, _, _ := unstructured.NestedSlice(o.Object, "status", "parents")
	for _, p := range parents {
		conds, _, _ := unstructured.NestedSlice(p.(map[string]any), "conditions")
		for _, c := range conds {
			cm := c.(map[string]any)
			if cm["type"] != "Accepted" {
				continue
			}
			if cm["status"] == "True" {
				return cond(metav1.ConditionTrue, "Accepted", "served by the gateway")
			}
			return cond(metav1.ConditionFalse, "NotAccepted", fmt.Sprint(cm["message"]))
		}
	}
	return cond(metav1.ConditionUnknown, "Pending",
		"waiting for agentgateway to accept the model; if this lasts, check agentgateway.agentgatewayModels.enabled")
}

// controllerOwnerKind is the Kind of the Ambit object that controls o, or "".
func controllerOwnerKind(o *unstructured.Unstructured) string {
	return strings.Split(controllerOwner(o), "/")[0]
}

// deleteGenerated removes one object Ambit generated, if present and owned by Ambit.
func (r *Reconciler) deleteGenerated(ctx context.Context, kind, name string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: kind})
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, u); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedByAmbit(u) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, u))
}

func generates(objs []*unstructured.Unstructured, kind, name string) bool {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return true
		}
	}
	return false
}
