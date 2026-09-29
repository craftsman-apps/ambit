package controller

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// MCPBackendName is the AgentgatewayBackend the chart's MCP route sends traffic to. It multiplexes
// every Server; clients see each tool as "<server>_<tool>".
const MCPBackendName = "ambit-mcp"

// ToolHive's proxy listens here; its Service is mcp-<name>-proxy.
const proxyPort = 8080

// serverAccess is who may use one server: on all its tools, and per tool.
type serverAccess struct {
	all   Access
	tools map[string]Access
}

func compileServers(servers []v1.Server, rules []v1.AccessRule) []*unstructured.Unstructured {
	if len(servers) == 0 {
		return nil
	}
	sorted := append([]v1.Server(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	access := map[string]*serverAccess{}
	for _, s := range sorted {
		access[s.Name] = &serverAccess{tools: map[string]Access{}}
	}
	for _, r := range rules {
		for _, sa := range r.Spec.Servers {
			a := access[sa.Name]
			if a == nil {
				continue // reported as UnknownServer on the rule
			}
			if len(sa.Tools) == 0 {
				a.all = merge(a.all, r.Spec)
				continue
			}
			for _, tool := range sa.Tools {
				a.tools[tool] = merge(a.tools[tool], r.Spec)
			}
		}
	}

	var out, owners []*unstructured.Unstructured
	var refs []metav1.OwnerReference
	var targets []any
	var clauses []string
	for _, s := range sorted {
		out = append(out, mcpServer(s))
		refs = append(refs, metav1.OwnerReference{APIVersion: v1.GroupVersion.String(), Kind: "Server", Name: s.Name, UID: s.UID})
		protocol, path := "StreamableHTTP", "/mcp"
		if s.Spec.Transport == "sse" {
			protocol, path = "SSE", "/sse"
		}
		targets = append(targets, map[string]any{"name": s.Name, "static": map[string]any{
			"backendRef": map[string]any{"name": "mcp-" + s.Name + "-proxy"},
			"port":       int64(proxyPort), "path": path, "protocol": protocol,
		}})
		clauses = append(clauses, serverClauses(s.Name, access[s.Name])...)
	}
	expr := "false"
	if len(clauses) > 0 {
		expr = strings.Join(clauses, " || ")
	}
	// ponytail: one expression for every server and tool; CRD limit is 16384 characters, so split
	// into several Allow policies if a catalog ever grows past a few hundred rule entries.
	backend := generated("agentgateway.dev/v1alpha1", "AgentgatewayBackend", MCPBackendName, sorted[0].Namespace, map[string]any{
		"mcp": map[string]any{
			// One broken server must not take away every other server's tools.
			"failureMode": "FailOpen",
			"prefixMode":  "Always",
			"targets":     targets,
		},
	})
	policy := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", MCPBackendName+"-access", sorted[0].Namespace, map[string]any{
		"targetRefs": []any{map[string]any{"group": "agentgateway.dev", "kind": "AgentgatewayBackend", "name": MCPBackendName}},
		"backend": map[string]any{"mcp": map[string]any{"authorization": map[string]any{
			"action": "Allow",
			"policy": map[string]any{"matchExpressions": []any{expr}},
		}}},
	})
	owners = append(owners, backend, policy)
	for _, o := range owners {
		o.SetOwnerReferences(refs) // not controller refs: garbage-collected only when the last Server is gone
	}
	return append(out, owners...)
}

func merge(a Access, spec v1.AccessRuleSpec) Access {
	a.AllSignedIn = a.AllSignedIn || spec.AllSignedIn
	a.Groups = append(a.Groups, spec.Groups...)
	return a
}

// normalize sorts and removes duplicate groups so the expression is stable.
func normalize(a Access) Access {
	sort.Strings(a.Groups)
	a.Groups = slices.Compact(a.Groups)
	return a
}

// serverClauses gives one clause for whole-server access, then one per group of tools that share
// the same audience. Access is by the unprefixed tool name, which is what agentgateway evaluates.
func serverClauses(name string, a *serverAccess) []string {
	target := "mcp.tool.target == " + strconv.Quote(name)
	var out []string
	if who := normalize(a.all); who.AllSignedIn || len(who.Groups) > 0 {
		out = append(out, withWho(target, who))
	}
	byWho := map[string][]string{}
	whoOf := map[string]Access{}
	for tool, acc := range a.tools {
		acc = normalize(acc)
		k := CEL(acc)
		byWho[k] = append(byWho[k], tool)
		whoOf[k] = acc
	}
	type clause struct{ first, text string }
	var cs []clause
	for k, tools := range byWho {
		sort.Strings(tools)
		q := make([]string, len(tools))
		for i, t := range tools {
			q[i] = strconv.Quote(t) // tool names are restricted by the CRD pattern
		}
		match := "mcp.tool.name == " + q[0]
		if len(q) > 1 {
			match = "mcp.tool.name in [" + strings.Join(q, ", ") + "]"
		}
		cs = append(cs, clause{tools[0], withWho(target+" && "+match, whoOf[k])})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].first < cs[j].first })
	for _, c := range cs {
		out = append(out, c.text)
	}
	return out
}

func withWho(cond string, who Access) string {
	if who.AllSignedIn {
		return cond
	}
	return cond + " && " + CEL(who)
}

func mcpServer(s v1.Server) *unstructured.Unstructured {
	spec := map[string]any{
		"image":     s.Spec.Image,
		"transport": s.Spec.Transport,
		"proxyPort": int64(proxyPort),
	}
	if s.Spec.Transport == "" {
		spec["transport"] = "stdio"
	}
	if s.Spec.MCPPort != 0 {
		spec["mcpPort"] = int64(s.Spec.MCPPort)
	}
	o := generated("toolhive.stacklok.dev/v1beta1", "MCPServer", s.Name, s.Namespace, spec)
	o.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: v1.GroupVersion.String(), Kind: "Server", Name: s.Name, UID: s.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}})
	return o
}

// generated builds an object with only Ambit's own labels (GitOps rule 1: no Argo CD metadata).
func generated(apiVersion, kind, name, ns string, spec map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetName(name)
	o.SetNamespace(ns)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit"})
	return o
}
