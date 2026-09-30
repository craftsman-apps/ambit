package controller

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OffboardedConfigMap is written by the console's SCIM receiver: the emails of deactivated users.
const OffboardedConfigMap = "ambit-offboarded"

// safeEmail keeps CEL string literals trivially safe; anything else is left out and logged.
var safeEmail = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+$`)

// OffboardedEmails reads the list, keeping only well-formed lowercase emails.
func OffboardedEmails(cm *corev1.ConfigMap) (ok, rejected []string) {
	var emails []string
	_ = json.Unmarshal([]byte(cm.Data["emails"]), &emails)
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if safeEmail.MatchString(e) {
			ok = append(ok, e)
		} else {
			rejected = append(rejected, e)
		}
	}
	sort.Strings(ok)
	return ok, rejected
}

// offboardedPolicy denies every gateway request from a deactivated user, so tokens their identity
// provider already issued stop working now instead of when they expire (journey A4).
func offboardedPolicy(cm *corev1.ConfigMap, emails []string, routes []string) *unstructured.Unstructured {
	expr := "false"
	if len(emails) > 0 {
		q := make([]string, len(emails))
		for i, e := range emails {
			q[i] = strconv.Quote(e)
		}
		expr = "jwt.email in [" + strings.Join(q, ", ") + "]"
	}
	var refs []any
	for _, r := range routes {
		refs = append(refs, map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": r})
	}
	o := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "ambit-offboarded", cm.Namespace, map[string]any{
		"targetRefs": refs,
		"traffic": map[string]any{"authorization": map[string]any{
			"action": "Deny",
			"policy": map[string]any{"matchExpressions": []any{expr}},
		}},
	})
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: cm.Name, UID: cm.UID}})
	return o
}
