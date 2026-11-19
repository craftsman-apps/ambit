package guard

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/extmcp"
)

func numArgs(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("bad test JSON %s: %v", raw, err)
	}
	return m
}

// Issues 334 and 301: a limit holds for every spelling of the argument name that Go's encoding/json reads as the
// same field, and notContains is not passed by extra spaces, tabs, newlines or SQL comments.
func TestArgumentLimitsSpellings(t *testing.T) {
	grants := []Grant{{Rule: "eng", Groups: []string{"eng"}, Arguments: []v1.ArgumentRule{
		{Name: "query", NotContains: []string{"DROP TABLE", "secret"}},
		{Name: "limit", Max: "100"},
		{Name: "mode", OneOf: []string{"read", "list"}},
		{Name: "sql", NotContains: []string{"drop"}},
	}}}
	ok := `"limit":10,"mode":"read"`
	for _, c := range []struct {
		args string
		want string // "" = allowed; else a part of the reason
	}{
		{`{"query":"select 1",` + ok + `}`, ""},
		{`{"Query":"DROP TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"QUERY":"DROP TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"ok","Query":"DROP TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"Query":"DROP TABLE users","query":"ok",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"x","limit":5,"Limit":500,"mode":"read"}`, "limit must be a number at most 100"},
		{`{"query":"x","LIMIT":500,"mode":"read"}`, "limit must be a number at most 100"},
		{`{"query":"x","Limit":5,"mode":"read"}`, ""}, // the server reads Limit as limit: 5 is within the limit
		{`{"query":"x","limit":1,"mode":"read","Mode":"delete"}`, "mode must be one of read, list"},
		{`{"query":"x",` + ok + `,"ſql":"DROP x"}`, "sql must not contain drop"}, // encoding/json folds ſ to s
		{`{"query":"DROP  TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"DROP\tTABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"DROP\nTABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"DROP/**/TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"DROP /* x */ TABLE users",` + ok + `}`, "query must not contain DROP TABLE"},
		{`{"query":"select DROPTABLE",` + ok + `}`, ""}, // words stay words: no space is added
		{`{"query":"a /* comment */ b",` + ok + `}`, ""},
	} {
		got := CheckArguments(grants, []string{"eng"}, "", "run", numArgs(t, c.args))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.args, got, c.want)
		}
	}
}

func respond(t *testing.T, g *Guard, method, body string) (out string, withheld bool) {
	t.Helper()
	res, err := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: method, McpResponse: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case res.GetPass() != nil:
		return body, false
	case res.GetError() != nil:
		return res.GetError().Reason, true
	}
	return string(res.GetMutated()), false
}

// Issue 298: object keys, JSON numbers and uri values are masked; type and mimeType of content items are structure.
// Issue 297: resources/read and prompts/get results are masked like tool results.
func TestMaskingCoversKeysNumbersURIsResourcesAndPrompts(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	for _, c := range []struct{ method, body, want string }{
		{"tools/call", `{"content":[],"structuredContent":{"kim@acme.com":{"orders":3}}}`, `{"content":[],"structuredContent":{"<EMAIL>":{"orders":3}}}`},
		{"tools/call", `{"content":[],"structuredContent":{"owner":{"uri":"mailto:kim@acme.com"}}}`, `{"content":[],"structuredContent":{"owner":{"uri":"mailto:<EMAIL>"}}}`},
		{"tools/call", `{"content":[{"type":"resource_link","uri":"https://crm.example/u/kim@acme.com","name":"profile"}]}`, `{"content":[{"name":"profile","type":"resource_link","uri":"https://crm.example/u/<EMAIL>"}]}`},
		{"tools/call", `{"content":[],"structuredContent":{"card":4111111111111111,"at":1763372401127,"price":12.50}}`, `{"content":[],"structuredContent":{"at":1763372401127,"card":"<CARD_NUMBER>","price":12.5}}`},
		{"tools/call", `{"content":[],"structuredContent":{"type":"kim@acme.com"}}`, `{"content":[],"structuredContent":{"type":"<EMAIL>"}}`},
		{"tools/call", `{"content":[{"type":"resource","resource":{"uri":"file:///kim@acme.com.csv","mimeType":"text/csv","text":"RRN 900101-1234567"}}]}`, `{"content":[{"resource":{"mimeType":"text/csv","text":"RRN <NATIONAL_ID>","uri":"file:///<EMAIL>"},"type":"resource"}]}`},
		{"resources/read", `{"contents":[{"uri":"file:///customers.csv","mimeType":"text/csv","text":"Kim,900101-1234567,kim@acme.com"}]}`, `{"contents":[{"mimeType":"text/csv","text":"Kim,<NATIONAL_ID>,<EMAIL>","uri":"file:///customers.csv"}]}`},
		{"prompts/get", `{"description":"brief for kim@acme.com","messages":[{"role":"user","content":{"type":"text","text":"RRN 900101-1234567"}}]}`, `{"description":"brief for <EMAIL>","messages":[{"content":{"text":"RRN <NATIONAL_ID>","type":"text"},"role":"user"}]}`},
	} {
		if got, _ := respond(t, g, c.method, c.body); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.method, c.body, got, c.want)
		}
	}
	// Two keys that mask to the same text cannot both be kept: the result is withheld.
	if got, withheld := respond(t, g, "tools/call", `{"content":[],"structuredContent":{"kim@acme.com":1,"lee@acme.com":2}}`); !withheld || strings.Contains(got, "acme") {
		t.Errorf("colliding keys: %s", got)
	}
	// A resources/read result that is not an object is withheld too.
	if _, withheld := respond(t, g, "resources/read", `["Kim,900101-1234567"]`); !withheld {
		t.Error("resources/read list result passed")
	}
}
