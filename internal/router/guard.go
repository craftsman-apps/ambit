package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Guard is the prompt guard (llm.promptGuard) as ambit-router applies it to the text it sends to the classifier. The
// gateway runs its own guard only when it calls the chosen model, after ambit-router has labelled the request
// (agentgateway v1.7.0: route ExtProc in apply_request_policies, prompt guard in the LLM backend call), so without
// this a prompt the guard refuses or masks would reach the classifier model unchanged.
//
// The rules are agentgateway's: the patterns first, then the built-ins, then the webhook. Every check here is at
// least as strict as the gateway's: a prompt the gateway refuses is never classified, and a masked text never holds
// what the gateway masks. PhoneNumber is a superset (see phoneRanges), so the router may refuse to classify a prompt
// the gateway lets through: that request gets no label (the default model).
type Guard struct {
	Mode     string   `json:"mode"` // reject or mask; audit is not loaded (the classifier gets the text, as the model does)
	Builtins []string `json:"builtins"`
	Patterns []string `json:"patterns"`
	Webhook  *struct {
		URL         string `json:"url"`
		FailureMode string `json:"failureMode"` // FailClosed or FailOpen
	} `json:"webhook"`

	res  []*regexp.Regexp
	HTTP *http.Client
}

// errGuard: the guard refuses the text, so the classifier is not called (reason "guard").
var errGuard = errors.New("refused by the prompt guard")

// LoadGuard reads the AMBIT_PROMPT_GUARD value the chart renders. Empty, or mode audit: nil (no checks).
func LoadGuard(s string, hc *http.Client) (*Guard, error) {
	if s == "" {
		return nil, nil
	}
	var g Guard
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		return nil, fmt.Errorf("AMBIT_PROMPT_GUARD: %w", err)
	}
	switch g.Mode {
	case "audit":
		return nil, nil
	case "reject", "mask":
	default:
		return nil, fmt.Errorf("AMBIT_PROMPT_GUARD: mode %q is not audit, reject or mask", g.Mode)
	}
	for _, b := range g.Builtins {
		if _, ok := builtins[b]; !ok {
			return nil, fmt.Errorf("AMBIT_PROMPT_GUARD: unknown built-in %q", b)
		}
	}
	// ponytail: Go's \d \w \s \b are ASCII, Rust's are Unicode: a pattern can match non-ASCII text in the gateway and
	// not here. The chart checks patterns with Go's parser too (#319); a Rust-compatible matcher if that matters.
	for _, p := range g.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("AMBIT_PROMPT_GUARD: pattern %q: %w", p, err)
		}
		g.res = append(g.res, re)
	}
	g.HTTP = hc
	return &g, nil
}

type span struct{ start, end int }

// builtin is one of agentgateway's PII recognizers (crates/agentgateway/src/llm/policy/pii, v1.7.0).
type builtin struct {
	entity string
	res    []*regexp.Regexp
	luhn   bool
}

var builtins = map[string]builtin{
	"CreditCard": {entity: "CREDIT_CARD", luhn: true, res: mustAll(
		`\b4\d{3}[- ]?(\d{3,4})[- ]?(\d{3,4})[- ]?(\d{3,5})\b`,
		`\b5[0-5]\d{2}[- ]?(\d{3,4})[- ]?(\d{3,4})[- ]?(\d{3,5})\b`,
		`\b6\d{3}[- ]?(\d{3,4})[- ]?(\d{3,4})[- ]?(\d{3,5})\b`,
		`\b3\d{3}[- ]?(\d{3,4})[- ]?(\d{3,4})[- ]?(\d{3,5})\b`,
		`\b1\d{3}[- ]?(\d{3,4})[- ]?(\d{3,4})[- ]?(\d{4,5})\b`)},
	"Ssn": {entity: "SSN", res: mustAll(
		`\b([0-9]{5})-([0-9]{4})\b`, `\b([0-9]{3})-([0-9]{6})\b`, `\b(([0-9]{3})-([0-9]{2})-([0-9]{4}))\b`,
		`\b[0-9]{9}\b`, `\b([0-9]{3})[- .]([0-9]{2})[- .]([0-9]{4})\b`)},
	"Email":       {entity: "EMAIL_ADDRESS", res: mustAll(`[a-zA-Z0-9_.+-]+@[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)+`)},
	"CaSin":       {entity: "CA_SIN", res: mustAll(`\b([0-9]{3})[- ]([0-9]{3})[- ]([0-9]{3})\b`, `\b[0-9]{9}\b`)},
	"PhoneNumber": {entity: "PHONE_NUMBER"},
}

