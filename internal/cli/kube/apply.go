package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldManager is the server-side apply field manager of every object the CLI applies (spec 4.8).
const FieldManager = "ambit"

// Decode splits a multi-document YAML into objects, skipping empty documents.
func Decode(manifest []byte) ([]*unstructured.Unstructured, error) {
	d := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var objs []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		err := d.Decode(&u.Object)
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, err
		}
		if len(u.Object) > 0 {
			objs = append(objs, u)
		}
	}
}

// Apply server-side applies every object in manifest with force-conflicts, as `kubectl apply --server-side
// --force-conflicts` does (the docs and `make install-crds`). Namespaced objects without a namespace go into ns.
// It never deletes anything.
func Apply(ctx context.Context, c client.Client, ns string, manifest []byte) error {
	objs, err := Decode(manifest)
	if err != nil {
		return err
	}
	for _, u := range objs {
		if u.GetNamespace() == "" && ns != "" {
			namespaced, err := c.IsObjectNamespaced(u)
			if err != nil {
				return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
			}
			if namespaced {
				u.SetNamespace(ns)
			}
		}
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

// CRDNames returns the names of the CustomResourceDefinitions in manifest.
func CRDNames(manifest []byte) ([]string, error) {
	objs, err := Decode(manifest)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, u := range objs {
		if u.GetKind() == "CustomResourceDefinition" {
			names = append(names, u.GetName())
		}
	}
	return names, nil
}

// WaitEstablished polls every second until each named CRD has the condition Established=True.
func WaitEstablished(ctx context.Context, c client.Client, names []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var waiting []string
		for _, n := range names {
			var crd apiextensionsv1.CustomResourceDefinition
			if err := c.Get(ctx, client.ObjectKey{Name: n}, &crd); err != nil || !established(&crd) {
				waiting = append(waiting, n)
			}
		}
		if len(waiting) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("CRDs not Established after %s: %v", timeout, waiting)
		case <-time.After(time.Second):
		}
	}
}

func established(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}
