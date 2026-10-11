package console

// Admission fencing for personal API key objects, against a real API server (envtest). The rendered
// chart template is applied as is. Run with `make test-go`; plain `go test` skips it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	admNS           = "ambit-system"
	admConsoleUser  = "system:serviceaccount:ambit-system:ambit-console"
	admNSController = "system:serviceaccount:kube-system:namespace-controller"
	msgWriter       = "only the Ambit console may write personal API key objects"
	msgLease        = "only the Ambit console may change the key Lease"
	msgHolder       = "only the Ambit console holding the key Lease may write personal API key objects"
)

func admClient(t *testing.T, base *rest.Config, user, pod string) kubernetes.Interface {
	t.Helper()
	c := rest.CopyConfig(base)
	c.Impersonate = rest.ImpersonationConfig{UserName: user}
	if pod != "" {
		c.Impersonate.Extra = map[string][]string{"authentication.kubernetes.io/pod-name": {pod}}
	}
	k, err := kubernetes.NewForConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func keySecret(name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: admNS, Labels: labels}, StringData: map[string]string{"key": "x"}}
}

func keyConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: admNS, Labels: map[string]string{"ambitproject.dev/key-event": "true"}}, Data: map[string]string{"e": "x"}}
}

func TestAdmissionFencesKeyObjects(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (run make test-go)")
	}
	rendered, err := exec.Command("helm", "template", "ambit", "../../charts/ambit", "-n", admNS, "-f", "../../deploy/quickstart.yaml",
		"--show-only", "templates/keys/admission.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, rendered)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer env.Stop()
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: admNS, Labels: map[string]string{"kubernetes.io/metadata.name": admNS}}}))
	// Everyone may write Secrets and ConfigMaps by RBAC; admission is what is under test.
	must(admin.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "writers"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"}, Verbs: []string{"*"}}, {APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"*"}}}}))
	must(admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "writers"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "writers"},
		Subjects: []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "system:authenticated"}}}))
	for _, doc := range strings.Split(string(rendered), "\n---") {
		var u unstructured.Unstructured
		must(yaml.Unmarshal([]byte(doc), &u.Object))
		if len(u.Object) == 0 {
			continue
		}
		must(admin.Create(ctx, &u))
	}

	adm := admClient(t, cfg, "admin", "")
	other := admClient(t, cfg, "someone", "")
	podA := admClient(t, cfg, admConsoleUser, "pod-a")
	podB := admClient(t, cfg, admConsoleUser, "pod-b")
	noPod := admClient(t, cfg, admConsoleUser, "")
	nsCtl := admClient(t, cfg, admNSController, "")
	keyLabels := map[string]string{"ambitproject.dev/key": "true"}
	sec := func(c kubernetes.Interface) typedcorev1.SecretInterface { return c.CoreV1().Secrets(admNS) }
	refused := func(err error, msg string) bool { return err != nil && strings.Contains(err.Error(), msg) }
	n := 0
	fresh := func(p string) string { n++; return fmt.Sprintf("%s-%d", p, n) }
	del := func(c kubernetes.Interface, name string) error {
		return c.CoreV1().Secrets(admNS).Delete(ctx, name, metav1.DeleteOptions{})
	}
	// Policies and bindings reach the admission plugin through informers: poll until a write that must be
	// refused is refused (remove it, as the namespace controller, whenever it was let through).
	waitRefused := func(what string, c kubernetes.Interface, msg string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			name := fresh("probe")
			_, err := sec(c).Create(ctx, keySecret(name, keyLabels), metav1.CreateOptions{})
			if refused(err, msg) {
				return
			}
			if err == nil {
				_ = del(nsCtl, name)
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: still not refused with %q after 60 s (last error: %v)", what, msg, err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// 1. No Lease.
	waitRefused("admin writes a key Secret (writer policy)", adm, msgWriter)
	// Each binding has its own object selector; wait for all three.
	waitUntil := func(what string, refusedNow func() bool) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for !refusedNow() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: still not refused after 60 s", what)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	waitUntil("owner-record binding", func() bool {
		name := fresh("probe")
		_, err := sec(adm).Create(ctx, keySecret(name, map[string]string{"ambitproject.dev/key-owner-record": "true"}), metav1.CreateOptions{})
		if err == nil {
			_ = del(nsCtl, name)
		}
		return refused(err, msgWriter)
	})
	waitUntil("key-event binding", func() bool {
		name := fresh("probe")
		_, err := adm.CoreV1().ConfigMaps(admNS).Create(ctx, keyConfigMap(name), metav1.CreateOptions{})
		if err == nil {
			_ = adm.CoreV1().ConfigMaps(admNS).Delete(ctx, name, metav1.DeleteOptions{})
		}
		return refused(err, msgWriter)
	})
	if _, err := sec(other).Create(ctx, keySecret("plain", nil), metav1.CreateOptions{}); err != nil {
		t.Errorf("unlabelled Secret by any user must succeed: %v", err)
	}
	if _, err := other.CoreV1().ConfigMaps(admNS).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: admNS}}, metav1.CreateOptions{}); err != nil {
		t.Errorf("unlabelled ConfigMap by any user must succeed: %v", err)
	}
	if _, err := sec(noPod).Create(ctx, keySecret(fresh("nopod"), keyLabels), metav1.CreateOptions{}); !refused(err, msgWriter) {
		t.Errorf("console user without a pod name must be refused by the writer policy, got %v", err)
	}
	if _, err := sec(adm).Create(ctx, keySecret(fresh("adm"), map[string]string{"ambitproject.dev/key-owner-record": "true"}), metav1.CreateOptions{}); !refused(err, msgWriter) {
		t.Errorf("admin writing an owner record must be refused, got %v", err)
	}
	if _, err := adm.CoreV1().ConfigMaps(admNS).Create(ctx, keyConfigMap(fresh("ev")), metav1.CreateOptions{}); !refused(err, msgWriter) {
		t.Errorf("admin writing a key event must be refused, got %v", err)
	}
	noLease := fresh("nolease")
	if _, err := sec(podA).Create(ctx, keySecret(noLease, keyLabels), metav1.CreateOptions{}); err != nil {
		t.Errorf("console pod without a Lease present must be allowed (holder policy not applied): %v", err)
	}
	if err := del(nsCtl, noLease); err != nil {
		t.Errorf("namespace controller must delete a key Secret with no Lease: %v", err)
	}

	// 3. Relabelling an unlabelled Secret into a key is refused, for an admin.
	g, err := sec(other).Create(ctx, keySecret("relabel", nil), metav1.CreateOptions{})
	must(err)
	g.Labels = keyLabels
	if _, err := adm.CoreV1().Secrets(admNS).Update(ctx, g, metav1.UpdateOptions{}); !refused(err, msgWriter) {
		t.Errorf("relabelling into a key by an admin must be refused, got %v", err)
	}

	// 2. Lease held by pod-a.
	holder := "pod-a"
	must(admin.Create(ctx, &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-writer", Namespace: admNS}, Spec: coordv1.LeaseSpec{HolderIdentity: &holder}}))
	waitRefused("pod-b writes while pod-a holds the Lease (holder policy)", podB, msgHolder)
	leaseGone := func(c kubernetes.Interface) error {
		return c.CoordinationV1().Leases(admNS).Delete(ctx, "ambit-key-writer", metav1.DeleteOptions{})
	}
	waitUntil("Lease policy", func() bool { return refused(leaseGone(adm), msgLease) })
	if l, err := getLease(ctx, adm); err != nil {
		t.Fatal(err)
	} else {
		if _, err := adm.CoordinationV1().Leases(admNS).Update(ctx, l, metav1.UpdateOptions{}); !refused(err, msgLease) {
			t.Errorf("admin Lease update must be refused, got %v", err)
		}
		if _, err := podA.CoordinationV1().Leases(admNS).Update(ctx, l, metav1.UpdateOptions{}); err != nil {
			t.Errorf("console Lease update must be allowed: %v", err)
		}
	}
	name := fresh("held")
	s, err := sec(podA).Create(ctx, keySecret(name, keyLabels), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("holder pod-a must create: %v", err)
	}
	s.Data = map[string][]byte{"key": []byte("y")}
	if _, err := podA.CoreV1().Secrets(admNS).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Errorf("holder pod-a must update: %v", err)
	}
	if _, err := podA.CoreV1().ConfigMaps(admNS).Create(ctx, keyConfigMap(fresh("ev")), metav1.CreateOptions{}); err != nil {
		t.Errorf("holder pod-a must write a key event: %v", err)
	}
	if err := del(podB, name); !refused(err, msgHolder) {
		t.Errorf("pod-b delete must be refused by the holder policy, got %v", err)
	}
	if err := del(adm, name); !refused(err, msgWriter) {
		t.Errorf("admin delete must be refused, got %v", err)
	}
	if err := del(nsCtl, name); err != nil {
		t.Errorf("namespace controller must still delete with the Lease present: %v", err)
	}
	name = fresh("held")
	if _, err := sec(podA).Create(ctx, keySecret(name, keyLabels), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := del(podA, name); err != nil {
		t.Errorf("holder pod-a must delete: %v", err)
	}

	// 4. Lease absent: the holder policy no longer applies, the writer policy still does.
	if err := leaseGone(nsCtl); err != nil {
		t.Fatalf("namespace controller must delete the Lease: %v", err)
	}
	waitUntil("holder policy lifted", func() bool {
		name := fresh("probe")
		_, err := sec(podB).Create(ctx, keySecret(name, keyLabels), metav1.CreateOptions{})
		if err == nil {
			_ = del(nsCtl, name)
		}
		return err == nil
	})
	for _, c := range []kubernetes.Interface{podA, podB} {
		name := fresh("nolease")
		if _, err := sec(c).Create(ctx, keySecret(name, keyLabels), metav1.CreateOptions{}); err != nil {
			t.Errorf("console pod must write while the Lease is absent: %v", err)
		}
		_ = del(nsCtl, name)
	}
	if _, err := sec(adm).Create(ctx, keySecret(fresh("adm"), keyLabels), metav1.CreateOptions{}); !refused(err, msgWriter) {
		t.Errorf("admin must still be refused by the writer policy while the Lease is absent, got %v", err)
	}
}

func getLease(ctx context.Context, c kubernetes.Interface) (*coordv1.Lease, error) {
	return c.CoordinationV1().Leases(admNS).Get(ctx, "ambit-key-writer", metav1.GetOptions{})
}
