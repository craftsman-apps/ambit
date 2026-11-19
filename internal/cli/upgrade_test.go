package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

func installed(version, status string, updated time.Time) []helmx.Release {
	return []helmx.Release{
		{Name: "ambit", Namespace: "ambit-system", Chart: "ambit", ChartVersion: version, Status: status, Revision: 8, Updated: updated},
		{Name: "ambit-crds", Namespace: "ambit-system", Chart: "ambit-crds", ChartVersion: version, Status: "deployed", Revision: 3, Updated: updated},
	}
}

func TestUpgradeHappyPath(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.4.0", "deployed", time.Now().Add(-time.Hour))
	r.tty, r.stdin = true, "y\n"
	code, s := r.run("upgrade", "-f", r.path("values.yaml"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
	for _, want := range []string{
		"Upgrade Ambit 0.4.0 → 0.5.0 in context prod-eu (https://1.2.3.4:6443), namespace ambit-system\nUpgrade? [y/N]",
		"To the release with agentgateway v1.7.0 (0.4.0)", // not: 0.4.0 is installed
		"✓ Chart ambit 0.5.0", "✓ Ambit 0.5.0 is upgraded in context prod-eu",
	} {
		if want == "To the release with agentgateway v1.7.0 (0.4.0)" {
			if strings.Contains(s, want) {
				t.Errorf("a note of the installed release is listed:\n%s", s)
			}
			continue
		}
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if len(r.helm.applyOpts) != 2 || r.helm.applyOpts[1].ResetThenReuseValues || r.helm.applyOpts[1].RecoverPending {
		t.Fatalf("apply options = %+v", r.helm.applyOpts)
	}
}

// 3.3 values rule: neither -f nor --reuse-values → exit 2; both → exit 2.
func TestUpgradeValuesRule(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.4.0", "deployed", time.Now())
	if code, s := r.run("upgrade", "--yes"); code != 2 || !strings.Contains(s, "--reuse-values") {
		t.Fatalf("neither: exit %d\n%s", code, s)
	}
	if code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--reuse-values", "--yes"); code != 2 || !strings.Contains(s, "cannot be used together") {
		t.Fatalf("both: exit %d\n%s", code, s)
	}
	if r.wrote() {
		t.Fatal("a refused upgrade wrote")
	}
}

// --reuse-values: the stored values decide (Helm's --reset-then-reuse-values); the Secret names come from them.
func TestUpgradeReuseValues(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.5.0", "deployed", time.Now())
	r.helm.values = map[string]map[string]any{"ambit/user": {"gateway": map[string]any{"tls": map[string]any{"certificateSecret": "stored-tls"}}}}
	os.WriteFile(r.path("crt"), []byte("CERTIFICATE-1234567890"), 0o600)
	code, s := r.run("upgrade", "--reuse-values", "--yes", "--secret-file", "stored-tls/tls.crt="+r.path("crt"))
	if code != 0 || !strings.Contains(s, "applies the same version again (a repair)") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	if !r.helm.applyOpts[1].ResetThenReuseValues || r.helm.applyVals[1] != nil {
		t.Fatalf("apply = %+v %+v", r.helm.applyOpts, r.helm.applyVals)
	}
}

// 3.3 downgrade: installed 0.6.0, CLI 0.5.0 → exit 1, no write.
func TestUpgradeRefusesDowngrade(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.6.0", "deployed", time.Now())
	code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--yes")
	if code != 1 || !strings.Contains(s, "use an ambit CLI of version 0.6.0, or helm rollback ambit <revision>") || r.wrote() {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// Not installed: "use ambit install"; installed by Argo CD: refused.
func TestUpgradeNotInstalled(t *testing.T) {
	r := newInstallRig(t)
	if code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--yes"); code != 1 || !strings.Contains(s, "ambit install -f values.yaml") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// 6.3 pending: refused without --recover-pending; with it, 3 minutes old → refused with the wait time; 20 minutes
// old → asked, then marked failed with the note and upgraded.
func TestUpgradePendingRelease(t *testing.T) {
	r := newInstallRig(t)
	started := time.Now().Add(-3 * time.Minute)
	r.helm.list = installed("0.4.0", "pending-upgrade", started)
	code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--yes")
	if code != 1 || !strings.Contains(s, "the release ambit is pending-upgrade since") || !strings.Contains(s, "ambit upgrade --recover-pending") || r.wrote() {
		t.Fatalf("no flag: exit %d\n%s", code, s)
	}
	code, s = r.run("upgrade", "-f", r.path("values.yaml"), "--yes", "--recover-pending")
	wait := started.Add(15 * time.Minute).UTC().Format("15:04")
	if code != 1 || !strings.Contains(s, "revision 8 of ambit started 3m0s ago") || !strings.Contains(s, "wait until "+wait+" UTC") || r.wrote() {
		t.Fatalf("3 minutes: exit %d\n%s", code, s)
	}
	r.helm.list = installed("0.4.0", "pending-upgrade", time.Now().Add(-20*time.Minute))
	r.tty, r.stdin = true, "y\nn\n"
	code, s = r.run("upgrade", "-f", r.path("values.yaml"), "--recover-pending")
	if code != 1 || !strings.Contains(s, "Mark revision 8 of ambit as failed and upgrade? [y/N]") || r.wrote() {
		t.Fatalf("20 minutes, answer n: exit %d\n%s", code, s)
	}
	r.stdin = "y\ny\n"
	code, s = r.run("upgrade", "-f", r.path("values.yaml"), "--recover-pending")
	if code != 0 || !r.helm.applyOpts[1].RecoverPending || r.helm.applyOpts[1].RecoverNote != recoverNote {
		t.Fatalf("20 minutes, answer y: exit %d %+v\n%s", code, r.helm.applyOpts, s)
	}
}

// 6.1 discovery: an install made by hand with other release names is upgraded under those names.
// 5.3: the target is printed also with --yes (no question).
func TestUpgradePrintsTargetWithYes(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.4.0", "deployed", time.Now())
	code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--yes")
	if code != 0 || !strings.Contains(s, "Upgrade Ambit 0.4.0 → 0.5.0 in context prod-eu (https://1.2.3.4:6443), namespace ambit-system\n") || strings.Contains(s, "[y/N]") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

func TestUpgradeUsesDiscoveredNames(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = []helmx.Release{{Name: "gw", Chart: "ambit", ChartVersion: "0.4.0", Status: "deployed", Namespace: "ambit-system"},
		{Name: "gw-crds", Chart: "ambit-crds", ChartVersion: "0.4.0", Status: "deployed", Namespace: "ambit-system"}}
	code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--yes")
	if code != 0 || !strings.Contains(s, "Found the release gw of chart ambit") ||
		strings.Join(r.helm.applied, ",") != "gw-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds 0.5.0,gw oci://ghcr.io/craftsman-apps/charts/ambit 0.5.0" {
		t.Fatalf("exit %d, applied %v\n%s", code, r.helm.applied, s)
	}
}

func TestUpgradeDryRun(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = installed("0.4.0", "deployed", time.Now())
	applied := 0
	r.ctrl = fakeCtrl(&applied)
	code, s := r.run("upgrade", "-f", r.path("values.yaml"), "--dry-run")
	if code != 0 || !strings.Contains(s, "Would upgrade ambit-crds 0.5.0") || strings.Contains(s, "[y/N]") || r.wrote() || applied != 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// The notes table names headings that exist in docs/upgrading.md, in release order.
func TestUpgradeNotesMatchTheDocs(t *testing.T) {
	b, err := os.ReadFile("../../docs/upgrading.md")
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range upgradeNotes {
		if !strings.Contains(string(b), "\n## "+n.heading+"\n") {
			t.Errorf("docs/upgrading.md has no heading %q", n.heading)
		}
		if i > 0 && semverLess(n.version, upgradeNotes[i-1].version) {
			t.Errorf("upgradeNotes is not in release order at %s", n.version)
		}
	}
	if got := upgradeLinks("0.2.0", "0.4.0"); len(got) != 3 || !strings.Contains(got[1], "#to-the-release-with-access-rules-that-name-people") ||
		!strings.Contains(got[2], "#to-the-release-with-agentgateway-v170") {
		t.Fatalf("links = %q", got)
	}
}

func TestUpgradeLinksNameEachVersionOnce(t *testing.T) {
	got := upgradeLinks("0.4.0", "0.5.0")
	if len(got) != 2 || !strings.HasSuffix(got[1], "service accounts (0.5.0): https://ambitproject.dev/docs/upgrading#to-the-release-with-service-accounts-050") {
		t.Fatalf("links = %q", got)
	}
}
