// Package v1alpha1 holds Ambit's API: the objects people write (in the console or in Git).
// +kubebuilder:object:generate=true
// +groupName=ambit.craftsmanapps.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "ambit.craftsmanapps.com", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
