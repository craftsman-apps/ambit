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
	label string
	check func(string) bool
}{
	{regexp.MustCompile(`\b\d{6}-[1-4]\d{6}\b`), "<NATIONAL_ID>", nil},  // Korean resident registration number
	{regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), "<NATIONAL_ID>", nil}, // US SSN
	{regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), "<CARD_NUMBER>", luhn},
	{regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), "<EMAIL>", nil},
}

func luhn(s string) bool {
	sum, alt, digits := 0, false, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum, alt, digits = sum+d, !alt, digits+1
	}
	return digits >= 13 && sum%10 == 0
}

func (Builtin) Mask(_ context.Context, text string) (string, int, error) {
	n := 0
	for _, r := range builtinRules {
		// One output copy per rule that finds something (ReplaceAllStringFunc grows its buffer by doubling and
		// copies it again), and none for a rule that finds nothing.
		var b strings.Builder
		last := 0
		for _, loc := range r.re.FindAllStringIndex(text, -1) {
			if r.check != nil && !r.check(text[loc[0]:loc[1]]) {
				continue
			}
			if b.Len() == 0 {
				b.Grow(len(text))
			}
			b.WriteString(text[last:loc[0]])
			b.WriteString(r.label)
			last = loc[1]
			n++
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
