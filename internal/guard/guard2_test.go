package guard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/extmcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// pinReader is a client.Reader that returns one Server, NotFound for other names, or an error.
type pinReader struct {
	obj *v1.Server
	err bool
}

func (f pinReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if f.err {
		return errors.New("cache not synced")
	}
	if f.obj == nil || key.Name != f.obj.Name {
		return apierrors.NewNotFound(k8sschema.GroupResource{Resource: "servers"}, key.Name)
	}
	*(obj.(*v1.Server)) = *f.obj
	return nil
}

func (pinReader) List(context.Context, client.ObjectList, ...client.ListOption) error { return nil }

// Issue 300: when the Server cannot be read, pinning fails closed: nothing is approved, so every tool is hidden
// and every call refused.
func TestPinsFailClosedWhenTheServerCannotBeRead(t *testing.T) {
	s := &v1.Server{}
	s.Name = "crm"
	s.Status.ApprovedTools = []v1.ToolPin{{Name: "get_customer", Fingerprint: good}}
	s.Status.Tools = []v1.Tool{{Name: "get_customer", Fingerprint: good}}
	for name, c := range map[string]struct {
		r      pinReader
		server string
	}{"read error": {pinReader{err: true}, "crm"}, "no such Server": {pinReader{obj: s}, "other"}} {
		approved, _, pinned := KubePins{Client: c.r, Namespace: "ambit-system"}.Pins(t.Context(), c.server)
		if !pinned || len(approved) != 0 {
			t.Errorf("%s: pinned=%v approved=%v, want pinned with nothing approved", name, pinned, approved)
		}
	}
	if approved, _, pinned := (KubePins{Client: pinReader{obj: s}, Namespace: "ambit-system"}).Pins(t.Context(), "crm"); !pinned || approved["get_customer"] != good {
		t.Errorf("readable Server: %v %v", pinned, approved)
	}
	g := &Guard{Pins: KubePins{Client: pinReader{err: true}, Namespace: "ambit-system"}, Masker: Builtin{}}
	res, _ := g.CheckRequest(t.Context(), &extmcp.McpRequest{ServiceNames: []string{"crm"}, Method: "tools/call", McpRequest: []byte(`{"name":"get_customer","arguments":{}}`)})
	if res.GetError() == nil {
		t.Errorf("call passed while the Server could not be read: %v", res)
	}
}
