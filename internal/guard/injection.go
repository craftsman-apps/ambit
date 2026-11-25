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
	// must, when set, must also match the text that re matched: re finds the place (a comment), must says what is in it.
	must *regexp.Regexp
	// needles are lower-case words of which a text must hold one before re runs, and re runs only on the text around a hit.
	// A regular expression is slow in Go (it reads every byte, with a large cost per byte for (?i)), and almost no result
	// holds these words, so this keeps the cost of the guard small (see scan). nil: re runs on the whole text (your own
	// patterns).
	needles []string
	// partners are words of which one must be near (within gap bytes of) a needle for re to run on the text between them:
	// every match of re holds a needle and a partner. nil: a needle is enough.
	partners  []string
	gap       int
	back, fwd int  // bytes that re reads before the first and after the last word of a match (0: windowBack and windowFwd)
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
	rl("ignore-previous-instructions", []string{"ignore", "forget", "override", "bypass", "discard", "disregard"},
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
	rl("hide-from-user", []string{"user", "human", "person"},
		`(?i)\b(?:(?:do\s+not|don['’]t|never)\s+(?:tell|inform|mention|reveal|notify|alert|show)|without\s+(?:telling|informing|notifying|alerting))\s+(?:this\s+to\s+)?(?:(?:the|your|this)\s+(?:user|human|person)\b|(?:user|human|person)\s+(?:about|that|of|anything|know)\b)`+
			`|(?i)\b(?:hide|keep)\s+(?:this|it|that)\s+(?:a\s+secret\s+)?from\s+the\s+user\b`).near(100, "tell", "inform", "mention", "reveal", "notify", "alert", "show", "hide", "keep"),
	rl("persona-override", []string{"you are now", "dan mode", "developer mode", "god mode", "jailbreak mode"},
		`(?i)\byou\s+are\s+now\s+(?:in\s+)?(?:(?:a|an|the)\s+)?(?:DAN\b|developer\s+mode|jailbroken|unrestricted|unfiltered)|\b(?:enable|enter|activate|switch\s+to)\s+(?:DAN|developer|god|jailbreak)\s+mode\b`),
	rl("addressed-to-ai", []string{"important to", "important for", "attention to", "attention for", "note to", "note for", "message to", "message for", "instruction to", "instructions to", "instruction for", "instructions for"},
		`(?i)\b(?:important|attention|note|message|instructions?)\s+(?:to|for)\s+(?:the\s+|any\s+|all\s+)?(?:ai|assistants?|llms?|language\s+models?|agents?|models?)\b`).near(100, " ai", "assistant", "llm", "language model", "agent", "model"),
	// A comment with an instruction in it. re stops at the first "-->", so a comment is read alone.
	{name: "html-comment-instruction", needles: []string{"<!--"}, gap: 3300, back: 8, fwd: 3300, // 800 characters are up to 3,200 bytes
		partners: []string{"ignore", "instruction", "system prompt", "assistant", "you must", "you should", "do not", "don't"},
		re:       regexp.MustCompile(`(?s)<!--.{0,800}?-->`),
		must:     regexp.MustCompile(`(?i)ignore|instruction|system prompt|assistant|you must|you should|do not|don't`)},
	{name: "tag-characters", re: regexp.MustCompile(`[\x{E0000}-\x{E007F}]+`), needles: []string{"\xf3\xa0\x80", "\xf3\xa0\x81"}, invisible: true},
	{name: "zero-width-run", re: regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{2060}\x{FEFF}]{2,}`),
		needles: []string{"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff"}, invisible: true},
}

// matches returns the places in t where the rule matches, as FindAllStringIndex does with the limit n. over is true when
// there may be more than n.
func (r *injRule) matches(t string, n int) (ms [][]int, over bool) {
	limit := n
	if n > 0 {
		limit = n + 1 // one more than the limit shows that there are more
	}
	all := r.re.FindAllStringIndex(t, limit)
	if n > 0 && len(all) > n {
		over, all = true, all[:n]
	}
	for _, m := range all {
		if m[1] > m[0] && (r.must == nil || r.must.MatchString(t[m[0]:m[1]])) { // an empty match (a word boundary) is no finding
			ms = append(ms, m)
		}
	}
	return ms, over
}

// hasMatch says if the rule has a match with text in t (an empty match is none), looking at the first matches only.
func (r *injRule) hasMatch(t string) bool {
	ms, _ := r.matches(t, 64)
	return len(ms) > 0
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
	// The words of the enabled rules and a scanner for them (all), and the same for the rules with invisible text only (hidden,
	// for StripHidden): a rule that is switched off costs nothing and cannot fill the list of words (maxHits), and the words of
	// visible text cannot hide the invisible ones. A set has no scanner when it has no word.
	all, hidden wordSet
	hookURL     string
	timeout     time.Duration
	failure     string
	HTTP        *http.Client
}

// wordSet is the needles and partners of some rules, with the rule of each word, and a scanner for them.
type wordSet struct {
	words   []string
	rule    []int
	partner []bool
	scanner *matcher
}

func newWordSet(enabled []bool, invisibleOnly bool) (w wordSet) {
	for i, r := range injectionRules {
		if !enabled[i] || invisibleOnly && !r.invisible {
			continue
		}
		for _, n := range r.needles {
			w.words, w.rule, w.partner = append(w.words, n), append(w.rule, i), append(w.partner, false)
		}
		for _, n := range r.partners {
			w.words, w.rule, w.partner = append(w.words, n), append(w.rule, i), append(w.partner, true)
		}
	}
	if w.words != nil {
		w.scanner = newMatcher(w.words)
	}
	return w
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
	in := &Injector{mode: c.Mode}
	// The webhook answers a POST with the text of a result: a redirect would send that text on to another address, or turn the
	// POST into a request whose answer the guard would read as a verdict. Redirects are not followed.
	client := http.Client{}
	if hc != nil {
		client = *hc
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	in.HTTP = &client
	for _, d := range c.DisabledBuiltins {
		if !slices.Contains(InjectionBuiltinNames(), d) {
			return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: unknown built-in rule %q", d)
		}
	}
	in.enabled = make([]bool, len(injectionRules))
	for i, r := range injectionRules {
		in.enabled[i] = c.Builtins && !slices.Contains(c.DisabledBuiltins, r.name)
	}
	in.all, in.hidden = newWordSet(in.enabled, false), newWordSet(in.enabled, true)
	for i, p := range c.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: pattern %d: %w", i+1, err)
		}
		if re.MatchString("") { // it would match everywhere, and an empty match is never a finding
			return nil, fmt.Errorf("AMBIT_INJECTION_GUARD: pattern %d matches the empty text", i+1)
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

// StripHidden removes hidden characters (the rules with invisible text) from texts, in mask mode, and returns the texts and
// the names of the rules that found some. It runs before masking: the hidden characters can split an email address or a rule's
// words in two, and removing them after the masker (or the other rules) had read the text would put the pieces together
// again. A text is cleaned again until nothing is left, because removing characters can put two runs together. In the other
// modes it changes nothing. over is true when the texts could not be cleaned (too many matches, or still hidden characters
// after 4 rounds): the result must be withheld.
func (in *Injector) StripHidden(texts []string) (out []string, rules []string, over bool) {
	if in.mode != InjectionMask {
		return texts, nil, false
	}
	seen := map[string]bool{}
	out = texts
	total := 0
	for i, t := range texts {
		for round := 0; ; round++ {
			spans, o := in.spans(t, seen, true)
			over = over || o
			if total += len(spans); len(spans) == 0 || total > maxSpansPerResult {
				over = over || total > maxSpansPerResult
				break
			}
			if round == 3 { // still hidden characters after removing them 4 times
				over = true
				break
			}
			t = replaceSpans(t, spans)
			if &out[0] == &texts[0] {
				out = slices.Clone(texts)
			}
			out[i] = t
		}
	}
	for _, r := range injectionRules {
		if seen[r.name] {
			rules = append(rules, r.name)
		}
	}
	return out, rules, over
}

// Check looks for injection text in texts, which are masked already. hidden are the rules that StripHidden found in the
// texts before masking. It never reads the matched text into a message or a log.
func (in *Injector) Check(ctx context.Context, texts []string, hidden []string) InjectionResult {
	res := InjectionResult{Texts: texts, Changed: len(hidden) > 0}
	seen := map[string]bool{}
	for _, h := range hidden {
		seen[h] = true
	}
	var out []string // copy of texts, made when a text changes
	tooMany := false
	total := 0
	for i, t := range texts {
		spans, over := in.spans(t, seen, false)
		tooMany = tooMany || over
		if total += len(spans); total > maxSpansPerResult {
			tooMany = true // each replacement is longer than the text it replaces (or not), so a result of matches is never made
			break
		}
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
	if tooMany && in.mode != InjectionAudit {
		// Not every match is masked or even counted: the result is not made safe, so it is withheld.
		res.Action = ActionRejected
		res.Withhold = "has too many matches for the prompt-injection check to handle, so it was withheld"
		return res
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

const (
	windowBack = 64 // bytes read before and after the words of a rule: a match may start before its first word
	windowFwd  = 32
	// maxMatches is the most matches of one rule in one text that the guard keeps; with more, it does not try to mask (mask
	// and reject mode withhold the result). A text that is all matches would otherwise need more memory than the text.
	maxMatches = 10_000
	// maxSpansPerResult is the same for the whole result: the replacements of a result of 24 MiB that is all matches would be
	// 18 times as long as the matches.
	maxSpansPerResult = 100_000
	// maxWordWalk is how far a window moves to leave a word whole. A longer word is cut: no rule word is that long.
	maxWordWalk = 64
	// maxSpaceWalk is how far a window follows a run of white space (and then reads a margin more).
	maxSpaceWalk = 512
)

// near says if the text between two words is at most gap bytes, a run of white space counting as one byte (as the scanner reads
// it). Words that overlap are near.
func near(text string, from, to, gap int) bool {
	if to <= from {
		return true
	}
	if to-from > gap+maxSpaceWalk {
		return false
	}
	n, space := 0, false
	for i := from; i < to; i++ {
		if isSpaceByte(text[i]) {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		n++
	}
	return n <= gap
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// window is the part of text that the rule r reads for the words [ws, we): it starts and ends between words and at a
// character boundary, so it holds no half word or half character that could make a match that the whole text does not
// have. ok is false when no match can hold the words.
func window(text string, r *injRule, ws, we int) (lo, hi int, ok bool) {
	back, fwd := windowBack, windowFwd
	if r.back > 0 {
		back = r.back
	}
	if r.fwd > 0 {
		fwd = r.fwd
	}
	lo, hi = max(0, ws-back), min(len(text), we+fwd)
	// White space between the words of a match can be long (an instruction wrapped onto an indented line): when the window ends
	// in it, it goes on to the next word and a margin more, and the same before the first word.
	for n := 0; hi < len(text) && n < maxSpaceWalk && isSpaceByte(text[hi-1]) && isSpaceByte(text[hi]); n++ {
		hi++
	}
	if hi < len(text) && isSpaceByte(text[hi-1]) {
		hi = min(len(text), hi+fwd)
	}
	for n := 0; lo > 0 && lo < ws && n < maxSpaceWalk && isSpaceByte(text[lo]) && isSpaceByte(text[lo-1]); n++ {
		lo--
	}
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
		for n := 0; lo < ws && lo > 0 && n < maxWordWalk && (isWordByte(text[lo-1]) && isWordByte(text[lo]) || !utf8.RuneStart(text[lo])); n++ {
			lo++
		}
	}
	for n := 0; hi < len(text) && n < maxWordWalk && (isWordByte(text[hi-1]) && isWordByte(text[hi]) || !utf8.RuneStart(text[hi])); n++ {
		hi++
	}
	if !r.lineStart && lo == ws && lo > 0 && isWordByte(text[lo-1]) && isWordByte(text[lo]) {
		lo-- // the word begins inside a longer word: the window must show that (a \b is not there)
	}
	for lo < ws && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}
	return lo, hi, true
}

// spans finds the matches of every rule in text and adds the names of the rules that matched to seen. A built-in rule
// reads only the text around its words (one pass over the text finds all words); your own patterns read the whole text.
// Only in mask mode it returns the spans (the other modes need the names only, and stop at the first match of a rule);
// over is true when a rule had more than maxMatches matches in one place, or the text had more than maxHits words.
// hiddenOnly looks for the rules with invisible text only (StripHidden).
func (in *Injector) spans(text string, seen map[string]bool, hiddenOnly bool) (out []injSpan, over bool) {
	find := func(r *injRule, base int, t string) {
		if in.mode != InjectionMask && !hiddenOnly {
			// Audit and reject need the name of the rule, not where it matched: no list of spans, and nothing taken from the
			// budget of spans (maxSpansPerResult) that a later text needs.
			if r.re.MatchString(t) && r.hasMatch(t) {
				seen[r.name] = true
			}
			return
		}
		ms, more := r.matches(t, maxMatches)
		over = over || more
		for _, m := range ms {
			if m[1] > m[0] {
				seen[r.name] = true
				out = append(out, injSpan{base + m[0], base + m[1], r.invisible})
			}
		}
	}
	set := &in.all
	if hiddenOnly {
		set = &in.hidden
	}
	var hits []hit
	if set.scanner != nil {
		var overflow bool
		hits, overflow = set.scanner.scan(text)
		over = over || overflow
	}
	if hits != nil {
		type pos struct{ start, end int }
		wins := make([][][2]int, len(injectionRules))
		last := make([][2]pos, len(injectionRules)) // the last needle and the last partner of each rule
		seenWord := make([][2]bool, len(injectionRules))
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
			ri := set.rule[h.word]
			if hiddenOnly && !injectionRules[ri].invisible {
				continue
			}
			r := &injectionRules[ri]
			me := pos{h.start, h.end}
			if r.partners == nil {
				add(ri, me.start, me.end)
				continue
			}
			// Needle and partner: a window from the one to the other, when they are near.
			mine, other := 0, 1
			if set.partner[h.word] {
				mine, other = 1, 0
			}
			last[ri][mine], seenWord[ri][mine] = me, true
			if o := last[ri][other]; seenWord[ri][other] && near(text, o.end, me.start, r.gap) {
				add(ri, min(o.start, me.start), me.end)
			}
		}
		for ri, ws := range wins {
			for _, w := range ws {
				find(&injectionRules[ri], w[0], text[w[0]:w[1]])
			}
		}
	}
	if !hiddenOnly {
		for i := range in.custom {
			find(&in.custom[i], 0, text)
		}
	}
	return out, over
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
			if res.Action != "" {
				res.Action = ActionRejected // the client gets no masked result
			}
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
	// The whole answer, and only the answer: an answer that is cut at the size limit, or has anything after the first JSON
	// value, is not a guardrail response (it must not pass under FailClosed).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil || len(raw) > maxMessage {
		return "", nil, errors.New("webhook answer is too large or could not be read")
	}
	var a struct {
		Action map[string]json.RawMessage `json:"action"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Action == nil {
		return "", nil, errors.New("webhook answer is not a guardrail response")
	}
	if _, ok := a.Action["status_code"]; ok {
		return "reject", nil, nil
	}
	body2, ok := a.Action["body"]
	if !ok {
		return "pass", nil, nil
	}
	var m struct {
		Messages []struct {
			Content *string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body2, &m) != nil || len(m.Messages) != len(texts) {
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
