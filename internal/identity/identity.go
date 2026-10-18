// Package identity decides who a gateway request belongs to. The CEL fragments here are the single source for
// the controller's generated rules and the chart's policies (identity_test.go checks the chart uses them).
package identity

import (
	"regexp"
	"strings"
)

var usable = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

// CanonicalEmail trims and lower-cases A-Z only (as CEL's lowerAscii), and reports whether the result is usable.
func CanonicalEmail(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !usable.MatchString(s) {
		return "", false
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b), true
}

// PresentedKey is true when the request carries a key (it must then be valid). Only meaningful before the route
// stage removes the headers: in the auth Require rule, and in access logs of refused requests.
const PresentedKey = `"x-api-key" in request.headers || ("authorization" in request.headers && request.headers["authorization"].startsWith("Bearer ambit_"))`

// ValidKey is true for a matching, unexpired key. agentgateway attaches a matching key's metadata before the
// authorization stage checks until, so has(apiKey.keyId) alone is true for expired keys.
const ValidKey = `has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime`

// HasEmail: the JWT carries a usable email (same rule as CanonicalEmail).
func HasEmail() string {
	return `has(jwt.email) && type(jwt.email) == string && jwt.email.trim().matches('^[^@\\s]+@[^@\\s]+$')`
}

// Principal is the request's one identity: the key's email, else the JWT's canonical email (else its subject
// when emails are not required). Valid after the auth Require rule; never mixes the two credentials.
func Principal(requireEmail bool) string {
	if requireEmail {
		return `(has(apiKey.keyId) ? apiKey.email : jwt.email.trim().lowerAscii())`
	}
	return `(has(apiKey.keyId) ? apiKey.email : ((` + HasEmail() + `) ? jwt.email.trim().lowerAscii() : jwt.sub))`
}

// PrincipalEmail is the same identity's email, or "" when it has none (a JWT without a usable email, accepted when
// emails are not required). Never the subject: an AccessRule's users match emails only.
func PrincipalEmail() string {
	return `(has(apiKey.keyId) ? apiKey.email : ((` + HasEmail() + `) ? jwt.email.trim().lowerAscii() : ""))`
}

// PrincipalGroups are the groups of the same identity.
const PrincipalGroups = `(has(apiKey.keyId) ? apiKey.groups : (has(jwt.groups) ? jwt.groups : []))`

// Gate is the auth policy's Require rule: a presented key must be valid; otherwise the JWT must be.
func Gate(requireEmail bool) string {
	j := JWTGate(requireEmail)
	if requireEmail {
		j = "(" + j + ")"
	}
	return `(` + PresentedKey + `) ? (` + ValidKey + `) : ` + j
}

// JWTGate is the JWT branch alone (keys switched off).
func JWTGate(requireEmail bool) string {
	if requireEmail {
		return `has(jwt.sub) && ` + HasEmail()
	}
	return `has(jwt.sub)`
}
