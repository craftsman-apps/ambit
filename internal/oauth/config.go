package oauth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Config is what ambit-auth needs to know. The chart writes it; Validate refuses what a wrong value would make unsafe.
type Config struct {
	PublicURL string // the OAuth issuer and the sign-in host, https://auth.example.com
	Resource  string // the one MCP URL a token is for, https://mcp.example.com/mcp

	IdPIssuer       string // the company's OIDC provider; exact string match
	IdPClientID     string
	IdPClientSecret string
	GroupsClaim     string

	NoRegistration    bool     // dynamic registration is off: no /register, no registration_endpoint
	RegistrationHosts []string // redirect hosts that dynamic registration accepts
	MaxClients        int      // dynamic clients (the cap)
	UnusedClientDays  int      // a dynamic client not used for this many days is deleted
	MaxGrantDays      int      // a grant is refreshed for at most this many days; then the person connects again (0: no limit)
}

// Platforms maps the redirect host of a client to the platform name that the agent declares. A host that is not here
// is declared "other". It is a label for people and for audit, never a proof (docs/agents.md).
var Platforms = map[string]string{
	"chatgpt.com":                     "chatgpt",
	"claude.ai":                       "claude-ai",
	"vertexaisearch.cloud.google.com": "gemini-enterprise",
}

// Scopes are the OAuth scopes of Ambit: mcp (use the MCP route) and offline_access (a refresh token).
var Scopes = []string{"mcp", "offline_access"}

// redirectPathRE is the path of a registration entry: segments of ordinary characters (no percent escape, no "$", "(", ")" or ";": a path must mean the same to every reader), no "." or ".." segment, no empty segment, no query.
var redirectPathRE = regexp.MustCompile(`^(/[A-Za-z0-9_~!&'*+=:@-][A-Za-z0-9._~!&'*+=:@-]*)+$`)

// splitRedirectEntry splits an entry of RegistrationHosts: "chatgpt.com" (any path on the host) or "chatgpt.com/connector/oauth" (that path, and
// the paths below it).
func splitRedirectEntry(e string) (host, path string) {
	if i := strings.Index(e, "/"); i >= 0 {
		return e[:i], e[i:]
	}
	return e, ""
}

// RedirectAllowed reports whether a registered redirect address is on an entry of RegistrationHosts: the host is the entry's host, and the path is
// whatever the entry names, or below it. An entry without a path allows every path of the host. A path with a dot segment, an empty segment, a percent
// escape or a backslash is never allowed under an entry that names a path (a browser or the platform may read it as another path).
func (c Config) RedirectAllowed(u *url.URL) bool {
	for _, e := range c.RegistrationHosts {
		h, p := splitRedirectEntry(e)
		if h != u.Hostname() {
			continue
		}
		if p == "" {
			return true
		}
		if strings.Contains(u.EscapedPath(), "%") || strings.Contains(u.Path, "\\") { // an escaped spelling may be routed as another path by the platform
			continue
		}
		if u.Path == p || (strings.HasPrefix(u.Path, p+"/") && redirectPathRE.MatchString(u.Path)) {
			return true
		}
	}
	return false
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func httpsURL(name, raw string, wantPath bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("%s must be an https URL without user, query or fragment", name)
	}
	if !wantPath && (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s must not have a path", name)
	}
	if wantPath && (u.Path == "" || u.Path == "/") {
		return nil, fmt.Errorf("%s must have a path", name)
	}
	if strings.HasSuffix(raw, "/") {
		return nil, fmt.Errorf("%s must not end with a slash: the issuer is compared as written", name)
	}
	return u, nil
}

func (c *Config) Validate() error {
	var errs []error
	if _, err := httpsURL("the public URL", c.PublicURL, false); err != nil {
		errs = append(errs, err)
	}
	if _, err := httpsURL("the MCP resource", c.Resource, true); err != nil {
		errs = append(errs, err)
	}
	// The issuer may have a path (Keycloak: https://idp.example.com/realms/acme); it is compared as written.
	if u, err := url.Parse(c.IdPIssuer); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || strings.HasSuffix(c.IdPIssuer, "/") {
		errs = append(errs, errors.New("the identity provider issuer must be an https URL without user, query, fragment or final slash"))
	}
	if c.IdPClientID == "" || c.IdPClientSecret == "" {
		errs = append(errs, errors.New("the identity provider client ID and secret are needed"))
	}
	if len(c.RegistrationHosts) == 0 && !c.NoRegistration {
		errs = append(errs, errors.New("registration hosts: name at least one, or turn dynamic registration off"))
	}
	for _, e := range c.RegistrationHosts {
		h, p := splitRedirectEntry(e)
		if !hostRE.MatchString(h) {
			errs = append(errs, fmt.Errorf("registration host %q must be a lower-case host name, with an optional path (no scheme, port or wildcard)", e))
		} else if p != "" && !redirectPathRE.MatchString(p) {
			errs = append(errs, fmt.Errorf("registration entry %q: the path must start with / and have no query, fragment, dot segments or empty segments", e))
		}
	}
	if !c.NoRegistration && (c.MaxClients < 1 || c.MaxClients > 5000) {
		errs = append(errs, errors.New("the client cap must be 1 to 5000"))
	}
	if !c.NoRegistration && c.UnusedClientDays < 1 {
		errs = append(errs, errors.New("unused client days must be 1 or more"))
	}
	if c.MaxGrantDays < 0 || c.MaxGrantDays > 365 {
		errs = append(errs, errors.New("the longest grant must be 0 to 365 days"))
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	return errors.Join(errs...)
}

// Platform is the platform that a redirect host declares.
func Platform(host string) string {
	if p, ok := Platforms[host]; ok {
		return p
	}
	return "other"
}

func inScopes(requested []string) bool {
	for _, s := range requested {
		if !slices.Contains(Scopes, s) {
			return false
		}
	}
	return true
}
