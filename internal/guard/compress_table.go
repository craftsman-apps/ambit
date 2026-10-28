package guard

import (
	"encoding/csv"
	"io"
	"strings"
)

// compressTable handles CSV, TSV, pipe and Markdown tables. It keeps every row and cell; it removes the
// padding around cells in pipe tables, the separator row, and blank lines. CSV and TSV are written again
// through encoding/csv (the same cells, minimal quoting, no blank lines). Rows are streamed, never all held.
func compressTable(in string) (out string, ok bool) {
	head := in[:min(len(in), 4096)]
	l0, rest, _ := strings.Cut(head, "\n")
	l1, _, _ := strings.Cut(rest, "\n")
	if isSep(l1) || strings.Count(l0, "|") >= 2 {
		return compressPipeTable(in)
	}
	for _, comma := range []rune{'\t', ','} {
		if o, ok := compressCSV(in, comma); ok {
			return o, true
		}
	}
	return "", false
}

func compressCSV(in string, comma rune) (string, bool) {
	r := csv.NewReader(strings.NewReader(in))
	r.Comma = comma // FieldsPerRecord is 0: every row must have as many cells as the first (ragged is an error)
	r.ReuseRecord = true
	var b strings.Builder
	b.Grow(len(in))
	w := csv.NewWriter(&b)
	w.Comma = comma
	rows := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || (rows == 0 && len(rec) < 2) {
			return "", false
		}
		_ = w.Write(rec)
		rows++
	}
	w.Flush()
	return b.String(), rows >= 5
}

// isSep reports whether l is a Markdown or box separator row: only | + : - and spaces, with at least one dash.
func isSep(l string) bool {
	return strings.Contains(l, "-") && strings.Trim(l, "|+:- \t") == ""
}

func compressPipeTable(in string) (string, bool) {
	var b strings.Builder
	b.Grow(len(in))
	var cells []string
	rows, same, width := 0, 0, 0
	for rest := in; rest != ""; {
		var l string
		l, rest, _ = strings.Cut(rest, "\n")
		if strings.TrimSpace(l) == "" || isSep(l) {
			continue
		}
		cells = splitPipe(cells[:0], l)
		if rows == 0 {
			width = len(cells)
		}
		if len(cells) == width {
			same++
		}
		rows++
		for i, c := range cells {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(c)
		}
		b.WriteByte('\n')
	}
	if rows < 4 || width < 2 || same*10 < rows*9 {
		return "", false
	}
	return b.String(), true
}

// splitPipe appends the trimmed cells of a row to cells: it splits on unescaped pipes (an escaped pipe stays in
// its cell) and drops the empty cells outside the outer pipes. The cells are slices of l, not copies.
func splitPipe(cells []string, l string) []string {
	t := strings.TrimSpace(l)
	t = strings.TrimPrefix(t, "|")
	if strings.HasSuffix(t, "|") && !strings.HasSuffix(t, `\|`) {
		t = t[:len(t)-1]
	}
	from := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '\\':
			i++
		case '|':
			cells = append(cells, strings.TrimSpace(t[from:i]))
			from = i + 1
		}
	}
	return append(cells, strings.TrimSpace(t[from:]))
}
