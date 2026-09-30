package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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

	toolsRead map[types.UID]time.Time // when each Server's tools were last read (single worker, no lock)
}

// How often the tools of a running server are read again: servers may add or change tools.
const toolsRefresh = 10 * time.Minute

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
		Watches(&v1.AccessRule{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
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
	res := Compile(models.Items, servers.Items, rules.Items, r.Route)
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
			continue
		}
		switch kind {
		case "AgentgatewayModel":
			conds[owner] = acceptedCondition(current)
		case "MCPServer":
			conds[owner] = mcpServerCondition(current)
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

	for i := range models.Items {
		m := &models.Items[i]
		a := res.Access[m.Name]
		next := m.DeepCopy()
		next.Status.AllowedGroups, next.Status.AllowAllSignedIn = a.Groups, a.AllSignedIn
		setReady(&next.Status.Conditions, conds["Model/"+m.Name], m.Generation)
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
		setReady(&next.Status.Conditions, c, s.Generation)
		if c.Status == metav1.ConditionTrue && r.ListTools != nil {
			next.Status.Tools = r.readTools(ctx, *s)
			if result.RequeueAfter == 0 {
				result.RequeueAfter = toolsRefresh
			}
		}
		if err := r.updateStatus(ctx, s.Status, next.Status, next); err != nil {
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
func (r *Reconciler) readTools(ctx context.Context, s v1.Server) []v1.Tool {
	if r.toolsRead == nil {
		r.toolsRead = map[types.UID]time.Time{}
	}
	if t, ok := r.toolsRead[s.UID]; ok && time.Since(t) < toolsRefresh && len(s.Status.Tools) > 0 {
		return s.Status.Tools
	}
	tools, err := r.ListTools(ctx, s)
	if err != nil {
		log.FromContext(ctx).Error(err, "read tools", "server", s.Name)
		return s.Status.Tools
	}
	r.toolsRead[s.UID] = time.Now()
	return tools
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
