package controller

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OffboardedConfigMap is written by the console's SCIM receiver: the emails of deactivated users.
const OffboardedConfigMap = "ambit-offboarded"

// OffboardedEmails reads the list, keeping emails that pass the shared email rule (identity.CanonicalEmail), in
// canonical form. strconv.Quote makes each a safe CEL string literal.
func OffboardedEmails(cm *corev1.ConfigMap) (ok, rejected []string) {
	var emails []string
	_ = json.Unmarshal([]byte(cm.Data["emails"]), &emails)
	for _, e := range emails {
		if c, good := identity.CanonicalEmail(e); good {
			ok = append(ok, c)
		} else {
			rejected = append(rejected, e)
		}
	}
	sort.Strings(ok)
	return ok, rejected
}

// offboardedPolicy requires that the request's identity (key or JWT) is not a deactivated user, so tokens and keys
// stop working now instead of when they expire (journey A4). Require, not Deny: a Deny whose expression errors does
// not deny.
func offboardedPolicy(cm *corev1.ConfigMap, emails []string, routes []string) *unstructured.Unstructured {
	expr := "true"
	if len(emails) > 0 {
		q := make([]string, len(emails))
		for i, e := range emails {
			q[i] = strconv.Quote(e)
		}
		expr = "!(" + principal() + " in [" + strings.Join(q, ", ") + "])"
	}
	var refs []any
	for _, r := range routes {
		refs = append(refs, map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": r})
	}
	o := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "ambit-offboarded", cm.Namespace, map[string]any{
		"targetRefs": refs,
		"traffic": map[string]any{"authorization": map[string]any{
			"action": "Require",
			"policy": map[string]any{"matchExpressions": []any{expr}},
		}},
	})
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: cm.Name, UID: cm.UID}})
	return o
}
