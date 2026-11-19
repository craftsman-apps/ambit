package out

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Redactor replaces known secret values with [redacted]. Code that reads a secret (a key, a password, a token)
// adds it here before doing anything else with it.
type Redactor struct {
	mu      sync.Mutex
	secrets []string
}

// Patterns for secrets the CLI never registered, e.g. a token inside a library error, or an Ambit key that the
// person already had in plain text in a config file.
var secretPattern = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]{8,}|sk-or-v1-[a-z0-9]{16,}|ambit_[0-9A-Za-z]{43}`)

// pemPattern is a PEM private key block (a GitHub App key, a TLS key) in one string, e.g. inside a library error.
var pemPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// minLineLength is the shortest line of a multi-line secret that Add registers on its own. Shorter lines (an empty
// line, a short header) would damage ordinary text.
const minLineLength = 16

// holdBack is how many bytes at least the verbose reader keeps from one piece of a long line for the next, so that
// a token of the secret patterns that crosses the seam is matched whole.
const holdBack = 256

// Add registers a secret and the encoded forms it can take in library output (JSON string, URL query, URL path).
// Values shorter than 4 characters are ignored: replacing them would damage ordinary text.
func (r *Redactor) Add(secret string) {
	if len(secret) < 4 {
		return
	}
	// A library can print a multi-line secret (a PEM key) line by line: each line must be redacted on its own.
	if strings.ContainsAny(secret, "\r\n") {
		for _, l := range strings.FieldsFunc(secret, func(c rune) bool { return c == '\r' || c == '\n' }) {
			if len(l) >= minLineLength {
				r.Add(l)
			}
		}
	}
	forms := []string{secret, url.QueryEscape(secret), url.PathEscape(secret)}
	if b, err := json.Marshal(secret); err == nil {
		forms = append(forms, strings.Trim(string(b), `"`))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range forms {
		if !contains(r.secrets, f) {
			r.secrets = append(r.secrets, f)
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact returns s with every registered secret and every secret-looking token replaced.
func (r *Redactor) Redact(s string) string {
	r.mu.Lock()
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	r.mu.Unlock()
	s = pemPattern.ReplaceAllString(s, "[redacted]")
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		if sub := secretPattern.FindStringSubmatch(m); sub[1] != "" {
			return sub[1] + "[redacted]"
		}
		return "[redacted]"
	})
}

// SplitSafe splits s, a piece of a longer line, into a head that can be printed now (already redacted) and a
// raw rest to put in front of the next piece. The rest is the last bytes of s, at least as long as the longest
// registered secret minus one, and moved back so that no registered secret crosses the cut. So a secret that
// spans two pieces is still matched whole (#108).
func (r *Redactor) SplitSafe(s string) (head, rest string) {
	r.mu.Lock()
	keep := holdBack
	if len(r.secrets) > 0 && len(r.secrets[0]) > keep {
		keep = len(r.secrets[0]) // the list is sorted longest first
	}
	cut := len(s) - keep
	for moved := true; moved && cut > 0; {
		moved = false
		for _, v := range r.secrets {
			// An occurrence that starts before cut and ends after it starts at least len(v)-1 bytes before cut.
			from := cut - len(v) + 1
			if from < 0 {
				from = 0
			}
			if i := strings.Index(s[from:min(len(s), cut+len(v)-1)], v); i >= 0 && from+i < cut {
				cut, moved = from+i, true
			}
		}
	}
	r.mu.Unlock()
	if cut <= 0 {
		return "", s
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return r.Redact(s[:cut]), s[cut:]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
