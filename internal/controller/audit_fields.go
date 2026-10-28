package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Audit mode (#81): each access-log record says whether the AccessRules in force when it was served would have
// refused it. The gateway evaluates the fields at request time; they reuse the enforce compile's expressions, so
// there is no second copy of the rules to drift. A field whose CEL fails is left out of the record (agentgateway's
// eval_keep_empty), which the console counts as "mark missing", never as a refusal.

const (
	auditChunk   = 16000 // characters per field; agentgateway's CRD allows 16384
	maxLogFields = 64    // attributes one access-log policy may add
	// Fixed fields besides the chunks of ambit.wouldRefuse: ambit.accessMode, ambit.rulesVersion,
	// ambit.wouldRefuseTool, and one kept free.
	fixedLogFields = 4
)

// AuditTooLargeError: ambit.wouldRefuse needs more attributes than the access-log policy has room for, or one
// branch (branch, of size characters) is longer than one field holds.
type AuditTooLargeError struct {
	need, room int
	branch     string
	size       int
}

func (e *AuditTooLargeError) Error() string {
	if e.branch != "" {
		return fmt.Sprintf("the audit-only field for %s is %d characters, and one access-log field holds at most %d; the rules stay enforced. "+
			"To use audit-only mode, grant it in AccessRules to fewer groups and people (for a router: give it fewer targets)", e.branch, e.size, auditChunk)
	}
	return fmt.Sprintf("the audit-only fields need %d attributes for ambit.wouldRefuse, and the access-log policy has room for %d; the rules stay enforced", e.need, e.room)
}

// AuditFields returns ambit.wouldRefuse (split over ambit.wouldRefuse.1, .2, ... above auditChunk characters; each
// part ends in "n/a") and ambit.wouldRefuseTool, from the objects of the enforce compile (after RefuseUnpriced and
// CompileRouters). room is how many attributes ambit.wouldRefuse may use.
func AuditFields(res Result, room int) ([]any, error) {
	final := map[string]string{}    // Model object name -> its authorization expression
	specOf := map[string]string{}   // Model object name -> spec.model, as the gateway matches it
	bySpec := map[string][]string{} // spec.model -> the expressions of its Models (enforce allows when one does)
	var routers []*unstructured.Unstructured
	tool := "false"
	for _, o := range res.Objects {
		switch {
		case o.GetKind() == "AgentgatewayModel" && o.GetLabels()[RouterLabel] != "":
			routers = append(routers, o)
		case o.GetKind() == "AgentgatewayModel" && o.GetLabels()[FallbackTargetLabel] != "":
			// a copy of a fallback Model, open to fewer people than the Model itself: its expression adds nothing
		case o.GetKind() == "AgentgatewayModel":
			e, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
			spec, _, _ := unstructured.NestedString(o.Object, "spec", "match", "model")
			if len(e) != 1 {
				continue
			}
			final[o.GetName()], specOf[o.GetName()] = e[0], spec
			if !slices.Contains(bySpec[spec], e[0]) { // a Model's extra endpoints repeat its expression
				bySpec[spec] = append(bySpec[spec], e[0])
			}
		case o.GetKind() == "AgentgatewayPolicy" && o.GetName() == MCPBackendName+"-access":
			if e, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions"); len(e) == 1 {
				tool = e[0]
			}
		}
	}
	// Routers first: a routed record's llm.requestModel is the model that answered, so the model branches would
	// judge it as a direct call. The router name the client asked for is metadata.agentgateway_user_model.
	var branches, names []string // names: what each branch is for, in AuditTooLarge
	slices.SortFunc(routers, func(a, b *unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, vm := range routers {
		wire, _, _ := unstructured.NestedString(vm.Object, "spec", "match", "model")
		lines, _, _ := unstructured.NestedSlice(vm.Object, "spec", "virtualModel", "conditional", "targets")
		var answering, def string
		seen := map[string]bool{}
		for _, l := range lines {
			t, _, _ := unstructured.NestedString(l.(map[string]any), "modelRef", "name")
			def = t // the default is the last line
			if !seen[t] {
				seen[t] = true
				answering += fmt.Sprintf("llm.requestModel == %s ? (%s) : ", strconv.Quote(specOf[t]), orFalse(final[t]))
			}
		}
		branches = append(branches, fmt.Sprintf(`has(metadata.agentgateway_user_model) && metadata.agentgateway_user_model == %s ? `+
			`((%s) ? ((%sfalse) ? "allow" : ((%s) ? "other-model" : "refuse:model")) : "refuse:router") : `,
			strconv.Quote(wire), CEL(res.Access[vm.GetLabels()[RouterLabel]]), answering, orFalse(final[def])))
		names = append(names, "router "+strconv.Quote(vm.GetLabels()[RouterLabel]))
	}
	specs := make([]string, 0, len(bySpec))
	for s := range bySpec {
		specs = append(specs, s)
	}
	slices.Sort(specs)
	for _, s := range specs {
		branches = append(branches, fmt.Sprintf(`llm.requestModel == %s ? ((%s) ? "allow" : "refuse:model") : `,
			strconv.Quote(s), "("+strings.Join(bySpec[s], ") || (")+")"))
		names = append(names, "model "+strconv.Quote(s))
	}
	var chunks []string
	cur := ""
	for i, b := range branches {
		if len(b)+len(`"n/a"`) > auditChunk { // one branch alone is too long to split
			return nil, &AuditTooLargeError{branch: names[i], size: len(b) + len(`"n/a"`)}
		}
		if cur != "" && len(cur)+len(b)+len(`"n/a"`) > auditChunk {
			chunks, cur = append(chunks, cur+`"n/a"`), ""
		}
		cur += b
	}
	chunks = append(chunks, cur+`"n/a"`)
	if len(chunks) > room {
		return nil, &AuditTooLargeError{need: len(chunks), room: room}
	}
	var fields []any
	if len(chunks) == 1 {
		fields = append(fields, map[string]any{"name": "ambit.wouldRefuse", "expression": chunks[0]})
	} else {
		for i, c := range chunks {
			fields = append(fields, map[string]any{"name": fmt.Sprintf("ambit.wouldRefuse.%d", i+1), "expression": c})
		}
	}
	fields = append(fields, map[string]any{"name": "ambit.wouldRefuseTool",
		"expression": fmt.Sprintf(`has(mcp.tool) ? ((%s) ? "allow" : "refuse:tool") : "n/a"`, tool)})
	return fields, nil
}

// orFalse: a line's target that is not a Model the compile produced allows no one.
func orFalse(expr string) string {
	if expr == "" {
		return "false"
	}
	return expr
}
