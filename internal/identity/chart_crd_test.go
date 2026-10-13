package identity

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// Every agentgateway object the chart renders must pass the agentgateway v1.6.0 CRD schema, as the API server
// checks it on install. The agentgateway standalone config and the CRD differ in places (a transformation's
// response.set is a map in one and a list of {name, value} in the other); a helm upgrade with limits.enabled
// failed on that (2026-10-13). The renders cover every chart switch that adds agentgateway objects.
func TestChartMatchesAgentgatewayCRDs(t *testing.T) {
	schemas := agentgatewaySchemas(t)
	renders := map[string][]string{
		"quick start":      nil,
		"budgets":          {"--set", "limits.enabled=true"},
		"budgets + guards": {"--set", "limits.enabled=true", "--set", "protection.enabled=true"},
		"no email rule":    {"--set", "limits.enabled=true", "--set", "llm.jwt.requireEmail=false", "--set", "mcp.jwt.requireEmail=false"},
	}
	for name, args := range renders {
		checked := 0
		for _, d := range render(t, args...) {
			api, _ := d["apiVersion"].(string)
			if !strings.HasPrefix(api, "agentgateway.dev/") {
				continue
			}
			k, ok := schemas[api+"/"+d["kind"].(string)]
			if !ok {
				t.Errorf("%s: no CRD for %s %s", name, api, d["kind"])
				continue
			}
			checked++
			// As the API server reads it: whole numbers are int64 (YAML decoding gives float64).
			b, _ := json.Marshal(d)
			var obj map[string]any
			if err := utiljson.Unmarshal(b, &obj); err != nil {
				t.Fatal(err)
			}
			id := name + ": " + d["kind"].(string) + " " + get(t, d, "metadata", "name").(string)
			defaulting.Default(obj, k.structural) // as the API server does before its rules run
			opts := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
			for _, p := range pruning.PruneWithOptions(obj, k.structural, true, opts) {
				t.Errorf("%s: unknown field %s", id, p)
			}
			for _, e := range apivalidation.ValidateCustomResource(nil, obj, k.schema) {
				t.Errorf("%s: %v", id, e)
			}
			errs, _ := k.cel.Validate(context.Background(), nil, k.structural, obj, nil, celconfig.RuntimeCELCostBudget)
			for _, e := range errs {
				t.Errorf("%s: %v", id, e)
			}
		}
		if checked == 0 {
			t.Errorf("%s: no agentgateway objects rendered", name)
		}
	}
}

type crdSchema struct {
	structural *structuralschema.Structural
	schema     apivalidation.SchemaCreateValidator
	cel        *cel.Validator
}

// agentgatewaySchemas reads the vendored agentgateway CRDs (deploy/crds), by "apiVersion/kind".
func agentgatewaySchemas(t *testing.T) map[string]crdSchema {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/crds/agentgateway-crds-v1.6.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]crdSchema{}
	for _, part := range strings.Split("\n"+string(raw), "\n---\n") {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal([]byte(part), &crd); err != nil {
			t.Fatal(err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}
		for _, ver := range crd.Spec.Versions {
			var props apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(ver.Schema.OpenAPIV3Schema, &props, nil); err != nil {
				t.Fatal(err)
			}
			s, err := structuralschema.NewStructural(&props)
			if err != nil {
				t.Fatalf("%s %s: %v", crd.Name, ver.Name, err)
			}
			sv, _, err := apivalidation.NewSchemaValidator(&props)
			if err != nil {
				t.Fatal(err)
			}
			out[crd.Spec.Group+"/"+ver.Name+"/"+crd.Spec.Names.Kind] = crdSchema{s, sv, cel.NewValidator(s, true, celconfig.PerCallLimit)}
		}
	}
	if len(out) == 0 {
		t.Fatal("no CRDs read")
	}
	return out
}
