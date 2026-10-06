// Package presets is the catalog of work apps an admin can add in a few steps: each entry is a vendor's
// own MCP server and the OAuth details to reach it. Only entries that passed a live check (verified) are
// shown in the console; see docs/presets.md.
package presets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

//go:embed presets.yaml
var catalog []byte

//go:embed testing.yaml
var testing_ []byte

type Placeholder struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Pattern string `json:"pattern"`
	Example string `json:"example"`
}

type Live struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Callback  string         `json:"callback,omitempty"` // "http" (default) or "https"
}

type Preset struct {
	ID                 string            `json:"id"`
	Version            int               `json:"version"`
	DisplayName        string            `json:"displayName"`
	Category           string            `json:"category"`
	Server             ServerRef         `json:"server"`
	AuthorizationURL   string            `json:"authorizationURL"`
	TokenURL           string            `json:"tokenURL"`
	RevocationURL      string            `json:"revocationURL,omitempty"`
	Resource           string            `json:"resource,omitempty"` // "default" | URL | "none"
	Scopes             []string          `json:"scopes,omitempty"`
	AuthorizeParams    map[string]string `json:"authorizeParams,omitempty"`
	TokenRequestFormat string            `json:"tokenRequestFormat,omitempty"`
	Placeholders       []Placeholder     `json:"placeholders,omitempty"`
	Setup              string            `json:"setup"`
	ClientIDLabel      string            `json:"clientIDLabel,omitempty"`
	ClientSecretLabel  string            `json:"clientSecretLabel,omitempty"`
	Requires           string            `json:"requires,omitempty"`
	Preview            bool              `json:"preview,omitempty"`
	Docs               string            `json:"docs"`
	Live               Live              `json:"live"`
	Contract           struct {
		Unauthenticated []int `json:"unauthenticated,omitempty"` // HTTP statuses the server may answer without a token; default [401]
	} `json:"contract,omitempty"`
	Verified   bool   `json:"verified,omitempty"`
	VerifiedOn string `json:"verifiedOn,omitempty"` // date of the passing live run; excluded from Hash like Verified
	Testing    bool   `json:"-"`                    // set only by LoadTesting: plain http to *.svc, *.localtest.me, localhost
}

type ServerRef struct {
	URL       string `json:"url"`
	Transport string `json:"transport"`
}

type Resolved struct {
	Connection v1.ConnectionSpec // without ClientID and ClientSecretRef
	Server     v1.ServerSpec     // without Connection
	Setup      string
}

const maxAuthorizeParams = 16 // same cap as the Connection CRD

var (
	reserved = []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "scope", "resource", "response_type", "response_mode", "request", "request_uri"}
	scopeRE  = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]+$`)
	holeRE   = regexp.MustCompile(`\{([a-zA-Z]+)\}`)
	nameRE   = regexp.MustCompile(`^[a-zA-Z]+$`)
	// A placeholder value is a plain label: letters, digits, dot, hyphen, underscore, starting and ending
	// with a letter or digit. This holds whatever the catalog pattern says, so no value can carry /, @, ?,
	// #, %, :, \ or whitespace, and ".." is refused below. It can never end the host or add a user part.
	safeValueRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$`)
	hostRE      = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

func parse(b []byte) ([]Preset, error) {
	var ps []Preset
	err := yaml.UnmarshalStrict(b, &ps)
	return ps, err
}

// Parse reads catalog YAML (used by presetcheck to read the base branch's catalog).
func Parse(b []byte) ([]Preset, error) { return parse(b) }

func All() ([]Preset, error) { return parse(catalog) }

// LoadTesting returns the presets for the kind tests (AMBIT_TEST_PRESETS=1 only).
func LoadTesting() ([]Preset, error) {
	ps, err := parse(testing_)
	for i := range ps {
		ps[i].Testing = true
	}
	return ps, err
}

