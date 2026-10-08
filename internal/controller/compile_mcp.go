package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"net/url"
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

// The console's token broker: the gateway asks it for the calling user's token for a Connection.
const (
	BrokerService = "ambit-console"
	BrokerPort    = 8082
)

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
	access := accessByServer(sorted, rules)

	var out, owners []*unstructured.Unstructured
	var refs []metav1.OwnerReference
	var targets []any
	var clauses []string
	for _, s := range sorted {
		refs = append(refs, metav1.OwnerReference{APIVersion: v1.GroupVersion.String(), Kind: "Server", Name: s.Name, UID: s.UID})
		t, ok := target(s)
		if !ok {
			continue // a URL that does not parse (the console refuses these; kubectl can still write one): left out
		}
		if s.Spec.URL == "" {
			out = append(out, mcpServer(s))
		}
		// agentgateway opens each session by contacting every target: an image server that is still starting
		// (or stopping) would slow every user's new session, so it joins only once ToolHive runs it.
		if s.Spec.URL != "" || meta.IsStatusConditionTrue(s.Status.Conditions, "Ready") {
			targets = append(targets, map[string]any{"name": s.Name, "static": t})
		}
		if s.Spec.Connection != "" {
			out = append(out, connectionPolicy(s))
		}
		clauses = append(clauses, serverClauses(s.Name, access[s.Name])...)
	}
	if len(targets) == 0 {
		return out // agentgateway refuses an MCP backend without targets; the reconciler deletes a previous one
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

func accessByServer(servers []v1.Server, rules []v1.AccessRule) map[string]*serverAccess {
	access := map[string]*serverAccess{}
	for _, s := range servers {
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
	return access
}

// ServerCEL is the part of the MCP access policy that covers one server, merged from every rule;
// "false" when nobody may use any of its tools. The console shows it next to a rule.
func ServerCEL(servers []v1.Server, rules []v1.AccessRule, name string) string {
	a := accessByServer(servers, rules)[name]
	if a == nil {
		return "false"
	}
	if c := serverClauses(name, a); len(c) > 0 {
		return strings.Join(c, " || ")
	}
	return "false"
}

// target is where the gateway sends one server's MCP traffic: ToolHive's proxy, or the remote URL.
func target(s v1.Server) (map[string]any, bool) {
	protocol, path := "StreamableHTTP", "/mcp"
	if s.Spec.Transport == "sse" {
		protocol, path = "SSE", "/sse"
	}
	if s.Spec.URL == "" {
		// ToolHive's proxy Service by DNS name, not as a backendRef: agentgateway v1.6.0 rejects the whole shared
		// backend (every user's MCP traffic) while any referenced Service is missing, which this one is for a moment
		// around every create and delete. A host is resolved per connection, so only this server is affected.
		// ponytail: assumes the cluster domain cluster.local; add a controller flag for clusters that change it.
		host := "mcp-" + s.Name + "-proxy." + s.Namespace + ".svc.cluster.local"
		return map[string]any{"host": host, "port": int64(proxyPort), "path": path, "protocol": protocol}, true
	}
	// The CRD pattern still lets through a bad port ("host:bad") or escape ("%zz").
	u, err := url.Parse(s.Spec.URL)
	if err != nil || u.Hostname() == "" {
		return nil, false
	}
	port, err := strconv.ParseInt(u.Port(), 10, 64)
	if err != nil && u.Port() != "" {
		return nil, false
	}
	t := map[string]any{"host": u.Hostname(), "protocol": protocol, "path": u.Path}
	if u.Path == "" {
		t["path"] = "/"
	}
	if u.Scheme == "https" {
		if port == 0 {
			port = 443
		}
		t["policies"] = map[string]any{"tls": map[string]any{}} // verify with the system CAs
	} else if port == 0 {
		port = 80
	}
	t["port"] = port
	return t, true
}

// connectionPolicy makes the gateway ask the console's broker (extAuth) for the calling user's own
// token for the app, naming the user by the validated jwt.email claim, and send it upstream as
// Authorization. The user's gateway JWT is never forwarded (the gateway removes it after validation).
// Without a grant the broker adds no token, so only this server drops out of the user's session.
// FailOpen: if the broker is down, these servers fail the same way instead of every session failing.
func connectionPolicy(s v1.Server) *unstructured.Unstructured {
	o := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "ambit-conn-"+s.Name, s.Namespace, map[string]any{
		"targetRefs": []any{map[string]any{"group": "agentgateway.dev", "kind": "AgentgatewayBackend", "name": MCPBackendName, "sectionName": s.Name}},
		"backend": map[string]any{"extAuth": map[string]any{
			"backendRef":  map[string]any{"name": BrokerService, "port": int64(BrokerPort)},
			"failureMode": "FailOpen",
			"http": map[string]any{
				"path":                   strconv.Quote("/v1/connection/" + s.Spec.Connection), // a CEL string literal
				"addRequestHeaders":      map[string]any{"x-ambit-user": "jwt.email"},
				"allowedRequestHeaders":  []any{},
				"allowedResponseHeaders": []any{"authorization"},
			},
		}},
	})
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "Server", Name: s.Name, UID: s.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}})
	return o
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

// EffectiveAccess summarises who may use one server, for the console's "pick tools" step.
type EffectiveAccess struct {
	AllSignedIn bool                `json:"allSignedIn"`
	Groups      []string            `json:"groups"`
	AllTools    bool                `json:"allTools"` // some rule grants every tool, including ones added later
	Tools       map[string][]string `json:"tools"`    // tool → groups granted it by name
}

// ServerAccess reads the same merged access the gateway policy is built from (accessByServer), and
// is empty for a Server the reconciler leaves out: refused by BindServers, or with a URL that does not parse.
func ServerAccess(servers []v1.Server, conns []v1.Connection, rules []v1.AccessRule, name string) EffectiveAccess {
	out := EffectiveAccess{Groups: []string{}, Tools: map[string][]string{}}
	kept, _ := BindServers(servers, conns)
	i := slices.IndexFunc(kept, func(s v1.Server) bool { return s.Name == name })
	if i < 0 {
		return out
	}
	if _, ok := target(kept[i]); !ok {
		return out
	}
	a := accessByServer(kept, rules)[name]
	groups := map[string]bool{}
	add := func(x Access) {
		out.AllSignedIn = out.AllSignedIn || x.AllSignedIn
		for _, g := range x.Groups {
			groups[g] = true
		}
	}
	if a.all.AllSignedIn || len(a.all.Groups) > 0 {
		out.AllTools = true
		add(a.all)
	}
	for tool, x := range a.tools {
		add(x)
		out.Tools[tool] = append([]string{}, x.Groups...)
	}
	for g := range groups {
		out.Groups = append(out.Groups, g)
	}
	sort.Strings(out.Groups)
	return out
}
