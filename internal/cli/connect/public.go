package connect

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	privateSuffix = regexp.MustCompile(`(^|\.)(localhost|localtest\.me|local|internal|lan|home\.arpa)$`)
	ipv4          = regexp.MustCompile(`^(\d+)\.(\d+)\.\d+\.\d+$`)
)

// IsPublicHTTPS is the Go version of isPublicHttps in web/src/lib/tool-setup.ts: Cursor calls the model from its
// own servers, so the address must be reachable from the internet over HTTPS. Keep the two the same; their tests
// share the same cases.
func IsPublicHTTPS(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || strings.HasPrefix(u.Host, "[") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if !strings.Contains(h, ".") || privateSuffix.MatchString(h) {
		return false
	}
	if m := ipv4.FindStringSubmatch(h); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		return !(a == 10 || a == 127 || a == 0 || (a == 169 && b == 254) || (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168))
	}
	return true
}
