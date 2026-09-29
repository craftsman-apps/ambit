package controller

import (
	"context"
	"fmt"
	"slices"

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

var AgentgatewayModelGVK = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayModel"}

const fieldOwner = client.FieldOwner("ambit-controller")

// Reconciler recomputes the whole namespace on every change. ponytail: fine for hundreds of
// models and rules; switch to per-model keys if a namespace ever holds thousands.
type Reconciler struct {
	client.Client
	Namespace string
	Route     RouteRef
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "all"}}}
	})
	generated := &unstructured.Unstructured{}
	generated.SetGroupVersionKind(AgentgatewayModelGVK)
	return ctrl.NewControllerManagedBy(mgr).Named("ambit").
		// Only spec changes of Ambit objects: our own status writes must not trigger another pass.
		Watches(&v1.Model{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1.AccessRule{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Generated objects: status changes (agentgateway accepting them) and manual edits both matter.
		Watches(generated, all).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	var models v1.ModelList
	var rules v1.AccessRuleList
	if err := r.List(ctx, &models, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.List(ctx, &rules, client.InNamespace(r.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	res := Compile(models.Items, rules.Items, r.Route)

	modelCond := map[string]metav1.Condition{}
	for _, desired := range res.Objects {
		name := desired.GetName()
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(AgentgatewayModelGVK)
		err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, current)
		switch {
		case err == nil && !ownedByAmbit(current):
			modelCond[name] = cond(metav1.ConditionFalse, "NameConflict",
				fmt.Sprintf("an AgentgatewayModel named %q already exists and was not created by Ambit; rename the Model", name))
			continue
		case err != nil && !apierrors.IsNotFound(err):
			return reconcile.Result{}, err
		}
		// Server-side apply keeps the generated object exactly as compiled; a manual edit is overwritten.
		if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
			modelCond[name] = cond(metav1.ConditionFalse, "ApplyFailed", err.Error())
			logger.Error(err, "apply AgentgatewayModel", "model", name)
			continue
		}
		modelCond[name] = acceptedCondition(current)
	}

	for i := range models.Items {
		m := &models.Items[i]
		a := res.Access[m.Name]
		next := m.DeepCopy()
		next.Status.AllowedGroups, next.Status.AllowAllSignedIn = a.Groups, a.AllSignedIn
		c := modelCond[m.Name]
		c.ObservedGeneration = m.Generation
		meta.SetStatusCondition(&next.Status.Conditions, c)
		if !equality.Semantic.DeepEqual(m.Status, next.Status) {
			if err := r.Status().Update(ctx, next); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	for i := range rules.Items {
		ar := &rules.Items[i]
		next := ar.DeepCopy()
		meta.SetStatusCondition(&next.Status.Conditions, res.RuleConditions[ar.Name])
		if !equality.Semantic.DeepEqual(ar.Status, next.Status) {
			if err := r.Status().Update(ctx, next); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func ownedByAmbit(o *unstructured.Unstructured) bool {
	return slices.ContainsFunc(o.GetOwnerReferences(), func(ref metav1.OwnerReference) bool {
		return ref.APIVersion == v1.GroupVersion.String() && ref.Kind == "Model"
	})
}

func cond(s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: "Ready", Status: s, Reason: reason, Message: msg}
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
