package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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
	Namespace string
	Route     RouteRef
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "all"}}}
	})
	b := ctrl.NewControllerManagedBy(mgr).Named("ambit").
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
		if err := r.List(ctx, l, client.InNamespace(r.Namespace)); err != nil {
			return reconcile.Result{}, err
		}
	}
	res := Compile(models.Items, servers.Items, rules.Items, r.Route)

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
		setReady(&next.Status.Conditions, conds["Server/"+s.Name], s.Generation)
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
	return reconcile.Result{}, nil
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
	return r.Status().Update(ctx, obj)
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
