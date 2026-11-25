package connect

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// Hermes Agent's config.yaml is edited as text, like Codex's config.toml: every change is a marked block, and
// removing the blocks gives back the original file. The YAML parser only checks the result.
//
// ponytail: the line scanner below finds top-level keys (a name at column 0) and the children of a block mapping;
// it is not a YAML parser. The check at the end of PlanHermes (the decoded file must equal the decoded original
// plus Ambit's keys) is the safety net: anything the scanner misreads stops with "change it by hand".

var hermesBegin = blockBegin(Hermes)

// HermesHome is $HERMES_HOME, else %LOCALAPPDATA%\hermes on Windows (native install), else ~/.hermes. A named
// profile (hermes -p <name>) has its own home; set HERMES_HOME to connect it.
func HermesHome(goos string, getenv func(string) string, home string) string {
	if d := getenv("HERMES_HOME"); d != "" {
		return d
	}
	if goos == "windows" {
		if d := getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "hermes")
		}
		return filepath.Join(home, "AppData", "Local", "hermes")
	}
	return filepath.Join(home, ".hermes")
}

// HermesConfigPath is config.yaml in HermesHome.
func HermesConfigPath(goos string, getenv func(string) string, home string) string {
	return filepath.Join(HermesHome(goos, getenv, home), "config.yaml")
}

// HermesHeaderPath is the header file of Hermes' MCP entry. Hermes reads MCP headers only from the config or from
// environment variables, so its entry is mcp-remote with this file, like claude-desktop-mcp's.
func HermesHeaderPath(home string) string {
	return filepath.Join(home, ".ambit", "hermes-mcp-headers.txt")
}

var (
	yamlTopKey   = regexp.MustCompile(`^(?:"([^"]*)"|'([^']*)'|([A-Za-z_][A-Za-z0-9_-]*))[ \t]*:(?:[ \t]+(.*))?$`)
	yamlChildKey = regexp.MustCompile(`^[ \t]+(?:"([^"]*)"|'([^']*)'|([A-Za-z0-9_.-]+))[ \t]*:(?:[ \t]|$)`)
)

