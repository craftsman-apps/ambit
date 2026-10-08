package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/routerconfig"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The classifier switch keeps no state: every reconcile scales ambit-router to the wanted replicas and applies the
// ExtProc policy, or deletes the policy and scales to 0. FailOpen makes a missing pod harmless, and ambit-router
// finishes its requests on SIGTERM (spec rev 5, "Turning a classifier on and off at run time").
type Classifier struct {
	Deployment string // ambit-router's Deployment; empty: not installed
	Replicas   int32
}

const ClassifyPolicyName = "ambit-llm-classify"

// startTimeout: a classifier not available after this long is reported as ClassifierStartFailed.
const startTimeout = 15 * time.Minute

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// routerDeployment reads ambit-router's Deployment from the API server; nil when it is not installed.
func (r *Reconciler) routerDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	if r.Classifier.Deployment == "" {
		return nil, nil
	}
	var d appsv1.Deployment
	switch err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Classifier.Deployment}, &d); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &d, nil
}

// readKeys reads the Authorization entry of each type-LLM router's classifier Model Secret through the API reader
// (never the cache: the controller may get Secrets by name but not list them). A missing Secret or entry leaves the
// Model out; other errors are returned so the reconcile is retried.
func (r *Reconciler) readKeys(ctx context.Context, routers []v1.ModelRouter, models []v1.Model) (map[string]string, error) {
	byObject := map[string]v1.Model{}
	for _, m := range models {
		byObject[m.Name] = m
	}
	keys := map[string]string{}
	for _, mr := range routers {
		if mr.Spec.Classifier.Type != "LLM" || mr.Spec.Classifier.LLM == nil {
			continue
		}
		m, ok := byObject[mr.Spec.Classifier.LLM.Model]
		if !ok {
			continue
		}
		if _, callable := LLMEndpoint(m); !callable || m.Spec.CredentialsRef == nil {
			continue // llmConfig refuses this classifier; its Secret (AWS keys, Azure key, or none) is not ambit-router's
		}
		if _, done := keys[m.Name]; done {
			continue
		}
		var s corev1.Secret
		switch err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: m.Spec.CredentialsRef.Name}, &s); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, err
		}
		if v, ok := s.Data["Authorization"]; ok {
			keys[m.Name] = string(v)
		}
	}
	return keys, nil
}

func hasKeys(keys map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range keys {
		out[k] = true
	}
	return out
}

func configMapObject(ns string, cfg routerconfig.Config) *unstructured.Unstructured {
	b, _ := json.Marshal(cfg)
	o := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{routerconfig.ConfigKey: string(b)}}}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetName(routerconfig.ConfigMapName)
	o.SetNamespace(ns)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit"})
	return o
}

// credentialsObject holds only the keys the configuration names.
func credentialsObject(ns string, cfg routerconfig.Config, keys map[string]string) (*unstructured.Unstructured, error) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: routerconfig.SecretName, Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "ambit"}}, Data: map[string][]byte{}}
	for _, rt := range cfg.Routers {
		if rt.LLM != nil {
			s.Data[rt.LLM.CredentialKey] = []byte(keys[rt.LLM.CredentialKey])
		}
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
	if err != nil {
		return nil, err
	}
	o := &unstructured.Unstructured{Object: m}
	o.SetAPIVersion("v1")
	o.SetKind("Secret")
	unstructured.RemoveNestedField(o.Object, "metadata", "creationTimestamp")
	return o, nil
}

// applyCredentials writes the credentials Secret without the cache (see readKeys). A Secret of that name not
// created by Ambit is left alone.
func (r *Reconciler) applyCredentials(ctx context.Context, o *unstructured.Unstructured) error {
	var cur corev1.Secret
	switch err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: o.GetName()}, &cur); {
	case err == nil && cur.Labels["app.kubernetes.io/managed-by"] != "ambit":
		return fmt.Errorf("a Secret named %s already exists and was not created by Ambit", o.GetName())
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	return r.Patch(ctx, o, client.Apply, fieldOwner, client.ForceOwnership)
}

