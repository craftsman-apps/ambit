package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/tailscale/hujson"
)

// JSON and JSONC files (Claude Code's settings.json, opencode's opencode.json) are edited as text. hujson parses
// the file and gives the byte offsets of every member; an insert or a removal then changes only the bytes of that
// member. Everything else (comments, indentation, trailing commas, line endings) stays as it was, and the bytes
// Apply inserted are recorded, so Remove can give back the original file exactly.

// Set is one member to write: the value (compact JSON text) at the path of object names.
type Set struct {
	Path  []string
	Value string
}

// Doc is a JSON or JSONC file with an object at the top.
type Doc struct {
	bom bool
	src []byte
	eol string
}

// NewJSON is a new file: "{}" and a line ending.
func NewJSON(eol string) *Doc { return &Doc{src: []byte("{}" + eol), eol: eol} }

// ParseJSON reads a file. The error names the line of a syntax error.
func ParseJSON(b []byte) (*Doc, error) {
	d := &Doc{eol: "\n"}
	if bytes.HasPrefix(b, []byte(bom)) {
		d.bom, b = true, b[len(bom):]
	}
	d.src = append([]byte(nil), b...)
	if i := bytes.IndexByte(d.src, '\n'); i > 0 && d.src[i-1] == '\r' {
		d.eol = "\r\n"
	}
	if _, _, err := d.root(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Doc) Bytes() []byte {
	if d.bom {
		return append([]byte(bom), d.src...)
	}
	return d.src
}

func (d *Doc) root() (hujson.Value, *hujson.Object, error) {
	v, err := hujson.Parse(d.src)
	if err != nil {
		return v, nil, err
	}
	o, ok := v.Value.(*hujson.Object)
	if !ok {
		return v, nil, fmt.Errorf("the top level is not a JSON object")
	}
	return v, o, nil
}

func memberName(m hujson.ObjectMember) string { return m.Name.Value.(hujson.Literal).String() }

// find returns the last member called name, as JSON parsers read duplicate names.
func find(o *hujson.Object, name string) (int, bool) {
	for i := len(o.Members) - 1; i >= 0; i-- {
		if memberName(o.Members[i]) == name {
			return i, true
		}
	}
	return 0, false
}

// walk follows path from the root. It returns the object that holds the last name, and the member's index there.
// When a name is missing, depth is its position in path; when a value on the way is not an object, err says which.
func (d *Doc) walk(path []string) (holder hujson.Value, idx, depth int, err error) {
	rv, cur, err := d.root()
	if err != nil {
		return rv, 0, 0, err
	}
	holder = rv
	for depth = range path {
		i, ok := find(cur, path[depth])
		if !ok {
			return holder, 0, depth, nil
		}
		if depth == len(path)-1 {
			return holder, i, depth, nil
		}
		next, isObj := cur.Members[i].Value.Value.(*hujson.Object)
		if !isObj {
			return holder, 0, depth, fmt.Errorf("%s is not a JSON object", pointer(path[:depth+1]))
		}
		holder, cur = cur.Members[i].Value, next
	}
	return holder, 0, len(path), nil
}

// Get returns the JSON text of the value at path.
func (d *Doc) Get(path ...string) (string, bool) {
	hv, i, depth, err := d.walk(path)
	if err != nil || depth != len(path)-1 {
		return "", false
	}
	h := hv.Value.(*hujson.Object)
	if _, ok := find(h, path[len(path)-1]); !ok {
		return "", false
	}
	v := h.Members[i].Value
	return string(d.src[v.StartOffset:v.EndOffset]), true
}

func pointer(path []string) string {
	var sb strings.Builder
	for _, p := range path {
		sb.WriteString("/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(p))
	}
	return sb.String()
}

func splitPointer(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		out = append(out, strings.NewReplacer("~1", "/", "~0", "~").Replace(s))
	}
	return out
}

func marshalString(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// Apply writes every Set and returns one Entry per change, in the order made. A missing object on the way is
// created first (an entry with CreatedContainer). A value that exists is replaced and its old text is kept.
func (d *Doc) Apply(sets []Set) ([]Entry, error) {
	var entries []Entry
	for _, s := range sets {
		for depth := 1; depth <= len(s.Path); depth++ {
			path := s.Path[:depth]
			hv, i, at, err := d.walk(path)
			if err != nil {
				return nil, err
			}
			holder := hv.Value.(*hujson.Object)
			leaf := depth == len(s.Path)
			if at == depth-1 && memberFound(holder, path[depth-1]) {
				if !leaf {
					continue
				}
				v := holder.Members[i].Value
				prev := string(d.src[v.StartOffset:v.EndOffset])
				d.splice(v.StartOffset, v.EndOffset, []byte(s.Value))
				entries = append(entries, Entry{Pointer: pointer(path), Wrote: s.Value, Previous: &prev})
				continue
			}
			val := "{}"
			if leaf {
				val = s.Value
			}
			ins := d.insertMember(hv, path[depth-1], val)
			e := Entry{Pointer: pointer(path), Wrote: val, Inserted: ins, CreatedContainer: !leaf}
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func memberFound(o *hujson.Object, name string) bool { _, ok := find(o, name); return ok }

func (d *Doc) splice(from, to int, with []byte) {
	d.src = append(append(append([]byte(nil), d.src[:from]...), with...), d.src[to:]...)
}

// lineIndent is the white space at the start of the line that holds offset off.
func (d *Doc) lineIndent(off int) string {
	start := bytes.LastIndexByte(d.src[:off], '\n') + 1
	end := start
	for end < len(d.src) && (d.src[end] == ' ' || d.src[end] == '\t') {
		end++
	}
	return string(d.src[start:end])
}

// unit is one level of indentation in this file: what the first member of the top object is indented with.
func (d *Doc) unit() string {
	if _, o, err := d.root(); err == nil && len(o.Members) > 0 {
		if ind := trailingIndent(o.Members[0].Name.BeforeExtra); ind != "" {
			return ind
		}
	}
	return "  "
}

// trailingIndent is the spaces and tabs after the last line break in extra ("" when there is no line break).
func trailingIndent(extra hujson.Extra) string {
	i := bytes.LastIndexByte(extra, '\n')
	if i < 0 {
		return ""
	}
	rest := extra[i+1:]
	n := 0
	for n < len(rest) && (rest[n] == ' ' || rest[n] == '\t') {
		n++
	}
	return string(rest[:n])
}

// render is the member text `"name": value`, with a multi-line value indented under indent.
func (d *Doc) render(name, value, indent string, multiline bool) string {
	var b bytes.Buffer
	if multiline && json.Indent(&b, []byte(value), indent, d.unit()) == nil {
		value = strings.ReplaceAll(b.String(), "\n", d.eol)
	}
	return marshalString(name) + ": " + value
}

// insertMember appends a member to the end of o and returns the exact bytes it inserted.
func (d *Doc) insertMember(hv hujson.Value, name, value string) string {
	o := hv.Value.(*hujson.Object)
	multiline := bytes.IndexByte(d.src[hv.StartOffset:hv.EndOffset], '\n') >= 0
	var at int
	var ins string
	if n := len(o.Members); n > 0 {
		last := o.Members[n-1]
		at = last.Value.EndOffset
		sep := ","
		indent := trailingIndent(last.Name.BeforeExtra)
		if multiline {
			if indent == "" && !bytes.Contains(last.Name.BeforeExtra, []byte("\n")) {
				indent = d.lineIndent(last.Name.StartOffset)
			}
			sep += d.eol + indent
		} else {
			sep += string(last.Name.BeforeExtra[len(bytes.TrimRight(last.Name.BeforeExtra, " \t")):])
			if sep == "," && len(last.Name.BeforeExtra) > 0 {
				sep = "," + string(last.Name.BeforeExtra)
			}
		}
		ins = sep + d.render(name, value, indent, multiline)
	} else {
		start := hv.StartOffset
		at = start + 1
		parent := d.lineIndent(start)
		child := parent + d.unit()
		if !bytes.Contains(d.src, []byte("\n")) { // the whole file is one line: keep it that way
			ins = d.render(name, value, "", false)
		} else if bytes.Contains(o.AfterExtra, []byte("\n")) {
			ins = d.eol + child + d.render(name, value, child, true)
		} else {
			ins = d.eol + child + d.render(name, value, child, true) + d.eol + parent
		}
	}
	d.splice(at, at, []byte(ins))
	return ins
}

// Remove undoes entries, last first. An entry is removed only while its value is still what Apply wrote; one that
// the person or the tool changed stays, and its pointer is returned. A container Apply created is removed when it
// is empty. When the inserted bytes are still in place they are cut out exactly, which restores the original
// file; otherwise the member is removed by meaning and the rest of the file is kept as it is.
func (d *Doc) Remove(entries []Entry) (left []string, err error) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		path := splitPointer(e.Pointer)
		hv, idx, depth, err := d.walk(path)
		if err != nil {
			return left, err
		}
		if depth != len(path)-1 || !memberFound(hv.Value.(*hujson.Object), path[len(path)-1]) {
			if !e.CreatedContainer && d.hiddenByDuplicate(path) {
				left = append(left, e.Pointer) // an earlier object of the same name still holds it
			}
			continue // the member is gone already
		}
		holder := hv.Value.(*hujson.Object)
		m := holder.Members[idx]
		cur := d.src[m.Value.StartOffset:m.Value.EndOffset]
		if !jsonEqual(string(cur), e.Wrote) {
			if !e.CreatedContainer { // a container that still holds something is not a change of ours
				left = append(left, e.Pointer)
			}
			continue
		}
		if e.Previous != nil {
			d.splice(m.Value.StartOffset, m.Value.EndOffset, []byte(*e.Previous))
			continue
		}
		d.removeMember(hv, idx, e.Inserted)
	}
	return left, nil
}

// hiddenByDuplicate reports whether path exists in some object when an object of the same name comes after it:
// find reads the last one, so the entry would look gone.
func (d *Doc) hiddenByDuplicate(path []string) bool {
	_, root, err := d.root()
	if err != nil {
		return false
	}
	var has func(o *hujson.Object, path []string) bool
	has = func(o *hujson.Object, path []string) bool {
		for _, m := range o.Members {
			if memberName(m) != path[0] {
				continue
			}
			if len(path) == 1 {
				return true
			}
			if sub, ok := m.Value.Value.(*hujson.Object); ok && has(sub, path[1:]) {
				return true
			}
		}
		return false
	}
	return has(root, path)
}

func (d *Doc) removeMember(hv hujson.Value, i int, inserted string) {
	o := hv.Value.(*hujson.Object)
	m := o.Members[i]
	start := hv.StartOffset + 1
	if i > 0 {
		start = o.Members[i-1].Value.EndOffset
	}
	if inserted != "" && bytes.HasPrefix(d.src[start:], []byte(inserted)) && m.Name.StartOffset >= start && m.Value.EndOffset <= start+len(inserted) {
		d.splice(start, start+len(inserted), nil)
		return
	}
	switch {
	case i > 0: // from the comma before it to the end of its value
		prev := o.Members[i-1]
		d.splice(prev.Value.EndOffset+len(prev.Value.AfterExtra), m.Value.EndOffset, nil)
	case len(o.Members) > 1: // from its name to the name of the next member
		d.splice(m.Name.StartOffset, o.Members[1].Name.StartOffset, nil)
	default:
		rest := bytes.Replace(d.src[hv.StartOffset+1:hv.EndOffset-1], d.src[m.Name.StartOffset:m.Value.EndOffset], nil, 1)
		if len(bytes.Trim(rest, " \t\r\n,")) == 0 { // only white space and a comma are left: the object is {}
			d.splice(hv.StartOffset+1, hv.EndOffset-1, nil)
			return
		}
		end := m.Value.EndOffset
		if m.Value.AfterExtra != nil { // a trailing comma follows
			end += len(m.Value.AfterExtra) + 1
		}
		d.splice(m.Name.StartOffset, end, nil)
	}
}

// jsonEqual compares two JSON(C) texts by meaning.
func jsonEqual(a, b string) bool {
	parse := func(s string) (any, bool) {
		std, err := hujson.Standardize([]byte(s))
		if err != nil {
			return nil, false
		}
		var v any
		return v, json.Unmarshal(std, &v) == nil
	}
	x, ok1 := parse(a)
	y, ok2 := parse(b)
	return ok1 && ok2 && reflect.DeepEqual(x, y)
}

// Delete removes the member at path and reports whether there was one. Nothing is recorded: it is for a value that
// must not be kept anywhere but in the backup file (a secret).
func (d *Doc) Delete(path ...string) bool {
	hv, idx, depth, err := d.walk(path)
	if err != nil || depth != len(path)-1 || !memberFound(hv.Value.(*hujson.Object), path[len(path)-1]) {
		return false
	}
	d.removeMember(hv, idx, "")
	return true
}
