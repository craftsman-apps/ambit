package guard

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
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The injection guard (protection.injectionGuard, issue 82) looks for prompt-injection text in what a server sends back:
// the same texts the masker reads, after masking, so a webhook never receives personal data. It is a tripwire, not a
// detector: it finds only the wording its rules describe, and a new wording passes.

// Injection modes. audit records a finding and changes nothing; reject withholds the result; mask replaces each match.
const (
	InjectionAudit  = "audit"
	InjectionReject = "reject"
	InjectionMask   = "mask"
)

// Actions of an injection finding, as the record and the metric write them.
const (
	ActionFound    = "found"    // audit: seen, result unchanged
	ActionRejected = "rejected" // the result was withheld
	ActionMasked   = "masked"   // matches were replaced or removed
	ActionError    = "error"    // the webhook failed (metric only)
)

// InjectionPlaceholder replaces the text a rule matched in mask mode.
const InjectionPlaceholder = "<PROMPT_INJECTION>"

// RuleWebhook is the rule name of a finding made by the webhook.
const RuleWebhook = "webhook"

var injectionFindings = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_injection_total",
	Help: "Prompt-injection findings in tool results, by server and action (found, rejected, masked, error). One per rule and result."},
	[]string{"server", "action"})

// injRule is one rule of the injection guard.
type injRule struct {
	name string
	re   *regexp.Regexp
	// needles are lower-case words of which a text must hold one before re runs, and re runs only on the text around a hit.
	// A regular expression is slow in Go (it reads every byte, with a large cost per byte for (?i)), and almost no result
	// holds these words, so this keeps the cost of the guard small (see scan). nil: re runs on the whole text (your own
	// patterns).
	needles []string
	// partners are words of which one must be near (within gap bytes of) a needle for re to run on the text between them:
	// every match of re holds a needle and a partner. nil: a needle is enough.
	partners  []string
	gap       int
	lineStart bool // re starts with ^ (multi-line): its text starts at the start of a line
	invisible bool // a match is removed, not replaced by the placeholder (the text it hides is not text)
}

func rl(name string, needles []string, re string) injRule {
	return injRule{name: name, re: regexp.MustCompile(re), needles: needles}
}

// near requires one of partners within gap bytes of a needle (see injRule.partners).
func (r injRule) near(gap int, partners ...string) injRule {
	r.partners, r.gap = partners, gap
	return r
}

