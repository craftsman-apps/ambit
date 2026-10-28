package guard

import (
	"regexp"
	"strings"
)

var blockHeadRe = regexp.MustCompile(`:\s*[|>][+\-0-9]*\s*(#.*)?$`)

// yamlLine reports whether the line is "key: value" (the key may be quoted), "- item", or a bare "key:".
func yamlLine(l string) bool {
	t := strings.TrimLeft(l, " ")
	if strings.HasPrefix(t, "- ") {
		return true
	}
	if t != "" && (t[0] == '"' || t[0] == '\'') {
		i := strings.IndexByte(t[1:], t[0])
		if i < 0 {
			return false
		}
		t = t[i+2:]
		return strings.HasPrefix(t, ":")
	}
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c == ':':
			if i+1 == len(t) || t[i+1] == ' ' || t[i+1] == '\t' {
				return i > 0
			}
		case isWordByte(c) || c == '.' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return false
}

// compressYAML works on lines and never writes YAML again, so keys, values, order and anchors stay as they are.
// Always: it removes trailing spaces and blank lines outside block scalars (inside one they are content).
// With shorten it also removes full-line comments and metadata.managedFields blocks, each with a marker.
// omitted counts the markers.
func compressYAML(in string, shorten bool) (out string, omitted int, ok bool) {
	in = strings.TrimRight(in, "\n")
	content, matching := 0, 0
	for rest := in; rest != ""; {
		var l string
		l, rest, _ = strings.Cut(rest, "\n")
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			content++
			if yamlLine(l) {
				matching++
			}
		}
	}
	// No YAML parser here: it needs 30 times the text in memory. Detection is by lines instead: 80% of the
	// content lines are "key: value" or "- item", and no line is indented with a tab (invalid in YAML).
	if strings.Count(in, "\n") < 4 || content == 0 || matching*10 < content*8 || strings.Contains(in, "\n\t") {
		return "", 0, false
	}
	var b strings.Builder
	b.Grow(len(in))
	block := -1 // indent of the line that opened a block scalar, or -1
	skip := -1  // indent of a managedFields key being dropped, or -1
	for rest := in; rest != ""; {
		var l string
		l, rest, _ = strings.Cut(rest, "\n")
		ind := len(l) - len(strings.TrimLeft(l, " "))
		blank := strings.TrimSpace(l) == ""
		if block >= 0 {
			if blank || ind > block {
				b.WriteString(l + "\n")
				continue
			}
			block = -1
		}
		if skip >= 0 {
			if blank || ind > skip || (ind == skip && strings.HasPrefix(l[ind:], "- ")) {
				continue
			}
			skip = -1
		}
		if blank {
			continue
		}
		t := strings.TrimLeft(l, " ")
		if shorten {
			if strings.HasPrefix(t, "#") {
				continue
			}
			if t == "managedFields:" || strings.HasPrefix(t, "managedFields: ") {
				skip = ind
				omitted++
				b.WriteString(l[:ind] + "# [ambit: managedFields omitted here]\n")
				continue
			}
		}
		l = strings.TrimRight(l, " \t")
		if strings.ContainsAny(l, "|>") && blockHeadRe.MatchString(l) {
			block = ind
		}
		b.WriteString(l + "\n")
	}
	return b.String(), omitted, true
}
