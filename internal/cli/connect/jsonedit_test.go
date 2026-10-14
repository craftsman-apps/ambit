package connect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

var testSets = []Set{
	{Path: []string{"apiKeyHelper"}, Value: `"'/usr/local/bin/ambit' key print --url https://llm.example.com"`},
	{Path: []string{"env", "ANTHROPIC_BASE_URL"}, Value: `"https://llm.example.com"`},
	{Path: []string{"env", "ANTHROPIC_MODEL"}, Value: `"nvidia/nemotron"`},
}

// jsonCases are realistic files. Each one must survive connect then disconnect byte for byte.
var jsonCases = map[string]string{
	"empty object":        "{}",
	"empty object, eol":   "{}\n",
	"spaced empty object": "{ }\n",
	"newline only":        "{\n}\n",
	"two-space indent": `{
  "permissions": {
    "allow": ["Bash(ls)"]
  },
  "theme": "dark"
}
`,
	"four-space indent": "{\n    \"theme\": \"dark\"\n}\n",
	"tab indent":        "{\n\t\"theme\": \"dark\"\n}\n",
	"no final newline":  "{\n  \"theme\": \"dark\"\n}",
	"env present": `{
  "env": {
    "FOO": "1"
  },
  "theme": "dark"
}
`,
	"env empty":   "{\n  \"env\": {}\n}\n",
	"env is last": "{\n  \"theme\": \"dark\",\n  \"env\": {\"FOO\": \"1\"}\n}\n",
	"comments (JSONC)": `// my settings
{
  /* the theme */
  "theme": "dark", // keep dark
  // proxy below
  "proxy": "x" // last
}
// end
`,
	"trailing comma":             "{\n  \"theme\": \"dark\",\n}\n",
	"trailing comma, none after": "{\n  \"a\": 1,\n  \"b\": [1, 2,],\n}\n",
	"comment before close":       "{\n  \"a\": 1\n  // nothing else\n}\n",
	"CRLF":                       "{\r\n  \"theme\": \"dark\"\r\n}\r\n",
	"CRLF empty":                 "{}\r\n",
	"BOM":                        "\xef\xbb\xbf{\n  \"theme\": \"dark\"\n}\n",
	"single line":                `{"theme":"dark","a":[1,2]}`,
	"single line, spaces":        `{ "theme": "dark" }` + "\n",
	"already has apiKeyHelper":   "{\n  \"apiKeyHelper\": \"/bin/old\",\n  \"theme\": \"dark\"\n}\n",
	"already has model var":      "{\n  \"env\": {\n    \"ANTHROPIC_MODEL\": \"old\"\n  }\n}\n",
	"unicode and escapes":        "{\n  \"name\": \"Zoë \\u00e9 \\\"q\\\"\"\n}\n",
	"big numbers":                "{\n  \"n\": 12345678901234567890.5e+3\n}\n",
}

func TestJSONConnectThenDisconnectIsByteIdentical(t *testing.T) {
	for name, before := range jsonCases {
		t.Run(name, func(t *testing.T) {
			d, err := ParseJSON([]byte(before))
			if err != nil {
				t.Fatal(err)
			}
			entries, err := d.Apply(testSets)
			if err != nil {
				t.Fatal(err)
			}
			after := d.Bytes()
			if !json.Valid(stripForValid(t, after)) {
				t.Fatalf("not valid JSON(C) after Apply:\n%s", after)
			}
			for _, s := range testSets {
				if got, ok := d.Get(s.Path...); !ok || !jsonEqual(got, s.Value) {
					t.Fatalf("Get(%v) = %q, %v", s.Path, got, ok)
				}
			}
			d2, err := ParseJSON(after)
			if err != nil {
				t.Fatal(err)
			}
			left, err := d2.Remove(entries)
			if err != nil || len(left) != 0 {
				t.Fatalf("Remove = %v, %v", left, err)
			}
			if string(d2.Bytes()) != before {
				t.Fatalf("not byte-identical\n got  %q\n want %q\n after %q", d2.Bytes(), before, after)
			}
		})
	}
}

// Claude Code's settings.json is strict JSON: Apply must not add comments or trailing commas to a file that has none.
func TestJSONApplyKeepsStrictJSONStrict(t *testing.T) {
	for _, before := range []string{"{}", "{\n  \"a\": 1\n}\n", "{\n  \"a\": {\"b\": 1}\n}\n", "{\"env\":{}}"} {
		d, _ := ParseJSON([]byte(before))
		if _, err := d.Apply(testSets); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(d.Bytes()) {
			t.Fatalf("strict JSON became invalid:\n%s", d.Bytes())
		}
	}
}

