package console

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

func snapshotBody(t *testing.T, rules ...v1.AccessRule) string {
	t.Helper()
	type entry struct {
		Name string            `json:"name"`
		Spec v1.AccessRuleSpec `json:"spec"`
	}
	var out []entry
	for _, r := range rules {
		out = append(out, entry{r.Name, r.Spec})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func TestArgumentLimits(t *testing.T) {
	got := argumentLimits([]v1.ArgumentRule{{Name: "repo", OneOf: []string{"web", "api"}}, {Name: "per_page", Max: "100"}, {Name: "q", NotContains: []string{"secret"}}})
	if got != "repo oneOf [web, api]; per_page max 100; q notContains [secret]" {
		t.Error(got)
	}
}

func TestAccessExport(t *testing.T) {
	d := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rules := snapshotBody(t,
		v1.AccessRule{Spec: v1.AccessRuleSpec{Groups: []string{"eng", "=ops"}, Models: []string{"gpt-5"},
			Servers: []v1.ServerAccess{{Name: "github", Tools: []string{"create_issue", "list_issues"}, Arguments: []v1.ArgumentRule{{Name: "repo", OneOf: []string{"web", "api"}}}}, {Name: "time"}}}},
		v1.AccessRule{Spec: v1.AccessRuleSpec{Users: []string{"ana@example.com", "svc-ci@svc.ambit.internal"}, AllSignedIn: true, Models: []string{"gemma"}}},
	)
	// The test rules have no names in the body above: name them by editing the JSON.
	rules = strings.Replace(strings.Replace(rules, `"name":""`, `"name":"eng-models"`, 1), `"name":""`, `"name":"@everyone"`, 1)
	ch := goodCH(0)
	ch.snapshots = [][]driver.Value{
		{d.Add(10 * time.Hour), "111111111111", "aaaa", "audit", "[]"},
		{d.Add(15 * time.Hour), "3f9a1c0e2b7d", "bbbb", "enforce", rules},
	}
	ch.seen = [][]driver.Value{{"eng", "ana@example.com", d.AddDate(0, 0, -30), d.Add(9 * time.Hour), uint64(42)}}
	e := newEvidenceFixture(t, ch)
	e.cat.Evidence.Now = func() time.Time { return d.AddDate(0, 1, 0) }
	// Keys: one stored and alive, one stored but created after the date, one deleted after the date, one deleted before.
	for _, k := range []Key{
		{ID: "kalive", Email: "ana@example.com", Groups: []string{"eng"}, Created: d.AddDate(0, 0, -10), Until: d.AddDate(0, 1, 0), Scope: ScopeLLM, Hash: "h1"},
		{ID: "klater", Email: "ana@example.com", Created: d.AddDate(0, 0, 2), Until: d.AddDate(0, 1, 0), Scope: ScopeLLM, Hash: "h2"},
	} {
		sec := k.Secret(testNS)
		if err := e.kube.Create(t.Context(), sec); err != nil {
			t.Fatal(err)
		}
	}
	keyEventAt := func(at time.Time, action, id, email string) {
		t.Helper()
		ev := evidenceKeyEvent(at, action, id, email)
		if err := e.kube.Create(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	keyEventAt(d.AddDate(0, 0, -20), "create", "kgone", "svc-ci@svc.ambit.internal")
	keyEventAt(d.AddDate(0, 0, 3), "sa-delete-key", "kgone", "svc-ci@svc.ambit.internal")
	keyEventAt(d.AddDate(0, 0, -20), "create", "kearly", "bob@example.com")
	keyEventAt(d.AddDate(0, 0, -5), "revoke", "kearly", "bob@example.com")

	resp, body, err := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31", e.admin)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v %s", resp.StatusCode, err, body)
	}
	if resp.Header.Get("Content-Disposition") != `attachment; filename="ambit-access-2026-12-31.zip"` {
		t.Errorf("name %s", resp.Header.Get("Content-Disposition"))
	}
	files, m := csvFiles(t, body)
	g := files["grants.csv"]
	var lines []string
	for _, r := range g[1:] {
		lines = append(lines, strings.Join(r[3:], "|"))
		if r[0] != "2026-12-31T15:00:00Z" || r[1] != "3f9a1c0e2b7d" || r[2] != "bbbb" {
			t.Errorf("snapshot columns %v", r)
		}
	}
	want := []string{
		"eng-models|group|eng|model|gpt-5||enforce",
		"eng-models|group|eng|tool|github/create_issue|repo oneOf [web, api]|enforce",
		"eng-models|group|eng|tool|github/list_issues|repo oneOf [web, api]|enforce",
		"eng-models|group|eng|server|time||enforce",
		"eng-models|group|'=ops|model|gpt-5||enforce",
		"eng-models|group|'=ops|tool|github/create_issue|repo oneOf [web, api]|enforce",
		"eng-models|group|'=ops|tool|github/list_issues|repo oneOf [web, api]|enforce",
		"eng-models|group|'=ops|server|time||enforce",
		"'@everyone|user|ana@example.com|model|gemma||enforce",
		"'@everyone|service account|svc-ci@svc.ambit.internal|model|gemma||enforce",
		"'@everyone|all-signed-in|*|model|gemma||enforce",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("grants.csv\n%s", strings.ReplaceAll(strings.Join(lines, "\n"), "\n", " // "))
	}
	k := files["keys.csv"]
	if len(k) != 3 || k[1][0] != "kgone" || k[1][2] != "service account" || k[1][6] != "2027-01-03T00:00:00Z" ||
		k[2][0] != "kalive" || k[2][3] != "eng" || k[2][6] != "" {
		t.Errorf("keys.csv %v", k)
	}
	if p := files["people-seen.csv"]; len(p) != 2 || strings.Join(p[1], "|") != "eng|ana@example.com|2026-12-01T00:00:00Z|2026-12-31T09:00:00Z|42" {
		t.Errorf("people-seen.csv %v", p)
	}
	ofDay, _ := json.Marshal(m.Sources["snapshotsOfTheDay"])
	notes := strings.Join(m.Notes, " ")
	if !strings.Contains(string(ofDay), "111111111111") || !strings.Contains(notes, "each of the 2 snapshots") || !strings.Contains(notes, "Audit-only mode") ||
		!strings.Contains(notes, "dated member list") || m.Parameters["date"] != "2026-12-31" {
		t.Errorf("snapshots of the day %s, notes %s", ofDay, notes)
	}
	if b, _ := json.Marshal(m); strings.Contains(string(b), `"h1"`) {
		t.Error("a key hash is in the manifest")
	}
}

func TestAccessExportRefusals(t *testing.T) {
	e := newEvidenceFixture(t, goodCH(0))
	for path, want := range map[string]string{
		"/api/evidence/access.zip?date=2026-12-31":                 "the controller has not recorded",
		"/api/evidence/access.zip?from=2026-10-01&to=2026-12-31":   "date=YYYY-MM-DD",
		"/api/evidence/access.zip?date=31.12.2026":                 "date: use",
		"/api/evidence/access.zip?date=2026-12-31&email=a%40b.com": "not from, to or email",
	} {
		resp, body, _ := e.get(t, t.Context(), path, e.admin)
		if resp.StatusCode/100 != 4 || !strings.Contains(string(body), want) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	e.ch.snapshots = [][]driver.Value{{time.Date(2026, 12, 10, 8, 0, 0, 0, time.UTC), "v", "h", "enforce", "[]"}}
	// The fake answers "none before the end" only when it has no snapshots; a later first snapshot is told by the real
	// store (TestAccessEvidenceOnClickHouse). Here: GET /api/evidence names the first one.
	_, body, _ := e.get(t, t.Context(), "/api/evidence", e.admin)
	if !strings.Contains(string(body), `"snapshotsSince":"2026-12-10T08:00:00Z"`) {
		t.Errorf("GET /api/evidence %s", body)
	}
	e.cat.Audit = nil
	if resp, body, _ := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31", e.admin); resp.StatusCode != 404 || !strings.Contains(string(body), "evidence.enabled") {
		t.Errorf("no audit store: %d %s", resp.StatusCode, body)
	}
}