func yamlContent(s string) bool {
	t := strings.TrimLeft(s, " \t")
	return t != "" && !strings.HasPrefix(t, "#")
}
func yamlIndent(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// yamlInline is the value after "key:" on the same line, without a comment.
func yamlInline(v string) string {
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	if strings.HasPrefix(v, "#") {
		v = ""
	}
	return strings.TrimSpace(v)
}

// yamlEnd is the end (exclusive) of the lines that belong to the entry at line i: up to the next line at an indent
// of at most indent, without the blank and comment lines before it.
func yamlEnd(lines []line, i, indent int, limit int) int {
	last := i
	for j := i + 1; j < limit; j++ {
		s := lines[j].text
		if !yamlContent(s) {
			continue
		}
		if yamlIndent(s) <= indent {
			break
		}
		last = j
	}
	return last + 1
}

func decodeYAML(path string, b []byte) (map[string]any, error) {
	m := map[string]any{}
	if err := yaml.Unmarshal(bytes.TrimPrefix(b, []byte(bom)), &m); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML — %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func indentLines(prefix string, ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = prefix + l
	}
	return out
}

func hermesProviderLines(in Input) []string {
	return []string{"ambit:",
		"  base_url: " + marshalString(in.URL+"/v1"),
		`  api_mode: "chat_completions"`,
		"  key_cmd: " + marshalString(QuoteCmd(in.GOOS, in.AmbitPath)+" key print --url "+QuoteCmd(in.GOOS, in.URL)),
	}
}

func hermesMCPArgs(in Input, headerPath string) []string {
	args := []string{"-y", MCPRemote, in.MCPURL}
	if needsAllowHTTP(in.MCPURL) {
		args = append(args, "--allow-http")
	}
	return append(args, "--header-file", headerPath)
}

func hermesMCPLines(in Input, headerPath string) []string {
	var q []string
	for _, a := range hermesMCPArgs(in, headerPath) {
		q = append(q, marshalString(a))
	}
	return []string{"ambit:", `  command: "npx"`, "  args: [" + strings.Join(q, ", ") + "]"}
}

// PlanHermes plans the model, the provider "ambit" (its key_cmd runs ambit key print) and, with in.MCPURL, the MCP
// server "ambit" in config.yaml. A parse error, a block the person edited, and an entry the scanner cannot edit
// are errors and change nothing.
func PlanHermes(in Input, headerPath string) (*Plan, error) {
	if err := CheckCmdSafe(in.GOOS, "Hermes Agent", in.AmbitPath, in.URL); err != nil {
		return nil, err
	}
	if in.Exists {
		if _, err := decodeYAML(in.Path, in.File); err != nil {
			return nil, err
		}
	}
	cur := ParseText(in.File)
	var bodies []string // the blocks of an earlier connect
	if blocks, err := cur.Blocks(); err != nil {
		return nil, fmt.Errorf("%s: %w", in.Path, err)
	} else if len(blocks) > 0 {
		for _, b := range blocks {
			bodies = append(bodies, cur.BlockBody(b))
		}
		if notes, _ := removeBlocks(cur, in.Prev); len(notes) > 0 {
			return nil, fmt.Errorf("the Ambit block in %s was edited after connect — remove the lines between its markers by hand, then run the command again", in.Path)
		}
	}
	// decided: the person let an earlier connect replace this entry (--force). A block holds the lines it replaced
	// and the lines of one entry: the model, the provider (key_cmd) or the MCP server (mcp-remote).
	decided := func(entry string) bool {
		for _, b := range bodies {
			ls := strings.Split(b, "\n")
			if len(ls) == 0 || !strings.HasPrefix(ls[0], replacedPrefix) {
				continue
			}
			m := yamlChildKey.FindStringSubmatch(" " + strings.TrimLeft(strings.TrimPrefix(ls[0], replacedPrefix), " \t"))
			switch {
			case m == nil:
			case entry == "model" && m[1]+m[2]+m[3] == "model",
				entry == "providers" && m[1]+m[2]+m[3] == "ambit" && strings.Contains(b, "key_cmd: "),
				entry == "mcp_servers" && m[1]+m[2]+m[3] == "ambit" && strings.Contains(b, MCPRemote):
				return true
			}
		}
		return false
	}
	orig := cur.Bytes()
	origMap, err := decodeYAML(in.Path, orig)
	if err != nil {
		return nil, err
	}
	lines := cur.lines
	cantEdit := func(key string) error {
		return fmt.Errorf("%s sets %s in a way ambit cannot edit — change it by hand, then run the command again", in.Path, key)
	}
	top := map[string]int{} // top-level key → its line
	for i, l := range lines {
		m := yamlTopKey.FindStringSubmatch(l.text)
		if m == nil {
			continue
		}
		key := m[1] + m[2] + m[3]
		if _, dup := top[key]; dup || m[3] == "" { // a key twice, or a quoted key: YAML reads the last one
			if key == "model" || key == "providers" || key == "mcp_servers" {
				return nil, cantEdit(key)
			}
		}
		top[key] = i
	}

	p := &Plan{}
	var ops []tomlOp
	modelLines := []string{"model:", `  provider: "custom:ambit"`, "  default: " + marshalString(in.Model)}
	wantModel := map[string]any{"provider": "custom:ambit", "default": in.Model}
	if i, ok := top["model"]; ok {
		if !reflect.DeepEqual(origMap["model"], wantModel) && !decided("model") {
			p.Conflicts = append(p.Conflicts, Conflict{What: "model is set to another value", Why: "Hermes would keep using it instead of Ambit's"})
		}
		ops = append(ops, tomlOp{at: i, del: yamlEnd(lines, i, 0, len(lines)) - i, rank: 1, body: modelLines})
	} else {
		ops = append(ops, tomlOp{at: len(lines), rank: 2, body: modelLines, pad: true})
	}

	// child adds the entry "ambit" to the top-level mapping key.
	child := func(key string, entry []string, rank int) error {
		i, ok := top[key]
		if !ok {
			ops = append(ops, tomlOp{at: len(lines), rank: rank, body: append([]string{key + ":"}, indentLines("  ", entry)...), pad: true})
			return nil
		}
		m := yamlTopKey.FindStringSubmatch(lines[i].text)
		inline := yamlInline(m[4])
		end := yamlEnd(lines, i, 0, len(lines))
		first := -1
		for j := i + 1; j < end; j++ {
			if yamlContent(lines[j].text) {
				first = j
				break
			}
		}
		if first < 0 { // `key:`, `key: {}` or `key: null` with nothing below: the line becomes the mapping
			if inline != "" && inline != "{}" && inline != "null" && inline != "~" {
				return cantEdit(key)
			}
			ops = append(ops, tomlOp{at: i, del: 1, rank: rank, body: append([]string{key + ":"}, indentLines("  ", entry)...)})
			return nil
		}
		ind := lines[first].text[:yamlIndent(lines[first].text)]
		if inline != "" || strings.HasPrefix(strings.TrimLeft(lines[first].text, " \t"), "-") || strings.Contains(ind, "\t") {
			return cantEdit(key)
		}
		at := -1
		for j := first; j < end; j++ {
			s := lines[j].text
			if !yamlContent(s) || yamlIndent(s) != len(ind) {
				continue
			}
			if cm := yamlChildKey.FindStringSubmatch(s); cm != nil && cm[1]+cm[2]+cm[3] == "ambit" {
				if at >= 0 { // twice: YAML reads the last one
					return cantEdit(key + ".ambit")
				}
				at = j
			}
		}
		if at < 0 {
			ops = append(ops, tomlOp{at: i + 1, rank: rank, body: indentLines(ind, entry)})
			return nil
		}
		if !decided(key) {
			p.Conflicts = append(p.Conflicts, Conflict{What: key + ".ambit already exists", Why: "Ambit adds an entry of that name"})
		}
		ops = append(ops, tomlOp{at: at, del: yamlEnd(lines, at, len(ind), end) - at, rank: rank, body: indentLines(ind, entry)})
		return nil
	}
	if err := child("providers", hermesProviderLines(in), 3); err != nil {
		return nil, err
	}
	if in.MCPURL != "" {
		if err := child("mcp_servers", hermesMCPLines(in, headerPath), 4); err != nil {
			return nil, err
		}
	}
	if len(p.Conflicts) > 0 && !in.Force {
		return p, nil
	}
	after, recs, wrote := applyOps(orig, hermesBegin, ops)

	// The safety net: the decoded result is the decoded original with Ambit's keys, and nothing else.
	got, err := decodeYAML(in.Path, after)
	if err != nil {
		return nil, fmt.Errorf("ambit would write a file that is not valid YAML (%w); nothing was changed", err)
	}
	want, _ := decodeYAML(in.Path, orig)
	want["model"] = wantModel
	add := func(key string, v map[string]any) {
		m, _ := want[key].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		m["ambit"] = v
		want[key] = m
	}
	add("providers", map[string]any{"base_url": in.URL + "/v1", "api_mode": "chat_completions",
		"key_cmd": QuoteCmd(in.GOOS, in.AmbitPath) + " key print --url " + QuoteCmd(in.GOOS, in.URL)})
	if in.MCPURL != "" {
		var args []any
		for _, a := range hermesMCPArgs(in, headerPath) {
			args = append(args, a)
		}
		add("mcp_servers", map[string]any{"command": "npx", "args": args})
	}
	if !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("ambit could not edit %s safely (the result would not read as intended); nothing was changed — add the Ambit entries by hand", in.Path)
	}

	p.After = after
	p.Record = FileRecord{Path: in.Path, Created: !in.Exists, Blocks: recs, Wrote: wrote}
	if in.Prev != nil && in.Exists {
		p.Record.Created, p.Record.Dirs = in.Prev.Created, in.Prev.Dirs
	}
	if in.Exists && bytes.Equal(after, in.File) {
		p.Already = true
		if in.Prev != nil {
			p.Record = *in.Prev
		}
	}
	return p, nil
}