// classifyPolicy sends chat completions for the type-LLM routers to ambit-router (spec, "The ExtProc policy").
// The condition reads the requested model, so other chat requests skip the round trip (it cost +32 % gateway CPU
// on direct chat, measured 2026-10-07; the gateway buffers the body for model routing anyway). The hash is a CEL
// string literal: ambit-router labels only under the configuration this policy names.
// classifyMaxBody: larger bodies skip ambit-router and get no label. agentgateway v1.6.0 buffers up to 32 MiB, and
// ambit-router's gRPC server takes at most 4 MiB (4,194,304 bytes) per message, headers included; a larger body
// would fail the request with 500 once it is sent (FailOpen ends when the body goes out).
const classifyMaxBody = 4000000

func classifyPolicy(ns, route, service string, cfg routerconfig.Config) *unstructured.Unstructured {
	var wires []string
	for _, r := range cfg.Routers {
		wires = append(wires, strconv.Quote(r.Wire))
	}
	condition := routedPath + " && size(request.body) <= " + strconv.Itoa(classifyMaxBody) +
		" && json(request.body).model in [" + strings.Join(wires, ", ") + "]"
	hash := cfg.Hash
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route}},
		"traffic": map[string]any{
			"phase": "PostRouting",
			"extProc": map[string]any{"conditional": []any{map[string]any{
				"condition": condition,
				"policy": map[string]any{
					"backendRef":        map[string]any{"name": service, "port": int64(9000)},
					"failureMode":       "FailOpen",
					"requestAttributes": map[string]any{"ambit.groups": "jwt.groups", "ambit.config": "'" + hash + "'"},
					"processingOptions": map[string]any{
						"requestHeaderMode": "Send", "requestBodyMode": "None", "allowModeOverride": true,
						"responseHeaderMode": "Skip", "responseBodyMode": "None",
						"requestTrailerMode": "Skip", "responseTrailerMode": "Skip",
					},
				},
			}}},
		},
	}}}
	o.SetGroupVersionKind(AgentgatewayPolicyGVK)
	o.SetName(ClassifyPolicyName)
	o.SetNamespace(ns)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit"})
	return o
}

// scale sets ambit-router's replicas when they differ; the Deployment has no replicas in the chart.
func (r *Reconciler) scale(ctx context.Context, d *appsv1.Deployment, replicas int32) error {
	if d.Spec.Replicas != nil && *d.Spec.Replicas == replicas {
		return nil
	}
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	return r.Patch(ctx, d, client.RawPatch(types.MergePatchType, []byte(patch)))
}

// classifierState is read from the Deployment. since is when the router's ClassifierReady condition last turned
// False while Starting (zero when unknown).
func classifierState(d *appsv1.Deployment, wanted bool, since, now time.Time) (string, metav1.Condition) {
	switch {
	case d == nil && wanted:
		return "Off", cond(metav1.ConditionFalse, "ClassifierNotInstalled", "ambit-router is not installed (routing.router.enabled=false)")
	case d == nil || !wanted:
		return "Off", metav1.Condition{}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	switch avail := d.Status.AvailableReplicas; {
	case avail == 0:
		if !since.IsZero() && now.Sub(since) > startTimeout {
			return "Starting", cond(metav1.ConditionFalse, "ClassifierStartFailed",
				"ambit-router is not available after 15 minutes; see kubectl describe deployment "+d.Name+"; requests get no label")
		}
		return "Starting", metav1.Condition{}
	case avail < want:
		return "Unhealthy", cond(metav1.ConditionFalse, "ClassifierUnhealthy",
			fmt.Sprintf("%d of %d ambit-router replicas available; requests may get no label", avail, want))
	}
	return "Ready", metav1.Condition{}
}
