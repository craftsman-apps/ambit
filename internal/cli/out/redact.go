package out

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redactor replaces known secret values with [redacted]. Code that reads a secret (a key, a password, a token)
// adds it here before doing anything else with it.
type Redactor struct {
	mu      sync.Mutex
	secrets []string
}

// Patterns for secrets the CLI never registered, e.g. a token inside a library error.
var secretPattern = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]{8,}|sk-or-v1-[a-z0-9]{16,}`)

// Add registers a secret and the encoded forms it can take in library output (JSON string, URL query, URL path).
// Values shorter than 4 characters are ignored: replacing them would damage ordinary text.
func (r *Redactor) Add(secret string) {
	if len(secret) < 4 {
		return
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
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		if sub := secretPattern.FindStringSubmatch(m); sub[1] != "" {
			return sub[1] + "[redacted]"
		}
		return "[redacted]"
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