// injectionRules are the built-in rules, in the order they are checked. The names are part of the public API: the
// values protection.injectionGuard.disabledBuiltins and the audit records use them. Add a rule at the end; do not rename.
var injectionRules = []injRule{
	rl("ignore-previous-instructions", []string{"ignore", "forget", "override", "bypass", "discard"},
		`(?i)\b(?:ignore|forget|override|bypass|discard)\s+(?:(?:all|any|every|the|of|your|my|these|those)\s+){0,3}(?:previous|prior|above|earlier|preceding|former|original|existing|all|any|every|your|my)\s+(?:\w+\s+)?(?:instructions?|prompts?|rules|directions|guidelines|commands)\b`+
			`|(?i)\b(?:ignore|forget|disregard)\s+(?:everything|anything|all)\s+(?:above|before|prior|you\s+(?:were|have\s+been)\s+told)\b`).near(150, "instruction", "prompt", "rules", "directions", "guidelines", "commands", "above", "before", "prior", "told"),
	rl("disregard-instructions", []string{"disregard"},
		`(?i)\bdisregard\s+(?:(?:all|any|every|the|of|your|my|these|those|previous|prior|above|earlier|preceding|system|safety)\s+){1,5}(?:\w+\s+)?(?:instructions?|prompts?|rules|directions|guidelines|commands|messages|polic(?:y|ies))\b`).near(150, "instruction", "prompt", "rules", "directions", "guidelines", "commands", "messages", "polic"),
	rl("new-instructions", []string{"new instruction", "your new", "from now on"},
		`(?i)\b(?:your\s+new\s+(?:instructions?|task|orders)\s+(?:are|is)|new\s+instructions?\s*:|from\s+now\s+on,?\s+(?:you|your)\s+(?:must|will|are|should|shall|only|have\s+to))`),
	rl("reveal-system-prompt", []string{"system prompt", "system message", "system instruction", "developer message", "developer prompt", "developer instruction"},
		`(?i)\b(?:reveal|print|repeat|show|output|leak|display|tell\s+me)\s+(?:me\s+)?(?:your|the)\s+(?:(?:full|entire|exact|hidden|secret|initial|original)\s+)?(?:system|developer)\s+(?:prompt|message|instructions?)\b`).near(100, "reveal", "print", "repeat", "show", "output", "leak", "display", "tell me"),
	rl("chatml-marker", []string{"<|"},
		`(?i)<\|(?:im_start|im_end|system|user|assistant|endoftext|begin_of_text|start_header_id|end_header_id|eot_id)\|>`),
	rl("inst-marker", []string{"[inst]", "[/inst]", "<<sys>>", "<</sys>>", "[sys]", "[/sys]"},
		`(?i)\[/?INST\]|<</?SYS>>|\[/?SYS\]`),
	{name: "role-prefix", needles: []string{"system:", "system :", "assistant:", "assistant :"}, lineStart: true,
		re: regexp.MustCompile(`(?im)^[ \t]{0,8}(?:system|assistant)[ \t]?:`)},
	rl("hide-from-user", []string{"the user", "your user", "this user", "the human", "the person", "this from"},
		`(?i)\b(?:(?:do\s+not|don['’]t|never)\s+(?:tell|inform|mention|reveal|notify|alert|show)|without\s+(?:telling|informing|notifying|alerting))\s+(?:this\s+to\s+)?(?:the\s+|your\s+|this\s+)?(?:user|human|person)\b`+
			`|(?i)\b(?:hide|keep)\s+(?:this|it|that)\s+(?:a\s+secret\s+)?from\s+the\s+user\b`).near(100, "tell", "inform", "mention", "reveal", "notify", "alert", "show", "hide", "keep"),
	rl("persona-override", []string{"you are now", "dan mode", "developer mode", "god mode", "jailbreak mode"},
		`(?i)\byou\s+are\s+now\s+(?:in\s+)?(?:(?:a|an|the)\s+)?(?:DAN\b|developer\s+mode|jailbroken|unrestricted|unfiltered)|\b(?:enable|enter|activate|switch\s+to)\s+(?:DAN|developer|god|jailbreak)\s+mode\b`),
	rl("addressed-to-ai", []string{"important to", "important for", "attention to", "attention for", "note to", "note for", "message to", "message for", "instruction to", "instructions to", "instruction for", "instructions for"},
		`(?i)\b(?:important|attention|note|message|instructions?)\s+(?:to|for)\s+(?:the\s+|any\s+|all\s+)?(?:ai|assistants?|llms?|language\s+models?|agents?|models?)\b`).near(100, " ai", "assistant", "llm", "language model", "agent", "model"),
	{name: "html-comment-instruction", needles: []string{"<!--"}, gap: 900,
		partners: []string{"ignore", "instruction", "system prompt", "assistant", "you must", "you should", "do not", "don't"},
		re:       regexp.MustCompile(`(?is)<!--.{0,400}?(?:ignore|instruction|system prompt|assistant|you must|you should|do not|don't).{0,400}?-->`)},
	{name: "tag-characters", re: regexp.MustCompile(`[\x{E0000}-\x{E007F}]+`), needles: []string{"\xf3\xa0\x80", "\xf3\xa0\x81"}, invisible: true},
	{name: "zero-width-run", re: regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{2060}\x{FEFF}]{2,}`),
		needles: []string{"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff"}, invisible: true},
}

// InjectionBuiltinNames lists the names of the built-in rules (the chart's values schema lists the same names).
func InjectionBuiltinNames() []string {
	out := make([]string, len(injectionRules))
	for i, r := range injectionRules {
		out[i] = r.name
	}
	return out
}

// InjectionConfig is the AMBIT_INJECTION_GUARD value the chart renders from protection.injectionGuard.
type InjectionConfig struct {
	Mode             string   `json:"mode"`
	Builtins         bool     `json:"builtins"`
	DisabledBuiltins []string `json:"disabledBuiltins"`
	Patterns         []string `json:"patterns"`
	Webhook          *struct {
		URL         string `json:"url"`
		Timeout     string `json:"timeout"`     // a Go duration; empty: 2s
		FailureMode string `json:"failureMode"` // FailClosed or FailOpen
	} `json:"webhook"`
}

// Injector checks the texts of a result. It holds no state after LoadInjector.
type Injector struct {
	mode    string
	enabled []bool    // by index in injectionRules
	custom  []injRule // your own patterns
	hookURL string
	timeout time.Duration
	failure string
	HTTP    *http.Client
}

// LoadInjector reads the AMBIT_INJECTION_GUARD value. Empty: nil (no injection guard).
func LoadInjector(s string, hc *http.Client) (*Injector, error) {
	if s == "" {
		return nil, nil
	}
	var c InjectionConfig
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: %w", err)
	}
	switch c.Mode {
	case InjectionAudit, InjectionReject, InjectionMask:
	default:
		return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: mode %q is not audit, reject or mask", c.Mode)
	}
	in := &Injector{mode: c.Mode, HTTP: hc}
	for _, d := range c.DisabledBuiltins {
		if !slices.Contains(InjectionBuiltinNames(), d) {
			return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: unknown built-in rule %q", d)
		}
	}
	in.enabled = make([]bool, len(injectionRules))
	for i, r := range injectionRules {
		in.enabled[i] = c.Builtins && !slices.Contains(c.DisabledBuiltins, r.name)
	}
	for i, p := range c.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: pattern %d: %w", i+1, err)
		}
		in.custom = append(in.custom, injRule{name: fmt.Sprintf("pattern-%d", i+1), re: re})
	}
	if w := c.Webhook; w != nil && w.URL != "" {
		in.hookURL, in.timeout, in.failure = w.URL, 2*time.Second, w.FailureMode
		if w.Timeout != "" {
			d, err := time.ParseDuration(w.Timeout)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: webhook timeout %q is not a positive duration", w.Timeout)
			}
			in.timeout = d
		}
		if in.HTTP == nil {
			in.HTTP = &http.Client{}
		}
	}
	if !slices.Contains(in.enabled, true) && len(in.custom) == 0 && in.hookURL == "" {
		return nil, errors.New("AMBIT_INJECTION_GUARD: no rule, pattern or webhook: a guard with nothing to check")
	}
	return in, nil
}

// InjectionResult is what Check found in the texts of one result.
type InjectionResult struct {
	Texts   []string // the texts to use: changed only in mask mode
	Changed bool     // Texts differ from the texts given
	Rules   []string // the rules that matched, each once; RuleWebhook for the webhook
	Action  string   // "" (nothing found), ActionFound, ActionRejected or ActionMasked
	// Withhold, when set, is the reason to refuse the whole result: a rejection, or a webhook that failed with FailClosed.
	Withhold string
	// HookError is set when the webhook failed (it also decides Withhold); the guard counts it.
	HookError error
}

// Check looks for injection text in texts. It never reads the matched text into a message or a log.
func (in *Injector) Check(ctx context.Context, texts []string) InjectionResult {
	res := InjectionResult{Texts: texts}
	seen := map[string]bool{}
	var out []string // copy of texts, made when a text changes
	for i, t := range texts {
		spans := in.spans(t, seen)
		if len(spans) == 0 || in.mode != InjectionMask {
			continue
		}
		if out == nil {
			out = slices.Clone(texts)
		}
		out[i] = replaceSpans(t, spans)
	}
	if out != nil {
		res.Texts, res.Changed = out, true
	}
	for _, r := range injectionRules { // the rules in their own order, then the patterns
		if seen[r.name] {
			res.Rules = append(res.Rules, r.name)
		}
	}
	for _, r := range in.custom {
		if seen[r.name] {
			res.Rules = append(res.Rules, r.name)
		}
	}
	if len(res.Rules) > 0 {
		res.Action = in.action(res.Changed)
	}
	if in.mode == InjectionReject && len(res.Rules) > 0 {
		res.Withhold = "matched the prompt-injection check (" + strings.Join(res.Rules, ", ") + "), so it was withheld"
		return res
	}
	if in.hookURL != "" {
		in.callHook(ctx, &res)
	}
	return res
}

func (in *Injector) action(changed bool) string {
	switch {
	case in.mode == InjectionReject:
		return ActionRejected
	case in.mode == InjectionMask && changed:
		return ActionMasked
	}
	return ActionFound
}

type injSpan struct {
	start, end int
	remove     bool
}

// injectionWords are the needles and partners of all built-in rules; injectionWordRule is the rule of each word and
// injectionWordPartner says if it is a partner.
var injectionWords, injectionWordRule, injectionWordPartner = func() (words []string, rule []int, partner []bool) {
	for i, r := range injectionRules {
		for _, n := range r.needles {
			words, rule, partner = append(words, n), append(rule, i), append(partner, false)
		}
		for _, n := range r.partners {
			words, rule, partner = append(words, n), append(rule, i), append(partner, true)
		}
	}
	return
}()

var injectionScanner = newMatcher(injectionWords)

const (
	windowBack = 32 // bytes read before and after the words of a rule: a match may start before its first word
	windowFwd  = 32
)

// window is the part of text that the rule r reads for the words [ws, we): it starts and ends between words and at a
// character boundary, so it holds no half word or half character that could make a match that the whole text does not
// have. ok is false when no match can hold the words.
func window(text string, r *injRule, ws, we int) (lo, hi int, ok bool) {
	lo, hi = max(0, ws-windowBack), min(len(text), we+windowFwd)
	if r.lineStart { // the match starts at the start of a line, at most 8 blanks before the word
		from := max(0, ws-16)
		if nl := strings.LastIndexByte(text[from:ws], '\n'); nl >= 0 {
			lo = from + nl + 1
		} else if from > 0 {
			return 0, 0, false
		} else {
			lo = 0
		}
	} else {
		for lo < ws && lo > 0 && (isWordByte(text[lo-1]) && isWordByte(text[lo]) || !utf8.RuneStart(text[lo])) {
			lo++
		}
	}
	for hi < len(text) && (isWordByte(text[hi-1]) && isWordByte(text[hi]) || !utf8.RuneStart(text[hi])) {
		hi++
	}
	return lo, hi, true
}

// spans finds the matches of every rule in text and adds the names of the rules that matched to seen. A built-in rule
// reads only the text around its words (one pass over the text finds all words); your own patterns read the whole text.
func (in *Injector) spans(text string, seen map[string]bool) []injSpan {
	var out []injSpan
	find := func(r *injRule, base int, t string) {
		for _, m := range r.re.FindAllStringIndex(t, -1) {
			if m[1] > m[0] {
				seen[r.name] = true
				out = append(out, injSpan{base + m[0], base + m[1], r.invisible})
			}
		}
	}
	if hits := injectionScanner.scan(text); hits != nil {
		type pos struct{ start, end int }
		wins := make([][][2]int, len(injectionRules))
		lastNeedle, lastPartner := make([]*pos, len(injectionRules)), make([]*pos, len(injectionRules))
		add := func(ri, start, end int) {
			lo, hi, ok := window(text, &injectionRules[ri], start, end)
			if !ok {
				return
			}
			if ws := wins[ri]; len(ws) > 0 && lo <= ws[len(ws)-1][1] {
				ws[len(ws)-1][1] = max(ws[len(ws)-1][1], hi)
			} else {
				wins[ri] = append(ws, [2]int{lo, hi})
			}
		}
		for _, h := range hits {
			ri := injectionWordRule[h.word]
			if !in.enabled[ri] {
				continue
			}
			r := &injectionRules[ri]
			me := pos{h.end - len(injectionWords[h.word]), h.end}
			if r.partners == nil {
				add(ri, me.start, me.end)
				continue
			}
			// Needle and partner: a window from the one to the other, when they are near.
			mine, other := &lastNeedle[ri], &lastPartner[ri]
			if injectionWordPartner[h.word] {
				mine, other = other, mine
			}
			*mine = &me
			if o := *other; o != nil && me.start-o.end <= r.gap {
				add(ri, min(o.start, me.start), me.end)
			}
		}
		for ri, ws := range wins {
			for _, w := range ws {
				find(&injectionRules[ri], w[0], text[w[0]:w[1]])
			}
		}
	}
	for i := range in.custom {
		find(&in.custom[i], 0, text)
	}
	return out
}

// replaceSpans puts the placeholder in place of each span, and removes the spans of invisible text; overlapping spans
// become one, which is removed only if all of its parts are.
func replaceSpans(text string, spans []injSpan) string {
	slices.SortFunc(spans, func(a, b injSpan) int { return a.start - b.start })
	var b strings.Builder
	pos := 0
	for i := 0; i < len(spans); {
		s := spans[i]
		for i++; i < len(spans) && spans[i].start < s.end; i++ {
			s.end, s.remove = max(s.end, spans[i].end), s.remove && spans[i].remove
		}
		b.WriteString(text[pos:s.start])
		if !s.remove {
			b.WriteString(InjectionPlaceholder)
		}
		pos = s.end
	}
	b.WriteString(text[pos:])
	return b.String()
}

// callHook asks the webhook about the texts (the request and answer format of the prompt guard webhook) and adds its
// answer to res. In audit mode the answer is recorded and nothing else changes, also when the webhook fails.
func (in *Injector) callHook(ctx context.Context, res *InjectionResult) {
	ctx, cancel := context.WithTimeout(ctx, in.timeout)
	defer cancel()
	answer, masked, err := in.hook(ctx, res.Texts)
	if err != nil {
		res.HookError = err
		if in.mode != InjectionAudit && in.failure != "FailOpen" {
			res.Withhold = "could not be checked by the prompt-injection webhook (FailClosed), so it was withheld"
		}
		return
	}
	switch answer {
	case "reject":
		res.Rules = append(res.Rules, RuleWebhook)
		if in.mode == InjectionAudit {
			res.Action = ActionFound
			return
		}
		res.Action = ActionRejected
		res.Withhold = "matched the prompt-injection check (" + strings.Join(res.Rules, ", ") + "), so it was withheld"
	case "mask":
		res.Rules = append(res.Rules, RuleWebhook)
		if in.mode == InjectionAudit {
			if res.Action == "" {
				res.Action = ActionFound
			}
			return
		}
		res.Texts, res.Changed, res.Action = masked, true, ActionMasked
	}
}

// hook posts the texts as messages (role "tool") and reads agentgateway's guardrail answer: pass, mask (new messages) or
// reject (a status code). answer is "pass", "mask" or "reject".
func (in *Injector) hook(ctx context.Context, texts []string) (answer string, masked []string, err error) {
	msgs := make([]map[string]string, len(texts))
	for i, t := range texts {
		msgs[i] = map[string]string{"role": "tool", "content": t}
	}
	body, _ := json.Marshal(map[string]any{"body": map[string]any{"messages": msgs}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.hookURL, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := in.HTTP.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	var a struct {
		Action map[string]json.RawMessage `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(&a); err != nil || a.Action == nil {
		return "", nil, errors.New("webhook answer is not a guardrail response")
	}
	if _, ok := a.Action["status_code"]; ok {
		return "reject", nil, nil
	}
	raw, ok := a.Action["body"]
	if !ok {
		return "pass", nil, nil
	}
	var m struct {
		Messages []struct {
			Content *string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Messages) != len(texts) {
		return "", nil, errors.New("webhook mask answer does not hold one message for each text")
	}
	masked = make([]string, len(texts))
	for i, x := range m.Messages {
		if x.Content == nil {
			return "", nil, errors.New("webhook mask answer has a message without content")
		}
		masked[i] = *x.Content
	}
	return "mask", masked, nil
}
