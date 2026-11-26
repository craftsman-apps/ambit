package cli

import (
	"context"
	"fmt"

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

func (a *app) oauthBlock(ctx context.Context, on bool) error {
	p := a.printer()
	c, err := a.oauthClients()
	if err != nil {
		return err
	}
	ns := a.g.namespace
	// OAuth sign-in must be on: the token policy of the chart is there.
	if err := c.Ctrl.Get(ctx, client.ObjectKey{Namespace: ns, Name: oauthTokenPolicy}, policyObject(ns, oauthTokenPolicy)); err != nil {
		if apierrors.IsNotFound(err) {
			return &out.Error{What: "OAuth sign-in is not turned on in namespace " + ns, Why: "the policy " + oauthTokenPolicy + " does not exist",
				Next: "pass -n <namespace> if Ambit is installed in another namespace"}
		}
		return &out.Error{What: "the policy " + oauthTokenPolicy + " cannot be read", Why: err.Error(), Next: "check the context and your rights (kubectl auth can-i get agentgatewaypolicies -n " + ns + ")"}
	}
	existing := policyObject(ns, oauthBlockPolicy)
	getErr := c.Ctrl.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return &out.Error{What: "the policy " + oauthBlockPolicy + " cannot be read", Why: getErr.Error()}
	}
	exists := getErr == nil
	if on {
		if exists {
			p.Line("%s OAuth tokens are already blocked (policy %s in namespace %s).", p.Mark("ok"), oauthBlockPolicy, ns)
			return nil
		}
		if err := kube.Confirm(p, a.stdin(), a.stdinTTY(), a.g.yes, []string{
			fmt.Sprintf("Blocking every OAuth token on the public MCP route in context %s (%s), namespace %s.", c.Context, c.Server, ns),
			"Every platform that is connected gets 403 until you lift the block. API keys are not touched.",
		}, "Block now?"); err != nil {
			return err
		}
		if err := c.Ctrl.Create(ctx, blockPolicy(ns)); err != nil {
			return &out.Error{What: "the policy " + oauthBlockPolicy + " was not created", Why: err.Error(), Next: "kubectl auth can-i create agentgatewaypolicies -n " + ns}
		}
		p.Line("%s OAuth tokens are blocked: policy %s created in namespace %s. Every public gateway pod refuses a token within a second.", p.Mark("ok"), oauthBlockPolicy, ns)
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
		"Tokens that are valid work again at once. Lift it only when the check on every gateway pod says that the old key is gone (docs/oauth.md).",
	}, "Lift the block?"); err != nil {
		return err
	}
	if err := c.Ctrl.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return &out.Error{What: "the policy " + oauthBlockPolicy + " was not deleted", Why: err.Error()}
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
	if err := c.Ctrl.Get(ctx, client.ObjectKey{Namespace: ns, Name: oauthTokenPolicy}, tok); err != nil {
		if apierrors.IsNotFound(err) {
			p.Line("OAuth sign-in is not turned on in namespace %s.", ns)
			return nil
		}
		return &out.Error{What: "the policy " + oauthTokenPolicy + " cannot be read", Why: err.Error()}
	}
	blocked := false
	if err := c.Ctrl.Get(ctx, client.ObjectKey{Namespace: ns, Name: oauthBlockPolicy}, policyObject(ns, oauthBlockPolicy)); err == nil {
		p.Line("OAuth tokens are BLOCKED by the policy %s (made by ambit oauth block on; lift it with ambit oauth block off).", oauthBlockPolicy)
		blocked = true
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
	if !blocked {
		p.Line("OAuth tokens are not blocked.")
	}
	return nil
}
