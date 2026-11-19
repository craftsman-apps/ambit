package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Builtin masks common identifiers with patterns, in this order. It needs no service, so it cannot
// be down; it knows only these kinds. Use Presidio for names, addresses and more languages.
type Builtin struct{}

var builtinRules = []struct {
	re    *regexp.Regexp
	find  func(text string) [][]int // instead of re
	label string
	// pick returns the spans of a match to mask (none: leave it). Without pick the whole match is masked.
	pick func(text string, loc []int) [][2]int
}{
	// Korean resident registration number (7th digit 5 to 8: foreign residents). With a dash it is masked as it is;
	// with a space or no separator only when its date is valid and a label such as RRN stands just before it.
	{re: regexp.MustCompile(`\b\d{6}[- ]?[1-8]\d{6}\b`), label: "<NATIONAL_ID>", pick: rrn},
	// US SSN. With dashes as it is; with spaces or none only when it is a valid SSN and labelled.
	{re: regexp.MustCompile(`\b\d{3}[- ]?\d{2}[- ]?\d{4}\b`), label: "<NATIONAL_ID>", pick: ssn},
	// Card numbers: every run of digit groups, then the card numbers inside it (see cards).
	{find: digitRuns, label: "<CARD_NUMBER>", pick: cards},
	{re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+(?:@|%40)[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), label: "<EMAIL>"},
}

// labelBefore is the number of bytes before a number that rrn and ssn search for a label.
const labelBefore = 40

// labelled reports whether one of labels (lower case, without spaces, dashes or underscores) is in the
// labelBefore bytes before text[at:].
func labelled(text string, at int, labels ...string) bool {
	near := strings.ToLower(text[max(0, at-labelBefore):at])
	near = strings.NewReplacer(" ", "", "-", "", "_", "").Replace(near)
	return slices.ContainsFunc(labels, func(l string) bool { return strings.Contains(near, l) })
}

func whole(loc []int) [][2]int { return [][2]int{{loc[0], loc[1]}} }

func rrn(text string, loc []int) [][2]int {
	s := text[loc[0]:loc[1]]
	if s[6] == '-' {
		return whole(loc)
	}
	mm, dd := atoi(s[2:4]), atoi(s[4:6])
	if mm >= 1 && mm <= 12 && dd >= 1 && dd <= daysIn[mm-1] && labelled(text, loc[0], "rrn", "resident", "registration", "주민", "외국인", "등록번호") {
		return whole(loc)
	}
	return nil
}

