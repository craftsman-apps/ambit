package connect

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Codex is edited as text and never written back from a parsed form. Every change is a marked block (blockedit.go);
// removing all blocks gives back the original file. BurntSushi/toml only checks the result.
//
// ponytail: the line scanner below finds table headers; it is not a TOML parser. The check at the end of
// PlanCodex (the decoded file must equal the decoded original plus Ambit's keys) is the safety net. Upgrade path:
// a format-preserving TOML library, if one appears with a usable license.

const (
	codexBegin     = "# >>> ambit: written by `ambit connect codex`; `ambit disconnect codex` removes it >>>"
	replacedPrefix = "# ambit-replaced: "
)

// CodexConfigPath is $CODEX_HOME/config.toml, else ~/.codex/config.toml. Never a project file.
func CodexConfigPath(getenv func(string) string, home string) string {
	if d := getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func tomlErr(path string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s is not valid TOML — line %d: %s", path, pe.Position.Line, pe.Message)
	}
	return fmt.Errorf("%s is not valid TOML — %w", path, err)
}

func decodeTOML(path string, b []byte) (map[string]any, error) {
	m := map[string]any{}
	if _, err := toml.Decode(strings.TrimPrefix(string(b), bom), &m); err != nil {
		return nil, tomlErr(path, err)
	}
	return m, nil
}

type tomlHeader struct {
	line int
	path []string
}

// scanTOML finds the table headers: lines that start with "[" outside a multi-line string and outside an array.
func scanTOML(lines []line) []tomlHeader {
	var hs []tomlHeader
	ml := "" // the closing delimiter of an open multi-line string
	depth := 0
	for i, l := range lines {
		s := l.text
		if ml == "" && depth == 0 {
			if t := strings.TrimLeft(s, " \t"); strings.HasPrefix(t, "[") {
				hs = append(hs, tomlHeader{i, headerPath(t)})
				continue
			}
		}
		for j := 0; j < len(s); {
			switch {
			case ml == `"""`:
				switch {
				case s[j] == '\\':
					j += 2
				case strings.HasPrefix(s[j:], `"""`):
					ml, j = "", j+3
				default:
					j++
				}
			case ml == `'''`:
				if strings.HasPrefix(s[j:], `'''`) {
					ml, j = "", j+3
				} else {
					j++
				}
			case s[j] == '#':
				j = len(s)
			case strings.HasPrefix(s[j:], `"""`), strings.HasPrefix(s[j:], `'''`):
				ml, j = s[j:j+3], j+3
			case s[j] == '"' || s[j] == '\'':
				q := s[j]
				for j++; j < len(s) && s[j] != q; j++ {
					if q == '"' && s[j] == '\\' {
						j++
					}
				}
				j++
			case s[j] == '[':
				depth++
				j++
			case s[j] == ']':
				if depth > 0 {
					depth--
				}
				j++
			default:
				j++
			}
		}
	}
	return hs
}

