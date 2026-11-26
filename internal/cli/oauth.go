package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// The emergency block of OAuth tokens (docs/oauth.md, "If a signing key may be stolen"): one policy that Helm and Argo CD do not own, so that
// neither a Helm upgrade nor GitOps self-heal removes it. The CLI makes and removes it with the operator's own rights, not the console's.
const (
	oauthBlockPolicy  = "ambit-oauth-emergency-block"
	oauthTokenPolicy  = "ambit-mcp-public-oauth" // the policy of the chart that holds the token rule
	oauthPublicRoute  = "ambit-mcp-public"
	oauthBlockPurpose = "Emergency block of every OAuth token on the public MCP route (made by: ambit oauth block on). Lift it with: ambit oauth block off."
)

var agentgatewayPolicy = schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: "AgentgatewayPolicy"}

func (a *app) oauthCmd() *cobra.Command {
	oauth := &cobra.Command{Use: "oauth", Short: "OAuth sign-in for hosted MCP clients (experimental)"}
	block := &cobra.Command{Use: "block", Short: "The emergency block of every OAuth token"}
	block.AddCommand(
		&cobra.Command{
			Use:   "on",
			Short: "Refuse every OAuth token on the public MCP route, at once",
			Long: `Creates the policy ` + oauthBlockPolicy + ` on the public MCP route: from then on every request with an OAuth token gets 403 on every public
gateway pod (within a second; it needs no Hydra, no key and none of Ambit's own components). API keys are not touched. Helm and Argo CD do not own
this policy: a helm upgrade, Argo CD self-heal and Flux drift correction leave it alone, so it holds until you run ambit oauth block off.
On a terminal you confirm with y; without a terminal, --yes is required.`,
			Args: cobra.NoArgs,
			RunE: run(func(cmd *cobra.Command, _ []string) error { return a.oauthBlock(cmd.Context(), true) }),
		},
		&cobra.Command{
			Use:   "off",
			Short: "Lift the emergency block made by ambit oauth block on",
			Args:  cobra.NoArgs,
			RunE:  run(func(cmd *cobra.Command, _ []string) error { return a.oauthBlock(cmd.Context(), false) }),
		},
		&cobra.Command{
			Use:   "status",
			Short: "Say whether OAuth tokens are blocked, and by what",
			Args:  cobra.NoArgs,
			RunE:  run(func(cmd *cobra.Command, _ []string) error { return a.oauthBlockStatus(cmd.Context()) }),
		},
	)
	oauth.AddCommand(block)
	return oauth
}

// oauthClients connects to the cluster of --kubeconfig and --context, or uses the test clients.
func (a *app) oauthClients() (*kube.Clients, error) {
	c, err := kube.Connect(kube.Target{Kubeconfig: a.g.kubeconfig, Context: a.g.context, Log: a.printer().Verbose()})
	if err != nil {
		return nil, &out.Error{What: "the target cluster cannot be resolved", Why: err.Error(), Next: "kubectl config get-contexts, then pass --context (and --kubeconfig)"}
	}
	if a.testKube != nil {
		c.Kube, c.Ctrl = a.testKube.Kube, a.testKube.Ctrl
	}
	return c, nil
}

func policyObject(ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(agentgatewayPolicy)
	o.SetNamespace(ns)
	o.SetName(name)
	return o
}

// blockPolicy is the policy: a Require rule on the whole public route that is false for every request that carries a validated token. Keys never have
// one (the key rule has no token check), and only tokens of Ambit's own sign-in host pass the route's token check, so the rule refuses exactly the
// OAuth tokens, also those with a valid signature of a stolen key.
func blockPolicy(ns string) *unstructured.Unstructured {
	o := policyObject(ns, oauthBlockPolicy)
	o.SetAnnotations(map[string]string{"ambitproject.dev/purpose": oauthBlockPurpose, "ambitproject.dev/created-by": "ambit oauth block on"})
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": oauthPublicRoute}}, "spec", "targetRefs")
	_ = unstructured.SetNestedField(o.Object, "Require", "spec", "traffic", "authorization", "action")
	_ = unstructured.SetNestedSlice(o.Object, []any{"!has(jwt.iss)"}, "spec", "traffic", "authorization", "policy", "matchExpressions")
	return o
}

// oauthBlockWait is how long `on` waits for the gateway to report the policy accepted.
var oauthBlockWait = 20 * time.Second

// isBlock reports whether the object is the block that `on` makes: its whole spec is the spec that blockPolicy writes (a Require rule on the public route that
// is false for every request with a token, and nothing else), so that an object of the same name with other settings is never taken for the block, or deleted.
func isBlock(o *unstructured.Unstructured) bool {
	have, _, _ := unstructured.NestedMap(o.Object, "spec")
	want, _, _ := unstructured.NestedMap(blockPolicy(o.GetNamespace()).Object, "spec")
	h, err1 := json.Marshal(have)
	w, err2 := json.Marshal(want)
	return err1 == nil && err2 == nil && bytes.Equal(h, w)
}

