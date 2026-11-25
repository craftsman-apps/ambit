package console

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	got := argumentLimits([]v1.ArgumentRule{{Name: "repo", OneOf: []string{"web", "api"}}, {Name: "per_page", Max: "100"}, {Name: "q", NotContains: []string{"secret"}}, {Name: "limit", Min: "1", Max: "50"}})
	if got != "repo oneOf [web, api]; per_page max 100; q notContains [secret]; limit min 1; limit max 50" {
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

// at=T: every file is as of T. Two snapshots on the day (10:00 audit, 15:00 enforce); keys around 11:00 and 12:00.
func TestAccessExportAt(t *testing.T) {
	d := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	ten := snapshotBody(t, v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "ten"}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gpt-ten"}}})
	fif := snapshotBody(t, v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "fif"}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gpt-fifteen"}}})
	ten, fif = strings.Replace(ten, `"name":""`, `"name":"ten"`, 1), strings.Replace(fif, `"name":""`, `"name":"fif"`, 1)
	ch := goodCH(0)
	ch.snapshots = [][]driver.Value{
		{d.Add(10 * time.Hour), "111111111111", "aaaa", "audit", ten},
		{d.Add(15 * time.Hour), "222222222222", "bbbb", "enforce", fif},
	}
	e := newEvidenceFixture(t, ch)
	e.cat.Evidence.Now = func() time.Time { return d.AddDate(0, 1, 0) }
	// kstored: created 11:00, still stored. kgone: created the day before, deleted 12:00.
	sec := (Key{ID: "kstored", Email: "ana@example.com", Created: d.Add(11 * time.Hour), Until: d.AddDate(0, 1, 0), Scope: ScopeLLM, Hash: "h1"}).Secret(testNS)
	if err := e.kube.Create(t.Context(), sec); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []*corev1.ConfigMap{
		evidenceKeyEvent(d.AddDate(0, 0, -1), "create", "kgone", "bob@example.com"),
		evidenceKeyEvent(d.Add(12*time.Hour), "revoke", "kgone", "bob@example.com"),
	} {
		if err := e.kube.Create(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		at, version, model, keys, name string
		audit                          bool
	}{
		{"2026-12-31T10:00:00Z", "111111111111", "gpt-ten", "kgone", "ambit-access-2026-12-31-T100000Z.zip", true},         // a snapshot at exactly T counts
		{"2026-12-31T11:30:00Z", "111111111111", "gpt-ten", "kgone kstored", "ambit-access-2026-12-31-T113000Z.zip", true}, // the key created at 11:00 is alive
		{"2026-12-31T12:00:00Z", "111111111111", "gpt-ten", "kstored", "ambit-access-2026-12-31-T120000Z.zip", true},       // revoked at exactly T: not alive
		{"2026-12-31T14:59:59Z", "111111111111", "gpt-ten", "kstored", "ambit-access-2026-12-31-T145959Z.zip", true},
		{"2026-12-31T15:00:00Z", "222222222222", "gpt-fifteen", "kstored", "ambit-access-2026-12-31-T150000Z.zip", true},      // the audit mode of 10:00 still counts for the day up to T
		{"2026-12-31T17:30:00+02:00", "222222222222", "gpt-fifteen", "kstored", "ambit-access-2026-12-31-T153000Z.zip", true}, // an offset is converted to UTC
		{"2026-12-31T23:59:59Z", "222222222222", "gpt-fifteen", "kstored", "ambit-access-2026-12-31-T235959Z.zip", true},
	} {
		resp, body, err := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31&at="+url.QueryEscape(c.at), e.admin)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v %s", c.at, resp.StatusCode, err, body)
		}
		if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="`+c.name+`"` {
			t.Errorf("%s: name %s", c.at, got)
		}
		files, m := csvFiles(t, body)
		g := files["grants.csv"]
		if len(g) != 2 || g[1][1] != c.version || g[1][7] != c.model {
			t.Errorf("%s: grants.csv %v", c.at, g)
		}
		var keys []string
		for _, r := range files["keys.csv"][1:] {
			keys = append(keys, r[0])
		}
		if strings.Join(keys, " ") != c.keys {
			t.Errorf("%s: keys.csv %v, want %s", c.at, keys, c.keys)
		}
		at, _ := time.Parse(time.RFC3339, c.at)
		if m.Parameters["at"] != at.UTC().Format(time.RFC3339Nano) || m.Parameters["end"] != m.Parameters["at"] ||
			m.Parameters["peopleSeenFrom"] != at.UTC().AddDate(0, 0, -90).Format(time.RFC3339Nano) {
			t.Errorf("%s: parameters %v", c.at, m.Parameters)
		}
		if notes := strings.Join(m.Notes, " "); !strings.Contains(notes, "as of the time given with at") || !strings.Contains(notes, "newest one at or before at") {
			t.Errorf("%s: notes %s", c.at, notes)
		}
	}
	// Audit-only mode counts only up to T: the day's first snapshot is audit; at 09:00 there is none, so refuse; the
	// enforce snapshot alone (a day with one) has no audit note.
	e.ch.snapshots = e.ch.snapshots[1:]
	_, body, _ := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31&at=2026-12-31T20:00:00Z", e.admin)
	_, m := csvFiles(t, body)
	if notes := strings.Join(m.Notes, " "); strings.Contains(notes, "Audit-only") || strings.Contains(notes, "The rules changed during") {
		t.Errorf("a day with one snapshot: notes %s", notes)
	}
	// Without at nothing changes: the end of the day, the plain name, no "at" parameter.
	resp, body, _ := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31", e.admin)
	_, m = csvFiles(t, body)
	if _, has := m.Parameters["at"]; has || resp.Header.Get("Content-Disposition") != `attachment; filename="ambit-access-2026-12-31.zip"` {
		t.Errorf("without at: %v %s", m.Parameters, resp.Header.Get("Content-Disposition"))
	}
}

func TestAccessExportAtRefusals(t *testing.T) {
	d := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	ch := goodCH(0)
	ch.snapshots = [][]driver.Value{{d.Add(10 * time.Hour), "111111111111", "aaaa", "enforce", "[]"}}
	e := newEvidenceFixture(t, ch)
	for _, c := range []struct {
		at     string
		status int
		want   string
	}{
		{"noon", 400, "at: use an RFC 3339 time inside the date"},
		{"", 400, "at: use an RFC 3339 time inside the date"},
		{"2026-12-31", 400, "at: use an RFC 3339 time inside the date"},
		{"2026-12-31T10:00:00", 400, "at: use an RFC 3339 time inside the date"}, // no zone
		{"2026-12-30T23:59:59Z", 400, "at: the time must be inside the UTC day 2026-12-31"},
		{"2027-01-01T00:00:00Z", 400, "at: the time must be inside the UTC day 2026-12-31"},
		{"2026-12-31T23:30:00-02:00", 400, "at: the time must be inside the UTC day 2026-12-31"}, // 01:30 UTC the next day
		{"2026-12-31T09:59:59Z", 404, "No snapshot at or before 2026-12-31T09:59:59Z. Snapshots exist from 2026-12-31T10:00:00Z"},
	} {
		resp, body, _ := e.get(t, t.Context(), "/api/evidence/access.zip?date=2026-12-31&at="+url.QueryEscape(c.at), e.admin)
		if resp.StatusCode != c.status || !strings.Contains(string(body), c.want) {
			t.Errorf("at=%q: %d %s", c.at, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") == "application/zip" {
			t.Errorf("at=%q: a ZIP was started", c.at)
		}
	}
}

// GET /api/evidence/access/snapshots: the snapshots of a day, oldest first; none is an empty list; admins only.
func TestAccessSnapshotsOfADay(t *testing.T) {
	d := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	ch := goodCH(0)
	ch.snapshots = [][]driver.Value{
		{d.Add(-time.Hour), "000000000000", "zzzz", "enforce", "[]"}, // the day before
		{d.Add(10 * time.Hour), "111111111111", "aaaa", "audit", "[]"},
		{d.Add(15*time.Hour + 30*time.Minute), "222222222222", "bbbb", "enforce", "[]"},
		{d.AddDate(0, 0, 1), "333333333333", "cccc", "enforce", "[]"}, // exactly the next midnight: the next day
	}
	e := newEvidenceFixture(t, ch)
	_, body, _ := e.get(t, t.Context(), "/api/evidence/access/snapshots?date=2026-12-31", e.admin)
	want := `{"date":"2026-12-31","snapshots":[{"time":"2026-12-31T10:00:00Z","rulesVersion":"111111111111","rulesHash":"aaaa","accessMode":"audit"},` +
		`{"time":"2026-12-31T15:30:00Z","rulesVersion":"222222222222","rulesHash":"bbbb","accessMode":"enforce"}]}`
	if strings.TrimSpace(string(body)) != want {
		t.Errorf("snapshots of the day\n%s\nwant\n%s", body, want)
	}
	_, body, _ = e.get(t, t.Context(), "/api/evidence/access/snapshots?date=2026-11-01", e.admin)
	if strings.TrimSpace(string(body)) != `{"date":"2026-11-01","snapshots":[]}` {
		t.Errorf("a day with none: %s", body)
	}
	if resp, body, _ := e.get(t, t.Context(), "/api/evidence/access/snapshots?date=31.12.2026", e.admin); resp.StatusCode != 400 || !strings.Contains(string(body), "date: use") {
		t.Errorf("bad date: %d %s", resp.StatusCode, body)
	}
	if resp, _, _ := e.get(t, t.Context(), "/api/evidence/access/snapshots", e.admin); resp.StatusCode != 400 {
		t.Errorf("no date: %d", resp.StatusCode)
	}
	for ck, want := range map[*http.Cookie]int{nil: 401, as(e.auth, RoleViewer): 403, as(e.auth, RoleApprover): 403} {
		if resp, _, _ := e.get(t, t.Context(), "/api/evidence/access/snapshots?date=2026-12-31", ck); resp.StatusCode != want {
			t.Errorf("status %d, want %d", resp.StatusCode, want)
		}
	}
	e.cat.Audit = nil
	if resp, body, _ := e.get(t, t.Context(), "/api/evidence/access/snapshots?date=2026-12-31", e.admin); resp.StatusCode != 404 || !strings.Contains(string(body), "evidence.enabled") {
		t.Errorf("no audit store: %d %s", resp.StatusCode, body)
	}
}
