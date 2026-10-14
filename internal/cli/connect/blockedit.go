package connect

import (
	"fmt"
	"strings"
)

// Marked blocks are how Ambit owns lines in TOML and shell files. A block is a begin line that starts with
// beginPrefix, its body, and an end line. Removing every block gives back the original file, byte for byte.
const (
	beginPrefix = "# >>> ambit"
	endMarker   = "# <<< ambit <<<"
	bom         = "\xef\xbb\xbf"
)

type line struct{ text, eol string }

// Text is a file as lines. Every line keeps its own terminator ("\n", "\r\n" or none for the last line), so
// editing some lines never changes the bytes of the others.
type Text struct {
	bom   bool
	lines []line
	eol   string // used for inserted lines: "\r\n" when the first line break is one
}

func ParseText(b []byte) *Text {
	s := string(b)
	t := &Text{eol: "\n"}
	if strings.HasPrefix(s, bom) {
		t.bom, s = true, s[len(bom):]
	}
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			t.lines = append(t.lines, line{s, ""})
			break
		}
		l := line{s[:i], "\n"}
		if strings.HasSuffix(l.text, "\r") {
			l = line{l.text[:len(l.text)-1], "\r\n"}
		}
		t.lines = append(t.lines, l)
		s = s[i+1:]
	}
	if len(t.lines) > 0 && t.lines[0].eol == "\r\n" {
		t.eol = "\r\n"
	}
	return t
}

func (t *Text) Bytes() []byte {
	var sb strings.Builder
	if t.bom {
		sb.WriteString(bom)
	}
	for _, l := range t.lines {
		sb.WriteString(l.text)
		sb.WriteString(l.eol)
	}
	return []byte(sb.String())
}

// BlockRecord says what an insert changed besides the block itself, so RemoveBlock can undo it.
type BlockRecord struct {
	AddedFinalNewline bool `json:"addedFinalNewline,omitempty"` // the line before the block had no line break; one was added
	Padded            bool `json:"padded,omitempty"`            // a blank line was added before the block
}

// Block is a begin line, its body and its end line. Start and End are the line indexes of the two markers.
type Block struct{ Start, End int }

// Blocks returns the blocks in order. An unterminated block, or an end marker without a begin marker, is an error:
// Ambit cannot tell which lines it owns.
func (t *Text) Blocks() ([]Block, error) {
	var out []Block
	open := -1
	for i, l := range t.lines {
		switch {
		case strings.HasPrefix(l.text, beginPrefix):
			if open >= 0 {
				return nil, fmt.Errorf("line %d: an Ambit block begins again before the one at line %d ends", i+1, open+1)
			}
			open = i
		case l.text == endMarker:
			if open < 0 {
				return nil, fmt.Errorf("line %d: an Ambit block ends here but none began", i+1)
			}
			out = append(out, Block{open, i})
			open = -1
		}
	}
	if open >= 0 {
		return nil, fmt.Errorf("line %d: an Ambit block begins here and never ends", open+1)
	}
	return out, nil
}

// BlockBody is the lines between the markers, joined with "\n" and without terminators.
func (t *Text) BlockBody(b Block) string {
	var s []string
	for _, l := range t.lines[b.Start+1 : b.End] {
		s = append(s, l.text)
	}
	return strings.Join(s, "\n")
}

// InsertBlock puts a block before line index at (len(lines) appends it).
func (t *Text) InsertBlock(at int, beginLine string, body []string) BlockRecord {
	return t.insert(at, beginLine, body, false)
}

// InsertBlockPadded is InsertBlock with a blank line before the block, unless the line before is blank already.
func (t *Text) InsertBlockPadded(at int, beginLine string, body []string) BlockRecord {
	return t.insert(at, beginLine, body, true)
}

func (t *Text) insert(at int, beginLine string, body []string, pad bool) BlockRecord {
	var rec BlockRecord
	if at == len(t.lines) && at > 0 && t.lines[at-1].eol == "" {
		t.lines[at-1].eol = t.eol
		rec.AddedFinalNewline = true
	}
	var add []line
	if pad && at > 0 && t.lines[at-1].text != "" {
		add = append(add, line{"", t.eol})
		rec.Padded = true
	}
	add = append(add, line{beginLine, t.eol})
	for _, b := range body {
		add = append(add, line{b, t.eol})
	}
	add = append(add, line{endMarker, t.eol})
	t.lines = append(t.lines[:at], append(add, t.lines[at:]...)...)
	return rec
}

// RemoveBlock deletes the block and undoes what InsertBlock recorded.
func (t *Text) RemoveBlock(b Block, rec BlockRecord) {
	from, to := b.Start, b.End+1
	if rec.Padded && from > 0 && t.lines[from-1].text == "" {
		from--
	}
	t.lines = append(t.lines[:from], t.lines[to:]...)
	if rec.AddedFinalNewline && from == len(t.lines) && from > 0 {
		t.lines[from-1].eol = ""
	}
}
