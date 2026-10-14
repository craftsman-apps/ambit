//go:build interop

package helmx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelm3Interop is the Helm SDK gate of the spec (sections 5 and 9.2a). It needs a cluster (KUBECONFIG) and
// Helm 3 on PATH. The CLI's SDK installs and upgrades a release; Helm 3 then lists it, reads its history and
// values, rolls it back and upgrades it; the CLI upgrades it again over Helm 3's revision.
//
//	KUBECONFIG=<file> go test -tags interop -run TestHelm3Interop -v ./internal/cli/helmx
func TestHelm3Interop(t *testing.T) {
	const ns, name = "ambit-interop", "interop"
	run := func(tool string, args ...string) string {
		t.Helper()
		out, err := exec.Command(tool, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", tool, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	helm := func(args ...string) string { return run("helm", append(args, "-n", ns)...) }
	greeting := func() string {
		return run("kubectl", "-n", ns, "get", "configmap", name+"-greeting", "-o", "jsonpath={.data.greeting}")
	}
	// dump logs the state of every revision and of the live ConfigMap. It never fails the test.
	dump := func(label string, nrev int) {
		sh := func(tool string, args ...string) string {
			out, err := exec.Command(tool, args...).CombinedOutput()
			if err != nil {
				return fmt.Sprintf("(error: %v)\n%s", err, out)
			}
			return string(out)
		}
		t.Logf("=== %s: helm history\n%s", label, sh("helm", "history", name, "-n", ns))
		for n := 1; n <= nrev; n++ {
			r := fmt.Sprint(n)
			t.Logf("--- %s: revision %d user values: %s", label, n, strings.TrimSpace(sh("helm", "get", "values", name, "-n", ns, "--revision", r, "-o", "json")))
			t.Logf("--- %s: revision %d all values: %s", label, n, strings.TrimSpace(sh("helm", "get", "values", name, "-n", ns, "--revision", r, "--all", "-o", "json")))
			t.Logf("--- %s: revision %d manifest:\n%s", label, n, sh("helm", "get", "manifest", name, "-n", ns, "--revision", r))
		}
		t.Logf("--- %s: every namespace that holds the ConfigMap\n%s", label, sh("kubectl", "get", "configmap", "-A", "--field-selector", "metadata.name="+name+"-greeting", "-o", "custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,GREETING:.data.greeting,MANAGERS:.metadata.managedFields[*].manager"))
		t.Logf("--- %s: live ConfigMap\n%s", label, sh("kubectl", "-n", ns, "get", "configmap", name+"-greeting", "-o", "yaml", "--show-managed-fields"))
	}
	if v := run("helm", "version", "--short"); !strings.HasPrefix(v, "v3.") {
		t.Fatalf("helm on PATH is %s; this gate needs Helm 3", v)
	}
	c, err := New(Options{Kubeconfig: os.Getenv("KUBECONFIG"), Namespace: ns, Log: logWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Two revisions written by the CLI's SDK.
	for i, g := range []string{"one", "two"} {
		t.Logf("SDK Apply #%d: name=%s ref=testdata/interop version=%q vals={greeting:%s}", i+1, name, "", g)
		r, err := c.Apply(ctx, name, "testdata/interop", "", map[string]any{"greeting": g})
		if err != nil {
			t.Fatalf("SDK revision %d: %v", i+1, err)
		}
		if r.Revision != i+1 {
			t.Fatalf("SDK revision = %d, want %d", r.Revision, i+1)
		}
	}

	dump("after SDK revisions 1-2", 2)

	// 2. Helm 3 reads them.
	if !strings.Contains(helm("list", "-q"), name) {
		t.Fatal("helm 3 list does not show the release")
	}
	var hist []map[string]any
	if err := json.Unmarshal([]byte(helm("history", name, "-o", "json")), &hist); err != nil || len(hist) != 2 {
		t.Fatalf("helm 3 history = %v (%v), want 2 revisions", hist, err)
	}
	if v := helm("get", "values", name, "-o", "json"); v != `{"greeting":"two"}` {
		t.Fatalf("helm 3 get values = %s", v)
	}

	// 3. Helm 3 rolls back to revision 1 (revision 3), then upgrades with the same values (revision 4).
	helm("rollback", name, "1", "--wait")
	if g := greeting(); g != "one" {
		t.Fatalf("after helm 3 rollback the ConfigMap says %q, want one", g)
	}
	helm("upgrade", name, "testdata/interop", "--reuse-values", "--wait")
	if g := greeting(); g != "one" {
		t.Fatalf("after helm 3 upgrade the ConfigMap says %q, want one", g)
	}

	dump("after Helm 3 rollback and upgrade (step 3)", 4)

	// 4. The CLI's SDK upgrades over Helm 3's revision (revision 5).
	t.Logf("SDK Apply over Helm 3: name=%s ref=testdata/interop version=%q vals={greeting:three}", name, "")
	r, err := c.Apply(ctx, name, "testdata/interop", "", map[string]any{"greeting": "three"})
	if err != nil {
		t.Fatalf("SDK upgrade over a Helm 3 revision: %v", err)
	}
	t.Logf("SDK Apply returned %+v", r)
	dump("after SDK upgrade (step 4)", 5)
	if r.Revision != 5 || r.Status != "deployed" {
		t.Fatalf("SDK upgrade = %+v, want revision 5 deployed", r)
	}
	if g := greeting(); g != "three" {
		t.Fatalf("after the SDK upgrade the ConfigMap says %q, want three", g)
	}
}

// logWriter sends the SDK's log lines to the test log.
type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Logf("sdk: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
