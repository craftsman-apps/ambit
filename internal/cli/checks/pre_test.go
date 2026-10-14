package checks

import (
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

func TestR1KubeVersion(t *testing.T) {
	want(t, one(t, "R1", fx{minor: "29"}), Fail, "older than 1.30")
	want(t, one(t, "R1", fx{minor: "30+"}), Pass, "Kubernetes 1.30")
	want(t, one(t, "R1", fx{minor: "33"}), Pass, "1.33")
}

func TestR2Permissions(t *testing.T) {
	want(t, one(t, "R2", fx{allowed: true}), Pass, "you can create")
	want(t, one(t, "R2", fx{allowed: false}), Fail, "you cannot create customresourcedefinitions, create namespaces, create clusterroles")
	want(t, one(t, "R2", fx{kubeDenied: true}), Unknown, "cannot create selfsubjectaccessreviews")
}

func TestR3Installed(t *testing.T) {
	rel := func(v string) *helmx.Release { return &helmx.Release{ChartVersion: v, Status: "deployed"} }
	want(t, one(t, "R3", fx{}), Pass, "fresh install")
	want(t, one(t, "R3", fx{helm: &fakeHelm{rel: map[string]*helmx.Release{"ambit-crds": rel("0.2.0"), "ambit": rel("0.2.0")}}}), Pass, "0.2.0 are installed")
	want(t, one(t, "R3", fx{helm: &fakeHelm{rel: map[string]*helmx.Release{"ambit-crds": rel("0.1.0"), "ambit": rel("0.2.0")}}}), Warn, "different versions")
	want(t, one(t, "R3", fx{helm: &fakeHelm{rel: map[string]*helmx.Release{"ambit-crds": rel("0.2.0")}}}), Warn, "only ambit-crds")
	want(t, one(t, "R3", fx{helm: &fakeHelm{err: forbidden}}), Unknown, "cannot get secrets in ambit-system")
}

func TestR4StorageClass(t *testing.T) {
	bundled := map[string]any{"evidence": map[string]any{"enabled": true}}
	def := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}}
	want(t, one(t, "R4", fx{}), NA, "no values file")
	want(t, one(t, "R4", fx{values: map[string]any{"evidence": map[string]any{"enabled": false}}}), NA, "not used")
	want(t, one(t, "R4", fx{values: bundled, ctrlObjs: []runtime.Object{def}}), Pass, "default StorageClass standard")
	want(t, one(t, "R4", fx{values: bundled}), Fail, "no default StorageClass")
	want(t, one(t, "R4", fx{values: bundled, ctrlDenied: true}), Unknown, "cannot list storageclasses")
}

func TestR5TLSSecret(t *testing.T) {
	tls := func(name string) map[string]any {
		return map[string]any{"gateway": map[string]any{"tls": map[string]any{"enabled": true, "certificateSecret": name}}}
	}
	want(t, one(t, "R5", fx{}), NA, "no values file")
	want(t, one(t, "R5", fx{values: map[string]any{}}), NA, "gateway.tls.enabled is false")
	want(t, one(t, "R5", fx{values: tls("")}), Fail, "certificateSecret is empty")
	want(t, one(t, "R5", fx{values: tls("ambit-tls")}), Fail, "does not exist")
	want(t, one(t, "R5", fx{values: tls("ambit-tls"), ctrlObjs: []runtime.Object{secret("ambit-tls", nil)}}), Pass, "exists")
	want(t, one(t, "R5", fx{values: tls("ambit-tls"), ctrlDenied: true}), Unknown, "cannot get secrets in ambit-system")
}

const gwBundle = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gateways.gateway.networking.k8s.io
  annotations: {gateway.networking.k8s.io/bundle-version: v1.6.2}
spec:
  group: gateway.networking.k8s.io
  versions: [{name: v1, served: true}]
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: mcpservers.toolhive.stacklok.dev}
spec:
  group: toolhive.stacklok.dev
  versions: [{name: v1alpha1, served: true}, {name: v1beta1, served: true}]
`

func TestR6ThirdPartyCRDs(t *testing.T) {
	gw := func(v string) runtime.Object {
		return crd("gateways.gateway.networking.k8s.io", "gateway.networking.k8s.io", true, map[string]string{gatewayAPIVersionAnnotation: v}, "v1")
	}
	want(t, one(t, "R6", fx{bundle: gwBundle}), Pass, "no third-party CRDs yet")
	want(t, one(t, "R6", fx{bundle: gwBundle, ctrlObjs: []runtime.Object{gw("v1.6.2")}}), Pass, "1 third-party CRDs are present")
	want(t, one(t, "R6", fx{bundle: gwBundle, ctrlObjs: []runtime.Object{gw("v1.2.0")}}), Fail, "is Gateway API v1.2.0, v1.6.2 is required")
	old := crd("mcpservers.toolhive.stacklok.dev", "toolhive.stacklok.dev", true, nil, "v1alpha1")
	want(t, one(t, "R6", fx{bundle: gwBundle, ctrlObjs: []runtime.Object{old}}), Fail, "does not serve v1beta1")
	want(t, one(t, "R6", fx{bundle: gwBundle, ctrlDenied: true}), Unknown, "cannot get customresourcedefinitions")
}
