package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

// The access log. agentgateway uses only the first access-log policy of a Gateway (store/binds.rs), so the
// controller writes the one policy, under the name the chart used before, from the chart's static part.
const (
	AccessLogBase       = "ambit-access-log-base" // chart: spec.yaml holds the policy's spec without the generated fields
	AccessLogPolicy     = "ambit-access-log"
	AccessModeConfigMap = "ambit-access-mode" // the published mode and rules version, for the console and ambit check
	accessLogManager    = "ambit-controller"  // app.kubernetes.io/managed-by of the policy: not Helm's any more
)

// accessLogPolicy is the base spec with ambit.accessMode and ambit.rulesVersion appended, owned by the base
// ConfigMap: evidence turned off removes the ConfigMap, and with it the policy.
func accessLogPolicy(base *corev1.ConfigMap, mode, rulesVersion string) (*unstructured.Unstructured, error) {
	var spec map[string]any
	if err := yaml.Unmarshal([]byte(base.Data["spec.yaml"]), &spec); err != nil {
		return nil, fmt.Errorf("%s: spec.yaml: %w", AccessLogBase, err)
	}
	add, ok, _ := unstructured.NestedSlice(spec, "frontend", "accessLog", "attributes", "add")
	if !ok {
		return nil, fmt.Errorf("%s: spec.yaml has no frontend.accessLog.attributes.add", AccessLogBase)
	}
	add = append(add,
		map[string]any{"name": "ambit.accessMode", "expression": strconv.Quote(mode)},
		map[string]any{"name": "ambit.rulesVersion", "expression": strconv.Quote(rulesVersion)})
	if err := unstructured.SetNestedSlice(spec, add, "frontend", "accessLog", "attributes", "add"); err != nil {
		return nil, err
	}
	p := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", AccessLogPolicy, base.Namespace, spec)
	p.SetLabels(map[string]string{"app.kubernetes.io/managed-by": accessLogManager})
	p.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: base.Name, UID: base.UID}})
	return p, nil
}

// RulesVersion is the first 12 hex digits of a sha256 over every AccessRule decision the gateway makes: each
// AgentgatewayModel's authorization (router lines included) and the MCP access policy's. Order does not matter.
func RulesVersion(objs []*unstructured.Unstructured) string {
	auth := map[string]any{}
	for _, o := range objs {
		var path []string
		switch {
		case o.GetKind() == "AgentgatewayModel":
			path = []string{"spec", "policies", "authorization"}
		case o.GetKind() == "AgentgatewayPolicy" && o.GetName() == MCPBackendName+"-access":
			path = []string{"spec", "backend", "mcp", "authorization"}
		default:
			continue
		}
		a, _, _ := unstructured.NestedFieldNoCopy(o.Object, path...)
		auth[o.GetKind()+"/"+o.GetName()] = a
	}
	b, _ := json.Marshal(auth) // map keys are sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

func (r *Reconciler) accessMode() string {
	if r.AccessMode == "" {
		return "enforce"
	}
	return r.AccessMode
}

// writeAccessLog writes ambit-access-log from the base ConfigMap, also over the chart's object of an older
// version (Helm deletes that one after an upgrade; the watch then writes it again). No base: no policy.
func (r *Reconciler) writeAccessLog(ctx context.Context, mode, rulesVersion string) ([]gated, error) {
	var base corev1.ConfigMap
	if err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: AccessLogBase}, &base); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	p, err := accessLogPolicy(&base, mode, rulesVersion)
	if err != nil {
		return nil, err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(AgentgatewayPolicyGVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.Patch(ctx, p, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return nil, err
	}
	return []gated{{kind: p.GetKind(), name: p.GetName(), generation: p.GetGeneration(), changed: p.GetGeneration() != current.GetGeneration()}}, nil
}

// publishMode writes ambit-access-mode: the mode and the rules version in force, each with the time it last
// changed, once the change is active (no pending entry).
func (r *Reconciler) publishMode(ctx context.Context, mode, rulesVersion string) error {
	now := r.now().UTC().Format(time.RFC3339)
	return r.updateMode(ctx, func(d map[string]string) {
		if d["mode"] != mode {
			d["mode"], d["modeSince"] = mode, now
		}
		if d["rulesVersion"] != rulesVersion {
			d["rulesVersion"], d["rulesSince"] = rulesVersion, now
		}
		delete(d, "pending")
	})
}

// updateMode changes ambit-access-mode's data, writing only when something changed.
func (r *Reconciler) updateMode(ctx context.Context, change func(map[string]string)) error {
	var cm corev1.ConfigMap
	err := r.Live.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: AccessModeConfigMap}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	next := maps.Clone(cm.Data)
	if next == nil {
		next = map[string]string{}
	}
	change(next)
	if exists && maps.Equal(cm.Data, next) {
		return nil
	}
	cm.Data = next
	if !exists {
		cm.Name, cm.Namespace = AccessModeConfigMap, r.Namespace
		cm.Labels = map[string]string{"app.kubernetes.io/managed-by": accessLogManager}
		return r.Create(ctx, &cm)
	}
	return r.Update(ctx, &cm)
}

// AccessLogStartup writes ambit-access-log once at start, before the controller reports ready, on every replica
// (not only the leader: a new pod must be ready before the old leader stops). It writes only a missing policy or
// the chart's (an upgrade from a version whose chart rendered it), with ambit.accessMode "enforce": only the
// leader relaxes authorization, after its own access-log write. A policy the controller already wrote belongs to
// the leader, which may be in the other mode during a rolling update, so it is left alone.
type AccessLogStartup struct {
	Client    client.Client
	Namespace string
	done      atomic.Bool
}

// Ready is the readiness check.
func (s *AccessLogStartup) Ready(*http.Request) error {
	if !s.done.Load() {
		return errors.New("access-log policy not written yet")
	}
	return nil
}

// NeedLeaderElection: every replica runs it.
func (s *AccessLogStartup) NeedLeaderElection() bool { return false }

// Start tries until it succeeds or ctx ends.
func (s *AccessLogStartup) Start(ctx context.Context) error {
	for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
		err := s.ensure(ctx)
		if err == nil {
			s.done.Store(true)
			return nil
		}
		log.FromContext(ctx).Error(err, "write the access-log policy at start", "retryIn", wait.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (s *AccessLogStartup) ensure(ctx context.Context) error {
	var base corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: AccessLogBase}, &base); err != nil {
		return client.IgnoreNotFound(err)
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(AgentgatewayPolicyGVK)
	switch err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: AccessLogPolicy}, current); {
	case err == nil && current.GetLabels()["app.kubernetes.io/managed-by"] == accessLogManager:
		return nil
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	// The rules in force: the generated objects as they are now.
	var models unstructured.UnstructuredList
	models.SetGroupVersionKind(AgentgatewayModelGVK)
	if err := s.Client.List(ctx, &models, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	var live []*unstructured.Unstructured
	for i := range models.Items {
		if ownedByAmbit(&models.Items[i]) {
			live = append(live, &models.Items[i])
		}
	}
	mcp := &unstructured.Unstructured{}
	mcp.SetGroupVersionKind(AgentgatewayPolicyGVK)
	switch err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: MCPBackendName + "-access"}, mcp); {
	case err == nil && ownedByAmbit(mcp):
		live = append(live, mcp)
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	p, err := accessLogPolicy(&base, "enforce", RulesVersion(live))
	if err != nil {
		return err
	}
	return s.Client.Patch(ctx, p, client.Apply, fieldOwner, client.ForceOwnership)
}