func Get(id string) (Preset, bool) {
	ps, err := All()
	if err != nil {
		return Preset{}, false
	}
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Hash identifies an entry's content; Verified is left out so marking a preset verified needs no new version.
func Hash(p Preset) string {
	p.Verified, p.VerifiedOn, p.Testing = false, "", false
	b, _ := json.Marshal(p) // Go sorts map keys
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fill replaces {name} holes. Only checked values are used: vals holds the declared placeholders after
// validation, so a hole the preset does not declare is an error, never a caller-chosen string.
func fill(s string, vals map[string]string, clientID string) (string, error) {
	var bad error
	out := holeRE.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		switch name {
		case "clientID":
			return url.PathEscape(clientID)
		case "callbackURL":
			return m // only Setup substitutes it
		}
		v, ok := vals[name]
		if !ok {
			bad = fmt.Errorf("{%s} is not a declared placeholder", name)
		}
		return v
	})
	return out, bad
}

func (p Preset) validURL(s string) bool {
	u, err := url.Parse(s)
	// The literal lowercase prefix is required (u.Scheme is lowercased and ? or # may leave RawQuery empty).
	if !strings.HasPrefix(s, "https://") && !(p.Testing && strings.HasPrefix(s, "http://")) {
		return false
	}
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || !hostRE.MatchString(u.Host) ||
		strings.ContainsAny(s, "\\ \t\r\n?#") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	return p.Testing && u.Scheme == "http" &&
		(strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local") || strings.HasSuffix(h, ".localtest.me") || h == "localhost")
}

// Resolve turns the preset into the specs to create. Every placeholder value must match its pattern and the
// plain-label rule, and every resulting URL must still be https with the host the preset intends. Nothing
// is returned unless every check passes.
func (p Preset) Resolve(values map[string]string, clientID, callbackURL string) (Resolved, error) {
	vals := map[string]string{}
	for _, ph := range p.Placeholders {
		re, err := regexp.Compile(ph.Pattern)
		if err != nil {
			return Resolved{}, fmt.Errorf("preset %s: placeholder %s: %v", p.ID, ph.Name, err)
		}
		v := values[ph.Name]
		if !re.MatchString(v) || !safeValueRE.MatchString(v) || strings.Contains(v, "..") {
			return Resolved{}, fmt.Errorf("%s: %q is not valid", ph.Label, v)
		}
		vals[ph.Name] = v
	}
	if clientID == "." || clientID == ".." {
		return Resolved{}, fmt.Errorf("the client ID %q is not valid", clientID)
	}
	for label, u := range map[string]string{"authorization URL": p.AuthorizationURL, "token URL": p.TokenURL, "server URL": p.Server.URL} {
		if u == "" {
			return Resolved{}, fmt.Errorf("preset %s has no %s", p.ID, label)
		}
	}
	if len(p.AuthorizeParams) > maxAuthorizeParams {
		return Resolved{}, fmt.Errorf("preset %s: at most %d authorizeParams", p.ID, maxAuthorizeParams)
	}
	for k := range p.AuthorizeParams {
		for _, r := range reserved {
			if k == r {
				return Resolved{}, fmt.Errorf("preset %s: authorizeParams may not set %s", p.ID, k)
			}
		}
	}
	if f := p.TokenRequestFormat; f != "" && f != "Form" && f != "JSON" {
		return Resolved{}, fmt.Errorf("preset %s: tokenRequestFormat must be Form or JSON", p.ID)
	}
	var err error
	conn := v1.ConnectionSpec{DisplayName: p.DisplayName, Scopes: append([]string(nil), p.Scopes...), TokenRequestFormat: p.TokenRequestFormat}
	srv := v1.ServerSpec{DisplayName: p.DisplayName, Transport: p.Server.Transport}
	for _, f := range []struct {
		label, src string
		dst        *string
	}{
		{"authorization URL", p.AuthorizationURL, &conn.AuthorizationURL},
		{"token URL", p.TokenURL, &conn.TokenURL},
		{"revocation URL", p.RevocationURL, &conn.RevocationURL},
		{"server URL", p.Server.URL, &srv.URL},
	} {
		if f.src == "" {
			continue
		}
		if *f.dst, err = fill(f.src, vals, clientID); err != nil {
			return Resolved{}, err
		}
		if !p.validURL(*f.dst) {
			return Resolved{}, fmt.Errorf("the %s %q is not a valid https URL", f.label, *f.dst)
		}
	}
	for i, sc := range conn.Scopes {
		if conn.Scopes[i], err = fill(sc, vals, clientID); err != nil || !scopeRE.MatchString(conn.Scopes[i]) {
			return Resolved{}, fmt.Errorf("scope %q is not valid", sc)
		}
	}
	if len(p.AuthorizeParams) > 0 {
		conn.AuthorizeParams = map[string]string{}
	}
	for k, v := range p.AuthorizeParams {
		if conn.AuthorizeParams[k], err = fill(v, vals, clientID); err != nil {
			return Resolved{}, err
		}
	}
	conn.ServerURL = srv.URL
	switch p.Resource {
	case "", "default":
		conn.Resource = srv.URL
	case "none":
	default:
		if conn.Resource, err = fill(p.Resource, vals, clientID); err != nil {
			return Resolved{}, err
		}
		if !p.validURL(conn.Resource) {
			return Resolved{}, fmt.Errorf("the resource %q is not a valid https URL", conn.Resource)
		}
	}
	return Resolved{Connection: conn, Server: srv, Setup: strings.ReplaceAll(p.Setup, "{callbackURL}", callbackURL)}, nil
}

// Validate checks the catalog itself; every problem is a CI failure, never a production one.
func Validate(ps []Preset) []string {
	var probs []string
	seen := map[string]bool{}
	for _, p := range ps {
		bad := func(f string, a ...any) { probs = append(probs, p.ID+": "+fmt.Sprintf(f, a...)) }
		if seen[p.ID] {
			bad("duplicate id")
		}
		seen[p.ID] = true
		if p.Version < 1 {
			bad("version must be 1 or more")
		}
		if !strings.Contains(p.Setup, "{callbackURL}") {
			bad("setup must show {callbackURL}")
		}
		if len(p.AuthorizeParams) > maxAuthorizeParams {
			bad("authorizeParams has more than %d entries", maxAuthorizeParams)
		}
		for k := range p.AuthorizeParams {
			for _, r := range reserved {
				if k == r {
					bad("authorizeParams may not set %s", k)
				}
			}
		}
		if f := p.TokenRequestFormat; f != "" && f != "Form" && f != "JSON" {
			bad("tokenRequestFormat must be Form or JSON")
		}
		if p.Server.Transport != "streamable-http" && p.Server.Transport != "sse" {
			bad("server transport must be streamable-http or sse")
		}
		if p.DisplayName == "" {
			bad("displayName is required")
		}
		if p.Verified && p.Live.Tool == "" {
			bad("verified without a live check")
		}
		values := map[string]string{}
		patternBad := false
		names := map[string]bool{}
		for _, ph := range p.Placeholders {
			if names[ph.Name] {
				bad("duplicate placeholder %s", ph.Name)
			}
			names[ph.Name] = true
			if !strings.HasPrefix(ph.Pattern, "^") || !strings.HasSuffix(ph.Pattern, "$") {
				bad("placeholder %s: pattern must start with ^ and end with $", ph.Name)
			}
			if !nameRE.MatchString(ph.Name) || ph.Name == "clientID" || ph.Name == "callbackURL" {
				bad("placeholder name %q is not allowed", ph.Name)
			}
			if _, err := regexp.Compile(ph.Pattern); err != nil {
				bad("placeholder %s: %v", ph.Name, err)
				patternBad = true
				continue
			}
			values[ph.Name] = ph.Example
		}
		if patternBad {
			continue // already reported; Resolve would only repeat it
		}
		if _, err := p.Resolve(values, "example-client", "https://ambit.example.test/connections/callback"); err != nil {
			bad("with the example values: %v", err)
		}
	}
	sort.Strings(probs)
	return probs
}