// headerPath reads the key parts of a header line such as [a."b c".d] or [[a.b]].
func headerPath(t string) []string {
	t = strings.TrimPrefix(strings.TrimPrefix(t, "["), "[")
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c == ']':
			return append(parts, cur.String())
		case c == '.':
			parts, cur = append(parts, cur.String()), strings.Builder{}
		case c == '"':
			j := i + 1
			for j < len(t) && t[j] != '"' {
				if t[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(t) {
				return append(parts, cur.String())
			}
			if u, err := strconv.Unquote(t[i : j+1]); err == nil {
				cur.WriteString(u)
			} else {
				cur.WriteString(t[i+1 : j])
			}
			i = j
		case c == '\'':
			j := strings.IndexByte(t[i+1:], '\'')
			if j < 0 {
				return append(parts, cur.String())
			}
			cur.WriteString(t[i+1 : i+1+j])
			i += j + 1
		case c == ' ' || c == '\t':
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, cur.String())
}

func isComment(s string) bool { return strings.HasPrefix(strings.TrimLeft(s, " \t"), "#") }

func keyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*(?:"` + regexp.QuoteMeta(key) + `"|'` + regexp.QuoteMeta(key) + `'|` + regexp.QuoteMeta(key) + `)\s*=`)
}

func keyLine(lines []line, key string) int {
	re := keyRe(key)
	for i, l := range lines {
		if re.MatchString(l.text) {
			return i
		}
	}
	return -1
}

func commented(l line) string {
	if l.text == "" {
		return strings.TrimRight(replacedPrefix, " ")
	}
	return replacedPrefix + l.text
}

type tomlOp struct {
	at, del, rank int
	body          []string // after the commented lines of the replaced original
	pad           bool
}

func codexProviderLines(in Input) []string {
	return []string{
		"[model_providers.ambit]",
		`name = "Ambit"`,
		"base_url = " + marshalString(in.URL+"/v1"),
		`wire_api = "responses"`,
		"",
		"[model_providers.ambit.auth]",
		"command = " + marshalString(in.AmbitPath),
		`args = ["key", "print", "--url", ` + marshalString(in.URL) + `]`,
	}
}

// PlanCodex plans the blocks of section 5.2. A parse error, and a block the person edited, are returned as errors
// and change nothing.
func PlanCodex(in Input) (*Plan, error) {
	if in.Exists {
		if _, err := decodeTOML(in.Path, in.File); err != nil {
			return nil, err
		}
	}
	cur := ParseText(in.File)
	var replacedBefore []string // lines an earlier connect replaced with --force: the person decided those already
	if blocks, err := cur.Blocks(); err != nil {
		return nil, fmt.Errorf("%s: %w", in.Path, err)
	} else if len(blocks) > 0 {
		// Ambit's earlier blocks are ours: take them out, and write them again below.
		var notes []string
		if notes, replacedBefore = removeBlocks(cur, in.Prev); len(notes) > 0 {
			return nil, fmt.Errorf("the Ambit block in %s was edited after connect — remove the lines between its markers by hand, then run the command again", in.Path)
		}
	}
	decided := func(match func(string) bool) bool {
		for _, l := range replacedBefore {
			if match(l) {
				return true
			}
		}
		return false
	}
	orig := cur.Bytes()
	origMap, err := decodeTOML(in.Path, orig)
	if err != nil {
		return nil, err
	}
	lines := cur.lines
	headers := scanTOML(lines)
	first := len(lines)
	if len(headers) > 0 {
		first = headers[0].line
	}
	topAt := first
	for first < len(lines) && topAt > 0 && isComment(lines[topAt-1].text) { // a comment above a table belongs to it
		topAt--
	}

	p := &Plan{}
	var ops []tomlOp
	var top []string
	for _, k := range []struct{ key, want string }{{"model", in.Model}, {"model_provider", "ambit"}} {
		v, ok := origMap[k.key]
		newLine := k.key + " = " + marshalString(k.want)
		switch {
		case !ok:
			top = append(top, newLine)
		case v == any(k.want):
		default:
			if !decided(keyRe(k.key).MatchString) {
				p.Conflicts = append(p.Conflicts, Conflict{What: k.key + " is set to another value", Why: "Codex would keep using it instead of Ambit's"})
			}
			if i := keyLine(lines[:first], k.key); i >= 0 {
				ops = append(ops, tomlOp{at: i, del: 1, rank: 1, body: []string{newLine}})
			} else if in.Force {
				return nil, fmt.Errorf("%s sets %s in a way ambit cannot edit — change it by hand, then run the command again", in.Path, k.key)
			}
		}
	}
	if mp, ok := origMap["model_providers"].(map[string]any); ok {
		if _, ok := mp["ambit"]; ok {
			if !decided(func(l string) bool {
				h := headerPath(strings.TrimSpace(l))
				return strings.HasPrefix(strings.TrimSpace(l), "[") && len(h) >= 2 && h[0] == "model_providers" && h[1] == "ambit"
			}) {
				p.Conflicts = append(p.Conflicts, Conflict{What: "[model_providers.ambit] already exists", Why: "Ambit adds a table of that name"})
			}
			ranges := ambitTableRanges(lines, headers)
			if len(ranges) == 0 && in.Force {
				return nil, fmt.Errorf("%s defines model_providers.ambit in a way ambit cannot edit — change it by hand, then run the command again", in.Path)
			}
			for _, r := range ranges {
				ops = append(ops, tomlOp{at: r[0], del: r[1] - r[0], rank: 2})
			}
		}
	}
	if len(p.Conflicts) > 0 && !in.Force {
		return p, nil
	}
	if len(top) > 0 {
		ops = append(ops, tomlOp{at: topAt, rank: 0, body: top, pad: true})
	}
	ops = append(ops, tomlOp{at: len(lines), rank: 3, body: codexProviderLines(in), pad: true})
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].at != ops[j].at {
			return ops[i].at < ops[j].at
		}
		return ops[i].rank < ops[j].rank
	})

	t := ParseText(orig)
	n0 := len(t.lines)
	var recs []BlockRecord
	var wrote []string
	for _, op := range ops {
		at := op.at + len(t.lines) - n0
		var rec BlockRecord
		var body []string
		if op.del > 0 {
			for _, l := range t.lines[at : at+op.del] {
				body = append(body, commented(l))
				rec.ReplacedEOL = append(rec.ReplacedEOL, l.eol)
			}
			t.lines = append(t.lines[:at:at], t.lines[at+op.del:]...)
		}
		body = append(body, op.body...)
		ins := t.insert(at, codexBegin, body, op.pad)
		rec.AddedFinalNewline, rec.Padded = ins.AddedFinalNewline, ins.Padded
		recs = append(recs, rec)
		wrote = append(wrote, strings.Join(body, "\n"))
	}
	after := t.Bytes()

	// The safety net: the decoded result is the decoded original with Ambit's keys, and nothing else.
	got, err := decodeTOML(in.Path, after)
	if err != nil {
		return nil, fmt.Errorf("ambit would write a file that is not valid TOML (%w); nothing was changed", err)
	}
	want, _ := decodeTOML(in.Path, orig)
	want["model"], want["model_provider"] = in.Model, "ambit"
	mp, _ := want["model_providers"].(map[string]any)
	if mp == nil {
		mp = map[string]any{}
		want["model_providers"] = mp
	}
	mp["ambit"] = map[string]any{"name": "Ambit", "base_url": in.URL + "/v1", "wire_api": "responses",
		"auth": map[string]any{"command": in.AmbitPath, "args": []any{"key", "print", "--url", in.URL}}}
	if !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("ambit could not edit %s safely (the result would not read as intended); nothing was changed — add the Ambit provider by hand", in.Path)
	}

	p.After = after
	p.Record = FileRecord{Path: in.Path, Created: !in.Exists, Blocks: recs, Wrote: wrote}
	if in.Prev != nil && in.Exists { // a disconnect restores the very first state
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

// ambitTableRanges are the line ranges of the tables model_providers.ambit and its sub-tables: from the header to
// the next header of another table.
func ambitTableRanges(lines []line, hs []tomlHeader) [][2]int {
	var out [][2]int
	for i, h := range hs {
		if len(h.path) < 2 || h.path[0] != "model_providers" || h.path[1] != "ambit" {
			continue
		}
		end := len(lines)
		if i+1 < len(hs) {
			end = hs[i+1].line
		}
		if n := len(out); n > 0 && out[n-1][1] == h.line {
			out[n-1][1] = end
			continue
		}
		out = append(out, [2]int{h.line, end})
	}
	return out
}

// removeBlocks takes Ambit's blocks out of t: it puts back the lines a block replaced, and undoes what the insert
// changed. rec says which blocks connect wrote (a block whose body differs was edited and stays, and the result
// lists it); nil rec means every marked block is Ambit's, which the markers prove. The blocks must be well formed.
func removeBlocks(t *Text, rec *FileRecord) (notes, replaced []string) {
	blocks, _ := t.Blocks()
	recFor := make([]int, len(blocks)) // index into rec.Blocks, or -1: edited, or -2: no record
	taken := map[int]bool{}
	for i, b := range blocks {
		recFor[i] = -2
		if rec == nil {
			continue
		}
		recFor[i] = -1
		body := t.BlockBody(b)
		for j, w := range rec.Wrote {
			if j < len(rec.Blocks) && !taken[j] && w == body {
				recFor[i], taken[j] = j, true
				break
			}
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if recFor[i] == -1 {
			notes = append([]string{"the Ambit block in " + rec.Path + " was edited after connect; left as it is"}, notes...)
			continue
		}
		var br BlockRecord
		if recFor[i] >= 0 {
			br = rec.Blocks[recFor[i]]
		}
		replaced = append(replaced, restoreBlock(t, blocks[i], br)...)
	}
	return notes, replaced
}

// restoreBlock gives back the lines the block replaced, and returns their text.
func restoreBlock(t *Text, b Block, br BlockRecord) (texts []string) {
	var back []line
	for _, l := range t.lines[b.Start+1 : b.End] {
		switch {
		case l.text == strings.TrimRight(replacedPrefix, " "):
			back = append(back, line{"", t.eol})
		case strings.HasPrefix(l.text, replacedPrefix):
			back = append(back, line{strings.TrimPrefix(l.text, replacedPrefix), t.eol})
		}
	}
	if len(back) == 0 {
		t.RemoveBlock(b, br)
		return nil
	}
	for i := range back {
		if i < len(br.ReplacedEOL) {
			back[i].eol = br.ReplacedEOL[i]
		}
		texts = append(texts, back[i].text)
	}
	t.lines = append(t.lines[:b.Start:b.Start], append(back, t.lines[b.End+1:]...)...)
	return texts
}

// UnplanCodex plans the disconnect of the Codex file. rec nil: nothing is recorded, and every marked block is
// removed. A block the person edited stays, with a note. A file that is gone is not an error.
func UnplanCodex(rec *FileRecord, current []byte, exists bool) (*Disconnected, error) {
	r := &Disconnected{}
	if !exists {
		return r, nil
	}
	t := ParseText(current)
	if _, err := t.Blocks(); err != nil {
		return nil, err
	}
	r.Notes, _ = removeBlocks(t, rec)
	r.After = t.Bytes()
	if rec != nil && rec.Created && len(r.After) == 0 {
		r.Delete, r.RemoveDirs = true, rec.Dirs
	}
	return r, nil
}
