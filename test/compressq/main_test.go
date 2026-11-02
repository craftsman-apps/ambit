package main

import (
	"encoding/csv"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The expected answers of questions.go are recomputed here from the fixture text, with other code than the
// generators, so a changed fixture cannot silently make an answer wrong.
func TestExpectedAnswers(t *testing.T) {
	if len(questions) != 20 {
		t.Fatalf("%d questions, want 20", len(questions))
	}
	want := map[string][]string{}
	var issues []struct {
		Number int
		Title  string
		State  string
		Labels []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(issuesText()), &issues); err != nil {
		t.Fatal(err)
	}
	bugs, closed := 0, 0
	for _, i := range issues {
		for _, l := range i.Labels {
			if l.Name == "bug" {
				bugs++
			}
		}
		if i.State == "closed" {
			closed++
		}
		if i.Number == 41 {
			want["q02"] = []string{strings.ToLower(i.Title)}
		}
	}
	want["q01"], want["q03"] = []string{itoa(bugs)}, []string{itoa(closed)}

	var rows []struct {
		ID     int
		Status string
		Amount json.Number
	}
	if err := json.Unmarshal([]byte(rowsText()), &rows); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, r := range rows {
		if r.Status == "failed" {
			failed++
		}
		if r.ID == 117 {
			want["q04"] = []string{r.Amount.String()}
		}
	}
	want["q05"] = []string{itoa(failed)}

	lg := logText()
	lines := strings.Split(strings.TrimSuffix(lg, "\n"), "\n")
	errs := regexp.MustCompile(` ERROR .* id=(\d+)`).FindAllStringSubmatch(lg, -1)
	want["q06"], want["q07"] = []string{itoa(len(errs))}, []string{errs[len(errs)-1][1]}
	want["q08"] = []string{regexp.MustCompile(`disk usage at (\d+)%`).FindStringSubmatch(lg)[1]}
	want["q09"] = []string{strings.TrimSuffix(strings.SplitN(lines[len(lines)-1], " ", 2)[0], "Z")[len("2026-03-01T"):]}
	traces := regexp.MustCompile(`\t/app/(\w+\.go):(\d+) `).FindAllStringSubmatch(lg, -1)
	want["q10"] = []string{traces[2][1], traces[2][2]}
	want["q20"] = []string{regexp.MustCompile(`2026-03-01T10:10:03Z INFO served request path=/api/items/(\d+)`).FindStringSubmatch(lg)[1]}

	tests := testsText()
	want["q11"] = []string{"testparseheader", "testretrybackoff", "testcacheevict"}
	want["q12"] = []string{regexp.MustCompile(`retry_test.go:\d+: got (\w+), want (\w+)`).FindStringSubmatch(tests)[1], regexp.MustCompile(`retry_test.go:\d+: got (\w+), want (\w+)`).FindStringSubmatch(tests)[2]}
	want["q13"] = []string{itoa(strings.Count(tests, "--- PASS"))}
	want["q14"] = []string{strings.TrimSuffix(regexp.MustCompile(`pkg\t([\d.]+s)`).FindStringSubmatch(tests)[1], "s")}

	recs, err := csv.NewReader(strings.NewReader(csvText())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	refunds := 0
	for _, r := range recs[1:] {
		if r[2] == "refund" {
			refunds++
		}
		if r[0] == "77" {
			want["q15"] = []string{r[3]}
		}
	}
	want["q16"] = []string{itoa(refunds)}

	var deg []string
	for _, l := range strings.Split(tableText(), "\n") {
		if regexp.MustCompile(`\|\s+degraded\s+\|$`).MatchString(l) {
			deg = append(deg, strings.TrimSpace(strings.Split(l, "|")[2]))
		}
	}
	want["q17"] = deg

	pods := podsText()
	want["q18"] = []string{itoa(strings.Count(pods, "namespace: staging"))}
	for _, item := range strings.Split(pods, "- apiVersion: v1\n  kind: Pod\n")[1:] {
		if strings.Contains(item, "restartCount: 7\n") {
			want["q19"] = []string{regexp.MustCompile(`name: (web-\d+)`).FindStringSubmatch(item)[1]}
		}
	}

	for _, q := range questions {
		w, ok := want[q.ID]
		if !ok || strings.Join(w, "|") != strings.Join(q.Expect, "|") {
			t.Errorf("%s: expect %q, recomputed %q", q.ID, q.Expect, w)
		}
	}
}

func TestFixturesAreBigEnoughAndMarked(t *testing.T) {
	for name, s := range map[string]string{"issues": issuesText(), "rows": rowsText(), "log": logText(), "tests": testsText(), "csv": csvText(), "table": tableText(), "pods": podsText()} {
		if len(s) < 8192 { // the default minBytes of result compression
			t.Errorf("%s is %d bytes, under minBytes 8192", name, len(s))
		}
	}
	if s, _ := json.Marshal(bothStructured()); strings.Contains(bothText(), "STRUCTMARK") || strings.Contains(string(s), "TXTMARK") {
		t.Error("text and structured content must not share a marker")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
