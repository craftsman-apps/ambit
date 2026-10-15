package connect

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeURL makes the gateway address the same string everywhere it is used as a key: scheme and host in lower
// case, no trailing "/" and no trailing "/v1" (the tools' base URLs add it themselves).
func NormalizeURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("%q is not a web address", s)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q must start with http:// or https:// and name a host", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("%q must not contain a user name, a query or a fragment", s)
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.EscapedPath(), "/"), "/v1")
	return scheme + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(path, "/"), nil
}