func TestJSONApplyMatchesTheFilesIndent(t *testing.T) {
	d, _ := ParseJSON([]byte("{\n    \"theme\": \"dark\"\n}\n"))
	if _, err := d.Apply(testSets[:2]); err != nil {
		t.Fatal(err)
	}
	want := `{
    "theme": "dark",
    "apiKeyHelper": "'/usr/local/bin/ambit' key print --url https://llm.example.com",
    "env": {
        "ANTHROPIC_BASE_URL": "https://llm.example.com"
    }
}
`
	if string(d.Bytes()) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", d.Bytes(), want)
	}
	// CRLF files get CRLF in the inserted lines.
	d, _ = ParseJSON([]byte("{\r\n  \"a\": 1\r\n}\r\n"))
	d.Apply(testSets[:1])
	if strings.Contains(strings.ReplaceAll(string(d.Bytes()), "\r\n", ""), "\n") {
		t.Fatalf("a bare LF in a CRLF file: %q", d.Bytes())
	}
}

func TestJSONNewFile(t *testing.T) {
	d := NewJSON("\n")
	if string(d.Bytes()) != "{}\n" {
		t.Fatalf("new file = %q", d.Bytes())
	}
	entries, _ := d.Apply(testSets)
	if left, _ := d.Remove(entries); len(left) != 0 || string(d.Bytes()) != "{}\n" {
		t.Fatalf("after remove: %q, left %v", d.Bytes(), left)
	}
}

