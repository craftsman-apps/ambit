package console

import (
	"bytes"
	"database/sql/driver"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// withSCIM gives the fixture SCIM with offboarding records saved as the console saves them (sealed Secrets).
func withSCIM(t *testing.T, e *evidenceFixture, recs ...OffboardingRecord) {
	t.Helper()
	e.cat.SCIM = &SCIM{Client: e.kube, Namespace: testNS, Conns: &Connections{Store: &GrantStore{Client: e.kube, Namespace: "ambit-grants",
		Key: []byte("fedcba9876543210fedcba9876543210")}}}
	for _, r := range recs {
		e.cat.SCIM.saveRecord(t.Context(), r)
	}
}

// keyEvent writes one key history event at a given time, as KeyService.event does.
func keyEvent(t *testing.T, e *evidenceFixture, at time.Time, action, email string) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-event-" + strings.ReplaceAll(action, "-", "") + at.Format("150405"), Namespace: testNS,
		Labels: map[string]string{keyEventLabel: "true"}, Annotations: map[string]string{keyEventAtAnn: at.Format(time.RFC3339), keyEventActionAnn: action}},
		Data: map[string]string{"actor": "scim", "action": action, "keyId": "k1", "email": email, "detail": ""}}
	if err := e.kube.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
}

func csvFiles(t *testing.T, body []byte) (map[string][][]string, manifest) {
	t.Helper()
	files := readZip(t, body)
	var m manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	out := map[string][][]string{}
	for name, b := range files {
		if name != "manifest.json" {
			out[name], _ = csv.NewReader(bytes.NewReader(b)).ReadAll()
		}
	}
	return out, m
}

func TestOffboardingExport(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	ch := goodCH(0)
	// ana: last accepted call the day before, none after, refused at +1 min. bob: two accepted calls after.
	ch.stats = [][]driver.Value{{at.Add(-24 * time.Hour), int64(0), at.Add(time.Minute)}, {time.Unix(0, 0), int64(2), time.Unix(0, 0)}}
	ch.after = [][][]driver.Value{{seededCall(0), seededCall(5)}}
	e := newEvidenceFixture(t, ch)
	withSCIM(t, e,
		OffboardingRecord{Email: "ana@example.com", At: at, Trigger: "scim PATCH active=false", SessionsEnded: 2, GatewayBlocked: true, KeysDeleted: 1,
			Grants: []GrantResult{{Connection: "jira", RevokedAtProvider: true}, {Connection: "=github", Error: "@provider said no, \"twice\"\n"}}, CompleteSuccess: true},
		OffboardingRecord{Email: "bob@example.com", At: at.Add(48 * time.Hour), Trigger: "-scim DELETE", GatewayBlocked: true, KeysError: "+timeout", Grants: []GrantResult{}},
		OffboardingRecord{Email: "old@example.com", At: at.AddDate(0, -2, 0), Trigger: "scim DELETE", Grants: []GrantResult{}}, // before the period
	)
	keyEvent(t, e, at.Add(time.Second), "offboard-delete", "ana@example.com")
	keyEvent(t, e, at.Add(2*time.Second), "create", "ana@example.com") // not an offboarding action
	keyEvent(t, e, at.AddDate(0, -1, 0), "revoke", "ana@example.com")  // before the period
	keyEvent(t, e, at.Add(3*time.Second), "exposed", "svc-ci@service.ambit")

	resp, body, err := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	if resp.Header.Get("Content-Disposition") != `attachment; filename="ambit-offboarding-2026-10-01-2026-10-31.zip"` {
		t.Errorf("name %s", resp.Header.Get("Content-Disposition"))
	}
	files, m := csvFiles(t, body)
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, " ") != "offboarding.csv grants-revoked.csv calls-after.csv key-events.csv" {
		t.Errorf("files %v", names)
	}
	off := files["offboarding.csv"]
	if len(off) != 3 {
		t.Fatalf("offboarding.csv %v", off)
	}
	want1 := []string{"ana@example.com", "2026-10-02T09:00:00Z", "scim PATCH active=false", "2", "true", "1", "0", "", "true", "2026-10-01T09:00:00Z", "0", "2026-10-02T09:01:00Z"}
	want2 := []string{"bob@example.com", "2026-10-04T09:00:00Z", "'-scim DELETE", "0", "true", "0", "0", "'+timeout", "false", "", "2", ""}
	if strings.Join(off[1], "|") != strings.Join(want1, "|") || strings.Join(off[2], "|") != strings.Join(want2, "|") {
		t.Errorf("offboarding.csv\n%v\n%v", off[1], off[2])
	}
	g := files["grants-revoked.csv"]
	if len(g) != 3 || g[2][2] != "'=github" || g[2][4] != "'@provider said no, \"twice\"\n" || g[1][3] != "true" {
		t.Errorf("grants-revoked.csv %v", g)
	}
	after := files["calls-after.csv"]
	if len(after) != 3 || after[0][0] != "offboarded_email" || after[1][0] != "bob@example.com" || after[1][1] != "2026-10-04T09:00:00Z" || len(after[1]) != len(callsHeader)+2 {
		t.Errorf("calls-after.csv %v", after)
	}
	ke := files["key-events.csv"]
	if len(ke) != 3 || ke[1][2] != "offboard-delete" || ke[2][2] != "exposed" {
		t.Errorf("key-events.csv %v", ke)
	}
	if m.Sources["offboardingRecords"].(map[string]any)["since"] != "2026-08-02T09:00:00Z" || m.Parameters["grace"] != "5s" {
		t.Errorf("sources %v parameters %v", m.Sources, m.Parameters)
	}
	if !strings.Contains(strings.Join(m.Notes, " "), "accepted_calls_after counts") {
		t.Errorf("notes %v", m.Notes)
	}
}

// Without the audit store: the record columns are there, the audit columns are empty, calls-after.csv is not, and
// the manifest says why.
func TestOffboardingExportWithoutAuditStore(t *testing.T) {
	e := newEvidenceFixture(t, nil)
	withSCIM(t, e, OffboardingRecord{Email: "ana@example.com", At: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), Trigger: "scim DELETE", Grants: []GrantResult{}})
	resp, body, err := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	files, m := csvFiles(t, body)
	if _, ok := files["calls-after.csv"]; ok || files["offboarding.csv"][1][10] != "" || !strings.Contains(strings.Join(m.Notes, " "), "audit store is off") {
		t.Errorf("files %v notes %v", files, m.Notes)
	}
}

func TestOffboardingExportRefusals(t *testing.T) {
	e := newEvidenceFixture(t, goodCH(0))
	if resp, body, _ := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31", e.admin); resp.StatusCode != 404 || !strings.Contains(string(body), "SCIM") {
		t.Errorf("no SCIM: %d %s", resp.StatusCode, body)
	}
	withSCIM(t, e)
	if resp, body, _ := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31&email=ana%40example.com", e.admin); resp.StatusCode != 400 || !strings.Contains(string(body), "only for the audit") {
		t.Errorf("email: %d %s", resp.StatusCode, body)
	}
	if resp, _, _ := e.get(t, t.Context(), "/api/evidence/offboarding.zip?from=2026-10-01&to=2026-10-31", as(e.auth, RoleViewer)); resp.StatusCode != 403 {
		t.Errorf("viewer: %d", resp.StatusCode)
	}
}