// accepted reports whether a gateway has accepted the current version of the policy (status.ancestors[].conditions: Accepted True for the current
// generation, and Attached not False). It says that the policy is attached; it does not say that every pod has loaded it.
func accepted(o *unstructured.Unstructured) bool {
	anc, _, _ := unstructured.NestedSlice(o.Object, "status", "ancestors")
	for _, a := range anc {
		m, _ := a.(map[string]any)
		conds, _, _ := unstructured.NestedSlice(m, "conditions")
		acc, attachedFalse := false, false
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if g, has := cm["observedGeneration"]; has && fmt.Sprint(g) != fmt.Sprint(o.GetGeneration()) {
				continue // a condition about an older version of the policy
			}
			switch cm["type"] {
			case "Accepted":
				acc = cm["status"] == "True"
			case "Attached":
				attachedFalse = cm["status"] == "False"
			}
		}
		if acc && !attachedFalse {
			return true
		}
	}
	return false
}

// waitAccepted waits up to oauthBlockWait for a gateway to report the policy accepted, and says whether it did.
func waitAccepted(ctx context.Context, c *kube.Clients, ns string) bool {
	got := policyObject(ns, oauthBlockPolicy)
	deadline := time.Now().Add(oauthBlockWait)
	for {
		if err := c.Ctrl.Get(ctx, client.ObjectKeyFromObject(got), got); err == nil && accepted(got) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

func (a *app) oauthBlock(ctx context.Context, on bool) error {
	p := a.printer()
	c, err := a.oauthClients()
	if err != nil {
		return err
	}
	ns := a.g.namespace
	if on {
		// OAuth sign-in must be on: the token policy of the chart is there. (Lifting the block needs no such policy.)
		if err := c.Ctrl.Get(ctx, client.ObjectKey{Namespace: ns, Name: oauthTokenPolicy}, policyObject(ns, oauthTokenPolicy)); err != nil {
			if apierrors.IsNotFound(err) {
				return &out.Error{What: "OAuth sign-in is not turned on in namespace " + ns, Why: "the policy " + oauthTokenPolicy + " does not exist",
					Next: "pass -n <namespace> if Ambit is installed in another namespace"}
			}
			return &out.Error{What: "the policy " + oauthTokenPolicy + " cannot be read", Why: err.Error(), Next: "check the context and your rights (kubectl auth can-i get agentgatewaypolicies -n " + ns + ")"}
		}
	}
	existing := policyObject(ns, oauthBlockPolicy)
	getErr := c.Ctrl.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return &out.Error{What: "the policy " + oauthBlockPolicy + " cannot be read", Why: getErr.Error()}
	}
	exists := getErr == nil
	if exists && !isBlock(existing) {
		return &out.Error{What: "a policy named " + oauthBlockPolicy + " exists, but it is not the block", Why: "its rule is not the one that ambit oauth block on makes, so it may not refuse tokens",
			Next: "kubectl -n " + ns + " get agentgatewaypolicy " + oauthBlockPolicy + " -o yaml; delete it, then run ambit oauth block on"}
	}
	if on {
		if exists {
			if waitAccepted(ctx, c, ns) {
				p.Line("%s OAuth tokens are already blocked (policy %s in namespace %s, accepted by the gateway).", p.Mark("ok"), oauthBlockPolicy, ns)
			} else {
				p.Line("%s Policy %s exists in namespace %s, but the gateway has not reported it accepted: tokens may not be blocked yet. Check it, and try a token:", p.Mark("warn"), oauthBlockPolicy, ns)
				p.Line("  kubectl -n %s get agentgatewaypolicy %s -o jsonpath='{.status.ancestors[*].conditions}'", ns, oauthBlockPolicy)
			}
			return nil
		}
		if err := kube.Confirm(p, a.stdin(), a.stdinTTY(), a.g.yes, []string{
			fmt.Sprintf("Blocking every OAuth token on the public MCP route in context %s (%s), namespace %s.", c.Context, c.Server, ns),
			"Every platform that is connected gets 403 until you lift the block. API keys are not touched.",
		}, "Block now?"); err != nil {
			return err
		}
		if err := c.Ctrl.Create(ctx, blockPolicy(ns)); err != nil {
			if !apierrors.IsAlreadyExists(err) { // another operator made it meanwhile: it must be the block
				return &out.Error{What: "the policy " + oauthBlockPolicy + " was not created", Why: err.Error(), Next: "kubectl auth can-i create agentgatewaypolicies -n " + ns}
			}
			if gerr := c.Ctrl.Get(ctx, client.ObjectKeyFromObject(existing), existing); gerr != nil || !isBlock(existing) {
				return &out.Error{What: "a policy named " + oauthBlockPolicy + " exists, but it is not the block", Why: "it was made while this command ran",
					Next: "kubectl -n " + ns + " get agentgatewaypolicy " + oauthBlockPolicy + " -o yaml"}
			}
		}
		// The block is in force when a gateway has accepted the policy; say what is known.
		ok := waitAccepted(ctx, c, ns)
		if ok {
			p.Line("%s OAuth tokens are blocked: policy %s created in namespace %s, and the gateway has accepted it.", p.Mark("ok"), oauthBlockPolicy, ns)
		} else {
			p.Line("%s Policy %s is created in namespace %s, but the gateway has not reported it accepted yet. Check it, and try a token:", p.Mark("warn"), oauthBlockPolicy, ns)
			p.Line("  kubectl -n %s get agentgatewaypolicy %s -o jsonpath='{.status.ancestors[*].conditions}'", ns, oauthBlockPolicy)
		}
		p.Line("Helm and Argo CD do not own this policy: an upgrade or self-heal leaves it. Lift it with: ambit oauth block off")
		return nil
	}
	if !exists {
		p.Line("There is no block made by this command (no policy %s in namespace %s). If tokens are still refused, the block is in the policy %s (oauth.emergencyBlock, or an edit of it): see docs/oauth.md.",
			oauthBlockPolicy, ns, oauthTokenPolicy)
		return nil
	}
	if err := kube.Confirm(p, a.stdin(), a.stdinTTY(), a.g.yes, []string{
		fmt.Sprintf("Lifting the block of OAuth tokens in context %s (%s), namespace %s.", c.Context, c.Server, ns),
		"Tokens that are valid work again at once. Lift it only when the check on every gateway pod is done (docs/oauth.md).",
	}, "Lift the block?"); err != nil {
		return err
	}
	// Delete exactly the object that was shown: not one that replaced it while the question was open.
	uid, rv := existing.GetUID(), existing.GetResourceVersion()
	if err := c.Ctrl.Delete(ctx, existing, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		return &out.Error{What: "the policy " + oauthBlockPolicy + " was not deleted", Why: err.Error(), Next: "ambit oauth block status, then run the command again"}
	}
	p.Line("%s The block is lifted: policy %s deleted.", p.Mark("ok"), oauthBlockPolicy)
	return nil
}

func (a *app) oauthBlockStatus(ctx context.Context) error {
	p := a.printer()
	c, err := a.oauthClients()
	if err != nil {
		return err
	}
	ns := a.g.namespace
	tok := policyObject(ns, oauthTokenPolicy)
	haveTok := true
	if err := c.Ctrl.Get(ctx, client.ObjectKey{Namespace: ns, Name: oauthTokenPolicy}, tok); err != nil {
		if !apierrors.IsNotFound(err) {
			return &out.Error{What: "the policy " + oauthTokenPolicy + " cannot be read", Why: err.Error()}
		}
		haveTok = false
	}
	blocked := false
	blk := policyObject(ns, oauthBlockPolicy)
	if err := c.Ctrl.Get(ctx, client.ObjectKeyFromObject(blk), blk); err == nil {
		switch {
		case !isBlock(blk):
			p.Line("A policy named %s exists, but it is not the block that ambit oauth block on makes: it may not refuse tokens (kubectl -n %s get agentgatewaypolicy %s -o yaml).", oauthBlockPolicy, ns, oauthBlockPolicy)
		case accepted(blk):
			p.Line("OAuth tokens are BLOCKED by the policy %s (made by ambit oauth block on; the gateway has accepted it; lift it with ambit oauth block off).", oauthBlockPolicy)
			blocked = true
		default:
			p.Line("The block policy %s exists, but the gateway has not reported it accepted: tokens may not be blocked yet.", oauthBlockPolicy)
		}
	} else if !apierrors.IsNotFound(err) {
		return &out.Error{What: "the policy " + oauthBlockPolicy + " cannot be read", Why: err.Error()}
	}
	exprs, _, _ := unstructured.NestedStringSlice(tok.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	for _, e := range exprs {
		if e == "false" {
			p.Line("OAuth tokens are BLOCKED by a clause `false` in the policy %s (oauth.emergencyBlock, or an edit of the policy; Helm or Argo CD may remove it).", oauthTokenPolicy)
			blocked = true
		}
	}
	switch {
	case blocked:
	case !haveTok:
		p.Line("OAuth sign-in is not turned on in namespace %s.", ns)
	default:
		p.Line("OAuth tokens are not blocked.")
	}
	return nil
}