func TestJSONApplyNestedObjectValue(t *testing.T) {
	val := `{"npm":"@ai-sdk/openai-compatible","name":"Ambit","options":{"baseURL":"https://x/v1","apiKey":"{env:AMBIT_API_KEY}"},"models":{"m":{"name":"m"}}}`
	before := "{\n  // opencode\n  \"theme\": \"x\",\n  \"provider\": {\n    \"other\": {\"npm\": \"y\"},\n  },\n}\n"
	d, _ := ParseJSON([]byte(before))
	entries, err := d.Apply([]Set{{Path: []string{"provider", "ambit"}, Value: val}, {Path: []string{"model"}, Value: `"ambit/m"`}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := d.Get("provider", "ambit"); !ok || !jsonEqual(got, val) {
		t.Fatalf("Get = %q", got)
	}
	if !strings.Contains(string(d.Bytes()), "\n    \"ambit\": {\n      \"npm\"") {
		t.Fatalf("nested value is not indented like its parent:\n%s", d.Bytes())
	}
	d2, _ := ParseJSON(d.Bytes())
	d2.Remove(entries)
	if string(d2.Bytes()) != before {
		t.Fatalf("got %q", d2.Bytes())
	}
}

// Section 14 item 3 of the spec: what happens when the file changes between connect and disconnect.
func TestJSONRemoveAfterChanges(t *testing.T) {
	before := "{\n  \"theme\": \"dark\"\n}\n"
	connected := func(t *testing.T) (string, []Entry) {
		d, _ := ParseJSON([]byte(before))
		e, err := d.Apply(testSets)
		if err != nil {
			t.Fatal(err)
		}
		return string(d.Bytes()), e
	}
	remove := func(t *testing.T, s string, e []Entry) (string, []string) {
		d, err := ParseJSON([]byte(s))
		if err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
		left, err := d.Remove(e)
		if err != nil {
			t.Fatal(err)
		}
		return string(d.Bytes()), left
	}

	t.Run("a: an unrelated value changed", func(t *testing.T) {
		s, e := connected(t)
		got, left := remove(t, strings.Replace(s, `"dark"`, `"light"`, 1), e)
		if got != strings.Replace(before, "dark", "light", 1) || len(left) != 0 {
			t.Fatalf("got %q, left %v", got, left)
		}
	})
	t.Run("b: an Ambit value was edited", func(t *testing.T) {
		s, e := connected(t)
		got, left := remove(t, strings.Replace(s, `nvidia/nemotron`, `my/own`, 1), e)
		if len(left) != 1 || left[0] != "/env/ANTHROPIC_MODEL" || !strings.Contains(got, "my/own") || strings.Contains(got, "apiKeyHelper") || strings.Contains(got, "ANTHROPIC_BASE_URL") {
			t.Fatalf("got %q, left %v", got, left)
		}
		if !strings.Contains(got, `"env"`) { // the container stays: it is not empty
			t.Fatalf("env was removed while it still holds an entry: %q", got)
		}
	})
	t.Run("c: an Ambit entry was deleted", func(t *testing.T) {
		s, e := connected(t)
		got, left := remove(t, strings.Replace(s, "    \"ANTHROPIC_MODEL\": \"nvidia/nemotron\"\n", "", 1), e)
		_ = left
		if strings.Contains(got, "apiKeyHelper") || strings.Contains(got, "ANTHROPIC") || !strings.Contains(got, "dark") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("d: the keys were reordered", func(t *testing.T) {
		d, _ := ParseJSON([]byte(`{"env": {"ANTHROPIC_MODEL": "nvidia/nemotron", "ANTHROPIC_BASE_URL": "https://llm.example.com"}, "theme": "x", "apiKeyHelper": "'/usr/local/bin/ambit' key print --url https://llm.example.com"}`))
		_, e := ParseJSON([]byte(before))
		dd, _ := ParseJSON([]byte(before))
		entries, _ := dd.Apply(testSets)
		_ = e
		left, err := d.Remove(entries)
		if err != nil || len(left) != 0 || string(d.Bytes()) != `{"theme": "x"}` {
			t.Fatalf("got %q, left %v, %v", d.Bytes(), left, err)
		}
	})
	t.Run("e: the tool rewrote the file", func(t *testing.T) {
		s, e := connected(t)
		var m map[string]any
		json.Unmarshal([]byte(s), &m)
		m["toolOwned"] = true
		b, _ := json.MarshalIndent(m, "\t", "\t") // other indentation, sorted keys
		got, left := remove(t, strings.ReplaceAll(string(b), "\n", "\r\n"), e)
		var after map[string]any
		if err := json.Unmarshal([]byte(got), &after); err != nil {
			t.Fatalf("%v: %q", err, got)
		}
		if len(left) != 0 || len(after) != 2 || after["theme"] != "dark" || after["toolOwned"] != true {
			t.Fatalf("got %q, left %v", got, left)
		}
	})
}

func TestJSONRestoresAReplacedValue(t *testing.T) {
	before := "{\n  \"env\": {\n    \"ANTHROPIC_MODEL\":   \"old\"\n  }\n}\n"
	d, _ := ParseJSON([]byte(before))
	entries, _ := d.Apply(testSets[2:])
	if entries[0].Previous == nil || *entries[0].Previous != `"old"` {
		t.Fatalf("entries = %+v", entries)
	}
	d2, _ := ParseJSON(d.Bytes())
	d2.Remove(entries)
	if string(d2.Bytes()) != before {
		t.Fatalf("got %q", d2.Bytes())
	}
}

func TestParseJSONErrors(t *testing.T) {
	for in, want := range map[string]string{
		"{\n  \"a\": \n}": "line 3",
		"[1, 2]":          "not a JSON object",
		`"text"`:          "not a JSON object",
		"{\"a\": 1} x":    "line 1",
	} {
		_, err := ParseJSON([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseJSON(%q) = %v, want %q", in, err, want)
		}
	}
	d, _ := ParseJSON([]byte(`{"env": "text"}`))
	if _, err := d.Apply(testSets); err == nil || !strings.Contains(err.Error(), "env") {
		t.Errorf("an env that is not an object: err = %v", err)
	}
}

func FuzzRoundTripJSONC(f *testing.F) {
	for _, s := range jsonCases {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		d, err := ParseJSON(in)
		if err != nil {
			return
		}
		entries, err := d.Apply(testSets)
		if err != nil {
			return // an env that is not an object, for example
		}
		d2, err := ParseJSON(d.Bytes())
		if err != nil {
			t.Fatalf("Apply produced a file that does not parse: %v\n%q", err, d.Bytes())
		}
		if _, err := d2.Remove(entries); err != nil || string(d2.Bytes()) != string(in) {
			t.Fatalf("not byte-identical (%v)\n in    %q\n after %q\n out   %q", err, in, d.Bytes(), d2.Bytes())
		}
	})
}

func stripForValid(t *testing.T, b []byte) []byte {
	t.Helper()
	b = []byte(strings.TrimPrefix(string(b), bom))
	s, err := hujson.Standardize(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A second "env" object added after connect hides Ambit's entries from the name lookup (the last one wins, as in
// every JSON parser). Remove must say the entries are still there instead of treating them as gone.
func TestJSONRemoveReportsEntriesHiddenByADuplicateObject(t *testing.T) {
	d, _ := ParseJSON([]byte("{\n  \"theme\": \"dark\"\n}\n"))
	entries, err := d.Apply(testSets)
	if err != nil {
		t.Fatal(err)
	}
	later := strings.Replace(string(d.Bytes()), "\n}\n", ",\n  \"env\": {\"B\": \"2\"}\n}\n", 1)
	d2, err := ParseJSON([]byte(later))
	if err != nil {
		t.Fatal(err)
	}
	left, err := d2.Remove(entries)
	if err != nil {
		t.Fatal(err)
	}
	want := "/env/ANTHROPIC_MODEL,/env/ANTHROPIC_BASE_URL" // last entry first, as Remove goes
	if got := strings.Join(left, ","); got != want {
		t.Fatalf("left = %s, want %s\n%s", got, want, d2.Bytes())
	}
	if strings.Contains(string(d2.Bytes()), "apiKeyHelper") {
		t.Fatalf("the visible entry must still be removed:\n%s", d2.Bytes())
	}
}
