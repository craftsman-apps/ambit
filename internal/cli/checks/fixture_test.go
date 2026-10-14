package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

const ns = "ambit-system"

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

type fakeHelm struct {
	rel       map[string]*helmx.Release
	user, all map[string]any
	err       error
}

func (f *fakeHelm) Apply(context.Context, string, string, string, map[string]any) (helmx.Release, error) {
	return helmx.Release{}, errors.New("checks never install")
}
func (f *fakeHelm) Get(n string) (*helmx.Release, error) { return f.rel[n], f.err }
func (f *fakeHelm) Values(_ string, all bool) (map[string]any, error) {
	if all {
		return f.all, f.err
	}
	return f.user, f.err
}

var forbidden = apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", errors.New("RBAC: access denied"))

// fx describes one test cluster. kubeObjs go to the client-go fake, ctrlObjs to the controller-runtime fake.
type fx struct {
	kubeObjs, ctrlObjs []runtime.Object
	helm               *fakeHelm
	ctrlDenied         bool // every controller-runtime read is forbidden
	kubeDenied         bool // every client-go call is forbidden
	allowed            bool // SelfSubjectAccessReviews answer allowed
	minor              string
	values             map[string]any // check -f
	forward            http.HandlerFunc
	forwardErr         error
	deep               bool
	token              string
	deepWait           time.Duration
	bundle             string
}

func (f fx) env(t *testing.T) *Env {
	cs := fake.NewSimpleClientset(f.kubeObjs...)
	minor := f.minor
	if minor == "" {
		minor = "31"
	}
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: minor, GitVersion: "v1." + minor + ".0"}
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview).DeepCopy()
		r.Status.Allowed = f.allowed
		return true, r, nil
	})
	if f.kubeDenied {
		cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
	}
	b := crfake.NewClientBuilder().WithScheme(kube.Scheme).WithRuntimeObjects(f.ctrlObjs...)
	if f.ctrlDenied {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return forbidden
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return forbidden
			},
		})
	}
	h := f.helm
	if h == nil {
		h = &fakeHelm{}
	}
	e := &Env{Kube: cs, Ctrl: b.Build(), Helm: h, Namespace: ns, ValuesFile: f.values, CRDBundle: []byte(f.bundle),
		Deep: f.deep, Token: f.token, HTTP: http.DefaultClient, Now: func() time.Time { return now }, DeepWait: f.deepWait}
	e.Forward = func(context.Context, string, int) (int, func(), error) {
		if f.forwardErr != nil {
			return 0, nil, f.forwardErr
		}
		s := httptest.NewServer(f.forward)
		t.Cleanup(s.Close)
		return s.Listener.Addr().(*net.TCPAddr).Port, func() {}, nil
	}
	return e
}

// one runs the check with the given ID.
func one(t *testing.T, id string, f fx) Result {
	t.Helper()
	for _, c := range Catalogue {
		if c.ID == id {
			return runOne(context.Background(), f.env(t), c)
		}
	}
	t.Fatalf("no check %s", id)
	return Result{}
}

func want(t *testing.T, r Result, s Status, contains string) {
	t.Helper()
	if r.Status != s || !strings.Contains(r.Message, contains) {
		t.Errorf("%s: %s %q; want %s containing %q", r.ID, r.Status, r.Message, s, contains)
	}
}

func readyPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func deploy(name string, labels map[string]string, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Status: appsv1.DeploymentStatus{ReadyReplicas: ready}}
}

var gwLabels = map[string]string{"gateway.networking.k8s.io/gateway-name": "ambit"}

func readyCond() []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}
}

func model(name, provider string, ref string, ready bool) *ambitv1alpha1.Model {
	m := &ambitv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ambitv1alpha1.ModelSpec{Provider: provider, Model: name + "-id"}}
	if ref != "" {
		m.Spec.CredentialsRef = &ambitv1alpha1.SecretRef{Name: ref}
	}
	if ready {
		m.Status.Conditions = readyCond()
	}
	return m
}

func secret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
}

func crd(name, group string, established bool, annotations map[string]string, versions ...string) *apiextensionsv1.CustomResourceDefinition {
	c := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: group}}
	for _, v := range versions {
		c.Spec.Versions = append(c.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{Name: v, Served: true})
	}
	if established {
		c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
	}
	return c
}

// certPEM is a self-signed certificate for hosts that expires at notAfter.
func certPEM(t *testing.T, notAfter time.Time, hosts ...string) []byte {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hosts[0]}, DNSNames: hosts,
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