func mustAll(ps ...string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range ps {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

func (b builtin) find(text string) []span {
	if b.entity == "PHONE_NUMBER" {
		return phoneRanges(text)
	}
	var out []span
	for _, re := range b.res {
		for _, m := range re.FindAllStringIndex(text, -1) {
			if !b.luhn || luhn(text[m[0]:m[1]]) {
				out = append(out, span{m[0], m[1]})
			}
		}
	}
	return out
}

func luhn(s string) bool {
	sum, digits, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == '-' || c == ' ' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		digits++
		double = !double
	}
	return digits > 0 && sum%10 == 0
}

// The gateway's phone recognizer: candidate runs, split into words; a span of at most 6 words and 32 bytes with 7 to
// 15 digits is a phone number when libphonenumber accepts it for one of seven regions. ambit-router has no
// libphonenumber: every such span counts, so it finds every number the gateway finds, and more.
var (
	phoneRun  = regexp.MustCompile(`(?i)(^|[^0-9])([+()]?[0-9][0-9\t\p{Zs}().\-+]{6,255})`)
	phoneWord = regexp.MustCompile(`[^\s\v\x{85}\p{Z}]+`) // Rust's \S+: not Unicode white space
)

func phoneRanges(text string) []span {
	var out []span
	for _, m := range phoneRun.FindAllStringSubmatchIndex(text, -1) {
		run := text[m[4]:m[5]]
		words := phoneWord.FindAllStringIndex(run, -1)
		for i := range words {
			for j := i; j < len(words) && j < i+6; j++ {
				s := run[words[i][0]:words[j][1]]
				if len(s) > 32 {
					break
				}
				n := 0
				for _, c := range []byte(s) {
					if c >= '0' && c <= '9' {
						n++
					}
				}
				if n > 15 {
					break
				}
				if n < 7 {
					continue
				}
				s = strings.TrimRightFunc(s, func(r rune) bool { return r < '0' || r > '9' })
				out = append(out, span{m[4] + words[i][0], m[4] + words[i][0] + len(s)})
			}
		}
	}
	return out
}

// replace puts with in place of each span; overlapping spans become one.
func replace(text string, spans []span, with string) string {
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var b strings.Builder
	pos := 0
	for i := 0; i < len(spans); {
		s := spans[i]
		for i++; i < len(spans) && spans[i].start < s.end; i++ {
			s.end = max(s.end, spans[i].end)
		}
		b.WriteString(text[pos:s.start])
		b.WriteString(with)
		pos = s.end
	}
	b.WriteString(text[pos:])
	return b.String()
}

// Msg is one message as the guard reads it: the role, and the content as text (a string, or the text parts).
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func msgsOf(ms []message) []Msg {
	out := make([]Msg, 0, len(ms))
	for _, m := range ms {
		var s string
		if json.Unmarshal(m.Content, &s) != nil {
			var parts []struct{ Type, Text string }
			_ = json.Unmarshal(m.Content, &parts)
			var b []string
			for _, p := range parts {
				if p.Type == "text" {
					b = append(b, p.Text)
				}
			}
			s = strings.Join(b, "\n")
		}
		out = append(out, Msg{m.Role, s})
	}
	return out
}

// lastUser is the content of the last user message ("" when there is none).
func lastUser(ms []Msg) string {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == "user" {
			return ms[i].Content
		}
	}
	return ""
}

// Regex applies the patterns and built-ins to every message, as the gateway does: the messages (masked in mask
// mode), or errGuard when one matches in reject mode, so a request the gateway refuses is never classified.
func (g *Guard) Regex(ms []Msg) ([]Msg, error) {
	out := make([]Msg, len(ms))
	for i, m := range ms {
		t, err := g.regex(m.Content)
		if err != nil {
			return nil, err
		}
		out[i] = Msg{m.Role, t}
	}
	return out, nil
}

func (g *Guard) regex(text string) (string, error) {
	reject := g.Mode == "reject"
	for _, re := range g.res {
		var spans []span
		for _, m := range re.FindAllStringIndex(text, -1) {
			if m[1] > m[0] { // like the gateway: an empty match masks nothing
				spans = append(spans, span{m[0], m[1]})
			}
		}
		if len(spans) == 0 {
			continue
		}
		if reject {
			return "", errGuard
		}
		text = replace(text, spans, "<masked>")
	}
	for _, name := range g.Builtins {
		b := builtins[name]
		spans := b.find(text)
		if len(spans) == 0 {
			continue
		}
		if reject {
			return "", errGuard
		}
		text = replace(text, spans, "<"+b.entity+">")
	}
	return text, nil
}

// Hook asks the webhook (agentgateway's guardrail protocol, POST /request) about the messages, as the gateway does
// in reject and mask mode: it passes, masks or refuses. It returns the text for the classifier, the last user
// message. A failed call follows failureMode.
func (g *Guard) Hook(ctx context.Context, ms []Msg) (string, error) {
	if g.Webhook == nil || g.Webhook.URL == "" {
		return lastUser(ms), nil
	}
	out, err := g.hook(ctx, ms)
	if err != nil && !errors.Is(err, errGuard) {
		if g.Webhook.FailureMode == "FailOpen" {
			return lastUser(ms), nil
		}
		return "", fmt.Errorf("%w (webhook failed, FailClosed: %v)", errGuard, err)
	}
	return out, err
}

func (g *Guard) hook(ctx context.Context, ms []Msg) (string, error) {
	body, _ := json.Marshal(map[string]any{"body": map[string]any{"messages": ms}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Webhook.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	var a struct {
		Action map[string]json.RawMessage `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil || a.Action == nil {
		return "", errors.New("webhook answer is not a guardrail response")
	}
	if _, ok := a.Action["status_code"]; ok {
		return "", errGuard // reject
	}
	raw, ok := a.Action["body"]
	if !ok {
		return lastUser(ms), nil // pass
	}
	// mask: like the gateway, every message needs a role and a string content, or the answer is an error
	var m struct {
		Messages []struct {
			Role    *string `json:"role"`
			Content *string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Messages == nil {
		return "", errors.New("webhook mask answer has no messages")
	}
	out := make([]Msg, 0, len(m.Messages))
	for _, x := range m.Messages {
		if x.Role == nil || x.Content == nil {
			return "", errors.New("webhook mask answer has a message without role or content")
		}
		out = append(out, Msg{*x.Role, *x.Content})
	}
	return lastUser(out), nil
}
