package guard

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// uncolumn turns {"columns":[...],"rows":[[...]]} back into an array of objects, everywhere in v.
func uncolumn(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = uncolumn(x[i])
		}
	case map[string]any:
		cols, hasC := x["columns"].([]any)
		rows, hasR := x["rows"].([]any)
		if hasC && hasR && len(x) == 2 {
			out := make([]any, len(rows))
			for i, r := range rows {
				o := map[string]any{}
				for j, c := range cols {
					o[c.(string)] = uncolumn(r.([]any)[j])
				}
				out[i] = o
			}
			return out
		}
		for k := range x {
			x[k] = uncolumn(x[k])
		}
	}
	return v
}

func decodeExact(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("invalid JSON: %v\n%.200s", err, s)
	}
	return v
}

func TestCompressJSONKeepsEveryValue(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		columnar bool
		maxRatio float64
	}{
		"issues minify":   {issueList(100), false, 0.85},
		"issues columnar": {issueList(100), true, 0.55},
		"rows columnar":   {queryRows(2000), true, 0.55},
		"rows minify":     {queryRows(2000), false, 0.85},
		"nested":          {"{\n  \"a\": {\n    \"b\": [\n      1,\n      2.50,\n      {\"c\": \"x y\"}\n    ]\n  }\n}\n", false, 0.9},
		"unicode escapes": {"{\n  \"q\": \"say \\\"hi\\\" \\u00e9 \\\\ 한국어\",\n  \"n\": 12345678901234567890.123456789e-5\n}", false, 0.95},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := compressJSON(tc.in, tc.columnar)
			if !ok {
				t.Fatal("not compressed")
			}
			if r := float64(len(out)) / float64(len(tc.in)); r > tc.maxRatio {
				t.Errorf("ratio %.2f, want at most %.2f", r, tc.maxRatio)
			}
			if got, want := uncolumn(decodeExact(t, out)), decodeExact(t, tc.in); !reflect.DeepEqual(got, want) {
				t.Errorf("information changed")
			}
			again, _ := compressJSON(tc.in, tc.columnar)
			if again != out {
				t.Error("not deterministic")
			}
		})
	}
}

func TestCompressJSONKeepsKeyOrderAndRawNumbers(t *testing.T) {
	out, ok := compressJSON("{ \"z\": 1.0, \"a\": 12345678901234567890.123456789, \"m\": [ 1e3 , \"s p\" ] }", false)
	if !ok || out != `{"z":1.0,"a":12345678901234567890.123456789,"m":[1e3,"s p"]}` {
		t.Fatalf("got %q", out)
	}
}

func TestCompressJSONColumnarOnlyForUniformArrays(t *testing.T) {
	mixed := `[{"a":1,"b":2},{"a":1,"b":2},{"a":1,"b":2},{"a":1,"b":2},{"a":1,"c":2}]`
	out, _ := compressJSON(mixed, true)
	if strings.Contains(out, `"columns"`) {
		t.Errorf("mixed keys must not become columnar: %s", out)
	}
	short := `[{"a":1},{"a":2},{"a":3},{"a":4}]`
	if out, _ := compressJSON(short, true); strings.Contains(out, `"columns"`) {
		t.Errorf("4 rows must not become columnar: %s", out)
	}
	uniform := `[{"a":1},{"a":2},{"a":3},{"a":4},{"a":5}]`
	if out, _ := compressJSON(uniform, true); out != `{"columns":["a"],"rows":[[1],[2],[3],[4],[5]]}` {
		t.Errorf("got %s", out)
	}
	if out, _ := compressJSON(uniform, false); strings.Contains(out, `"columns"`) {
		t.Errorf("columnar off (Presidio set): %s", out)
	}
}

func TestCompressJSONRefusesWhatItCannotKeep(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate keys":  `{"a": 1, "a": 2}`,
		"duplicate (row)": `[{"a":1,"a":2},{"a":1,"a":2},{"a":1,"a":2},{"a":1,"a":2},{"a":1,"a":2}]`,
		"invalid":         `{"a": }`,
		"scalar":          `"just a string"`,
		"prose":           "hello world",
		"empty":           "",
	} {
		if out, ok := compressJSON(in, true); ok {
			t.Errorf("%s: compressed to %q", name, out)
		}
	}
}

func FuzzCompressJSON(f *testing.F) {
	f.Add(`{"a": [1, 2, {"b": null}]}`)
	f.Add(queryRows(6))
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := compressJSON(in, true)
		if !ok {
			return
		}
		if len(out) > len(in) || !json.Valid([]byte(out)) {
			t.Fatalf("bad output for %q: %q", in, out)
		}
	})
}
