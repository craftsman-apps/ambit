// Package validate checks Ambit objects the way the Kubernetes API server would: defaults, unknown
// fields, the CRD schema and its CEL rules. `ambit validate` runs it in a pull request, before merge.
package validate

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// CRDs are copied here by `make generate` from charts/ambit-crds.
//
//go:embed crds/*.yaml
var crdFiles embed.FS

type kindSchema struct {
	structural *structuralschema.Structural
	schema     apivalidation.SchemaCreateValidator
	cel        *cel.Validator
}

// Validator knows every Ambit kind, by "apiVersion/kind".
type Validator struct{ kinds map[string]kindSchema }

func New() (*Validator, error) {
	v := &Validator{kinds: map[string]kindSchema{}}
	entries, err := crdFiles.ReadDir("crds")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		raw, err := crdFiles.ReadFile("crds/" + e.Name())
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		for _, ver := range crd.Spec.Versions {
			var props apiextensions.JSONSchemaProps
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(ver.Schema.OpenAPIV3Schema, &props, nil); err != nil {
				return nil, err
			}
			s, err := structuralschema.NewStructural(&props)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", crd.Name, ver.Name, err)
			}
			sv, _, err := apivalidation.NewSchemaValidator(&props)
			if err != nil {
				return nil, err
			}
			v.kinds[crd.Spec.Group+"/"+ver.Name+"/"+crd.Spec.Names.Kind] = kindSchema{structural: s, schema: sv, cel: cel.NewValidator(s, true, celconfig.PerCallLimit)}
		}
	}
	return v, nil
}

// Problem is one reason an object would be refused.
type Problem struct {
	Doc     int    // 1-based document number in the file
	Object  string // Kind/name
	Message string
}

func (p Problem) String() string { return fmt.Sprintf("document %d, %s: %s", p.Doc, p.Object, p.Message) }

// ErrNotAmbit marks documents that are not Ambit objects; they are skipped, not failed.
var ErrNotAmbit = errors.New("not an Ambit object")

// File checks every YAML document in r. It returns the problems found and the number of Ambit objects.
func (v *Validator) File(ctx context.Context, r io.Reader) ([]Problem, int, error) {
	dec := utilyaml.NewYAMLReader(bufio.NewReader(r))
	var problems []Problem
	n, checked := 0, 0
	for {
		doc, err := dec.Read()
		if errors.Is(err, io.EOF) {
			return problems, checked, nil
		}
		if err != nil {
			return problems, checked, err
		}
		n++
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			problems = append(problems, Problem{Doc: n, Object: "?", Message: "not valid YAML: " + err.Error()})
			continue
		}
		if obj == nil {
			continue
		}
		msgs, err := v.Object(ctx, obj)
		if errors.Is(err, ErrNotAmbit) {
			continue
		}
		checked++
		for _, m := range msgs {
			problems = append(problems, Problem{Doc: n, Object: name(obj), Message: m})
		}
	}
}

func name(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	return fmt.Sprintf("%v/%v", obj["kind"], md["name"])
}

// Object validates one object and returns human-readable problems (empty when valid).
func (v *Validator) Object(ctx context.Context, obj map[string]any) ([]string, error) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	if !strings.HasPrefix(apiVersion, "ambitproject.dev/") {
		return nil, ErrNotAmbit
	}
	k, ok := v.kinds[apiVersion+"/"+kind]
	if !ok {
		return []string{fmt.Sprintf("unknown kind %s %s for this version of Ambit", apiVersion, kind)}, nil
	}
	var out []string
	md, _ := obj["metadata"].(map[string]any)
	if n, _ := md["name"].(string); n == "" {
		out = append(out, "metadata.name is required")
	}
	if _, ok := md["namespace"]; ok {
		out = append(out, "metadata.namespace should be left out; the Argo CD Application sets it")
	}
	defaulting.Default(obj, k.structural)
	opts := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
	for _, p := range pruning.PruneWithOptions(obj, k.structural, true, opts) {
		out = append(out, "unknown field "+p)
	}
	for _, e := range apivalidation.ValidateCustomResource(nil, obj, k.schema) {
		out = append(out, e.Error())
	}
	errs, _ := k.cel.Validate(ctx, nil, k.structural, obj, nil, celconfig.RuntimeCELCostBudget)
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out, nil
}