// daysIn is the number of days of each month (February 29: the century is not checked).
var daysIn = [12]int{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func ssn(text string, loc []int) [][2]int {
	s := text[loc[0]:loc[1]]
	switch {
	case len(s) == 10 || len(s) == 11 && s[3] != s[6]: // one kind of separator, between all three parts
		return nil
	case len(s) == 11 && s[3] == '-':
		return whole(loc)
	}
	digits := strings.ReplaceAll(s, " ", "")
	area, group, serial := atoi(digits[:3]), atoi(digits[3:5]), atoi(digits[5:])
	// The SSA never issues area 000, 666 or 900-999, group 00 or serial 0000.
	if area == 0 || area == 666 || area >= 900 || group == 0 || serial == 0 || !labelled(text, loc[0], "ssn", "socialsecurity") {
		return nil
	}
	return whole(loc)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// digit returns the value of the ASCII or full-width digit at text[i:] and its length in bytes (0 if none).
func digit(text string, i int) (int, int) {
	if i >= len(text) {
		return -1, 0
	}
	if c := text[i]; c >= '0' && c <= '9' {
		return int(c - '0'), 1
	}
	if i+2 < len(text) && text[i] == 0xEF && text[i+1] == 0xBC && text[i+2] >= 0x90 && text[i+2] <= 0x99 { // ０-９
		return int(text[i+2] - 0x90), 3
	}
	return -1, 0
}

// digitRuns returns the runs of digit groups (ASCII or full-width digits; between groups a run of spaces, one "-"
// or one ".") of at least 13 bytes: the places where a card number can be. A loop, not a regular expression: it
// sees every number of a result, and a regular expression made masking a fifth slower.
func digitRuns(text string) (locs [][]int) {
	for i := 0; i < len(text); {
		if _, n := digit(text, i); n == 0 {
			i++
			continue
		}
		start, end := i, i
		for {
			for _, n := digit(text, i); n > 0; _, n = digit(text, i) {
				i += n
			}
			end = i
			j := i
			for j < len(text) && text[j] == ' ' {
				j++
			}
			if j == i && j < len(text) && (text[j] == '-' || text[j] == '.') {
				j++
			}
			if _, n := digit(text, j); j == i || n == 0 {
				break
			}
			i = j
		}
		if end-start >= 13 {
			locs = append(locs, []int{start, end})
		}
		i = end
	}
	return locs
}

type digitGroup struct {
	start, end int    // bytes
	digits     []int  // values
	sep        string // the separator before it: "" for the first group, " " for any run of spaces, "-" or "."
}

// cards returns the card numbers in a run of digit groups such as "2026-11-17 4111 1111 1111 1111": every part
// of the run that starts and ends at a group, has 13 to 19 digits, passes the Luhn check, starts with 2 to 6 (the
// banking and travel card ranges; Unix times in milliseconds start with 1), and is either one group or written like
// a card (one kind of separator, a first group of 4 digits, then groups of 3 to 6 and a last one of 2 to 6).
// Overlapping finds are joined, so a short number next to a card is masked with it rather than the card left.
func cards(text string, loc []int) (spans [][2]int) {
	if loc[1]-loc[0] < 13 {
		return nil
	}
	var groups []digitGroup
	for i, sep := loc[0], ""; i < loc[1]; {
		g := digitGroup{start: i, sep: sep}
		for i < loc[1] {
			d, n := digit(text, i)
			if n == 0 {
				break
			}
			g.digits, i = append(g.digits, d), i+n
		}
		g.end = i
		groups = append(groups, g)
		for ; i < loc[1]; i++ {
			if _, n := digit(text, i); n > 0 {
				break
			}
			sep = text[i : i+1]
		}
	}
	// Like \b: a group glued to a letter, such as "no4111", is not part of a card number.
	if loc[0] > 0 && isWordByte(text[loc[0]-1]) {
		groups = groups[1:]
	}
	if loc[1] < len(text) && isWordByte(text[loc[1]]) && len(groups) > 0 {
		groups = groups[:len(groups)-1]
	}
	for i := range groups {
		n := 0
		for j := i; j < len(groups); j++ {
			if j > i+1 && groups[j].sep != groups[i+1].sep {
				break
			}
			if n += len(groups[j].digits); n > 19 {
				break
			}
			if n < 13 || !isCard(groups[i:j+1]) {
				continue
			}
			if k := len(spans) - 1; k >= 0 && groups[i].start < spans[k][1] {
				spans[k][1] = max(spans[k][1], groups[j].end)
			} else {
				spans = append(spans, [2]int{groups[i].start, groups[j].end})
			}
		}
	}
	return spans
}

func isCard(groups []digitGroup) bool {
	if len(groups) > 1 {
		if len(groups[0].digits) != 4 {
			return false
		}
		for k, g := range groups[1:] {
			lo := 3
			if k == len(groups)-2 { // the last group
				lo = 2
			}
			if len(g.digits) < lo || len(g.digits) > 6 {
				return false
			}
		}
	}
	var all []int
	for _, g := range groups {
		all = append(all, g.digits...)
	}
	return all[0] >= 2 && all[0] <= 6 && luhn(all)
}

func luhn(digits []int) bool {
	sum := 0
	for i := range digits {
		d := digits[len(digits)-1-i]
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func (Builtin) Mask(_ context.Context, text string) (string, int, error) {
	n := 0
	for _, r := range builtinRules {
		// One output copy per rule that finds something (ReplaceAllStringFunc grows its buffer by doubling and
		// copies it again), and none for a rule that finds nothing.
		var b strings.Builder
		last := 0
		find := r.find
		if find == nil {
			find = func(text string) [][]int { return r.re.FindAllStringIndex(text, -1) }
		}
		for _, loc := range find(text) {
			spans := [][2]int{{loc[0], loc[1]}}
			if r.pick != nil {
				spans = r.pick(text, loc)
			}
			for _, s := range spans {
				if b.Len() == 0 {
					b.Grow(len(text))
				}
				b.WriteString(text[last:s[0]])
				b.WriteString(r.label)
				last = s[1]
				n++
			}
		}
		if last > 0 || b.Len() > 0 {
			b.WriteString(text[last:])
			text = b.String()
		}
	}
	return text, n, nil
}

// Presidio masks with Microsoft Presidio's analyzer service (MIT). It sends the strings of a result
// together, in pieces, and replaces the findings itself; the anonymizer service is not used. Any error is
// returned, and the guard then withholds the result (fail closed).
type Presidio struct {
	AnalyzerURL string
	// AnonymizerURL is accepted so that existing settings keep working. It is not used.
	AnonymizerURL string
	Language      string
	// Timeout is the deadline for all analyzer calls of one result. Default 10 s.
	Timeout time.Duration
	HTTP    *http.Client
}

const (
	presidioPieceChars   = 100_000 // the analyzer's spaCy limit is 1,000,000; stay well below it
	presidioOverlapChars = 200     // a finding up to this long is whole in at least one piece
	presidioParallel     = 4
	presidioSeparator    = "\n\n"
)

type finding struct {
	Type  string `json:"entity_type"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

func (p *Presidio) analyze(ctx context.Context, text string) ([]finding, error) {
	lang := p.Language
	if lang == "" {
		lang = "en"
	}
	b, _ := json.Marshal(map[string]any{"text": text, "language": lang})
	url := strings.TrimSuffix(p.AnalyzerURL, "/") + "/analyze"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := p.HTTP
	if c == nil {
		c = http.DefaultClient // the deadline is the context's
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presidio %s: HTTP %d", url, resp.StatusCode)
	}
	var out []finding
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// BuiltinThenPresidio runs the built-in masks on every text first, then Presidio on the masked texts, so
// Presidio adds to the built-in masks. The found count is the sum.
type BuiltinThenPresidio struct{ Presidio *Presidio }

func (b BuiltinThenPresidio) MaskAll(ctx context.Context, texts []string) ([]string, int, error) {
	pre, total := make([]string, len(texts)), 0
	for i, t := range texts {
		m, n, _ := Builtin{}.Mask(ctx, t) // never fails
		pre[i], total = m, total+n
	}
	out, n, err := b.Presidio.MaskAll(ctx, pre)
	if err != nil {
		return nil, 0, err
	}
	return out, total + n, nil
}

func (b BuiltinThenPresidio) Mask(ctx context.Context, text string) (string, int, error) {
	out, n, err := b.MaskAll(ctx, []string{text})
	if err != nil {
		return "", 0, err
	}
	return out[0], n, nil
}

// placeholders returns the [start, end) character spans of the built-in masks (<NATIONAL_ID>, <CARD_NUMBER>,
// <EMAIL>) in text, in order. Other text in angle brackets is not one: Presidio must still check it.
func placeholders(text []rune) (spans [][2]int) {
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		for _, r := range builtinRules {
			if l := []rune(r.label); i+len(l) <= len(text) && slices.Equal(text[i:i+len(l)], l) {
				spans = append(spans, [2]int{i, i + len(l)})
				i += len(l) - 1
				break
			}
		}
	}
	return spans
}

func (p *Presidio) Mask(ctx context.Context, text string) (string, int, error) {
	out, n, err := p.MaskAll(ctx, []string{text})
	if err != nil {
		return "", 0, err
	}
	return out[0], n, nil
}

// MaskAll masks all texts with a few analyzer calls: the texts are joined, cut into pieces of at most
// presidioPieceChars characters that overlap, and analyzed up to presidioParallel at a time under one
// deadline. Offsets count characters (runes), as the analyzer does.
func (p *Presidio) MaskAll(ctx context.Context, texts []string) ([]string, int, error) {
	var all []rune
	starts, ends := make([]int, len(texts)), make([]int, len(texts))
	nonEmpty := false
	for i, t := range texts {
		if i > 0 {
			all = append(all, []rune(presidioSeparator)...)
		}
		starts[i] = len(all)
		all = append(all, []rune(t)...)
		ends[i] = len(all)
		nonEmpty = nonEmpty || strings.TrimSpace(t) != ""
	}
	if !nonEmpty {
		return texts, 0, nil
	}
	d := p.Timeout
	if d == 0 {
		d = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var (
		mu    sync.Mutex
		found []finding
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(presidioParallel)
	for from := 0; ; from += presidioPieceChars - presidioOverlapChars {
		to := min(from+presidioPieceChars, len(all))
		g.Go(func() error {
			fs, err := p.analyze(gctx, string(all[from:to]))
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			for _, f := range fs {
				if f.Start < 0 || f.End < f.Start || f.End > to-from {
					return fmt.Errorf("presidio returned offsets %d-%d for a text of %d characters", f.Start, f.End, to-from)
				}
				f.Start, f.End = from+f.Start, from+f.End
				found = append(found, f)
			}
			return nil
		})
		if to == len(all) {
			break
		}
	}
	if err := g.Wait(); err != nil {
		return nil, 0, err
	}
	// Merge findings that overlap (the same one seen in two pieces, or a cut one and its whole copy).
	sort.Slice(found, func(i, j int) bool {
		if found[i].Start != found[j].Start {
			return found[i].Start < found[j].Start
		}
		return found[i].End > found[j].End
	})
	var merged []finding
	for _, f := range found {
		if n := len(merged); n > 0 && f.Start < merged[n-1].End {
			merged[n-1].End = max(merged[n-1].End, f.End)
			continue
		}
		merged = append(merged, f)
	}
	// Leave alone what lies wholly inside a mask placed by the built-in masker, such as <NATIONAL_ID>.
	spans := placeholders(all)
	merged = slices.DeleteFunc(merged, func(f finding) bool {
		i := sort.Search(len(spans), func(i int) bool { return spans[i][0] > f.Start }) - 1
		return i >= 0 && f.End <= spans[i][1]
	})
	// Replace inside each text only: a finding that reaches over a separator is cut at the text's edge.
	out, count := make([]string, len(texts)), 0
	for i, t := range texts {
		var b strings.Builder
		pos, hit := starts[i], false
		for _, f := range merged {
			s, e := max(f.Start, starts[i]), min(f.End, ends[i])
			if s >= e {
				continue
			}
			b.WriteString(string(all[pos:s]))
			b.WriteString("<" + f.Type + ">")
			pos, hit = e, true
			count++
		}
		if !hit {
			out[i] = t
			continue
		}
		b.WriteString(string(all[pos:ends[i]]))
		out[i] = b.String()
	}
	return out, count, nil
}
