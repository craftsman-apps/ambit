package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
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
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			if r.check != nil && !r.check(m) {
				return m
			}
			n++
			return r.label
		})
	}
	return text, n, nil
}

// Presidio masks with Microsoft Presidio's analyzer and anonymizer services (MIT). Any error is
// returned, and the guard then withholds the result (fail closed).
type Presidio struct {
	AnalyzerURL, AnonymizerURL string
	Language                   string
	HTTP                       *http.Client
}

func (p *Presidio) post(ctx context.Context, url string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("presidio %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Presidio) Mask(ctx context.Context, text string) (string, int, error) {
	if strings.TrimSpace(text) == "" {
		return text, 0, nil
	}
	lang := p.Language
	if lang == "" {
		lang = "en"
	}
	var found []json.RawMessage
	if err := p.post(ctx, strings.TrimSuffix(p.AnalyzerURL, "/")+"/analyze", map[string]any{"text": text, "language": lang}, &found); err != nil {
		return "", 0, err
	}
	if len(found) == 0 {
		return text, 0, nil
	}
	var out struct {
		Text string `json:"text"`
	}
	err := p.post(ctx, strings.TrimSuffix(p.AnonymizerURL, "/")+"/anonymize", map[string]any{"text": text, "analyzer_results": found}, &out)
	if err != nil {
		return "", 0, err
	}
	return out.Text, len(found), nil
}
