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
// emails are not required). Never the subject: an AccessRule's users match emails only. With requireVerified
// (accessRules.usersRequireVerifiedEmail), a JWT's email counts only when the token says email_verified: true.
func PrincipalEmail(requireVerified bool) string {
	has := HasEmail()
	if requireVerified {
		has += ` && has(jwt.email_verified) && jwt.email_verified == true`
	}
	return `(has(apiKey.keyId) ? apiKey.email : ((` + has + `) ? jwt.email.trim().lowerAscii() : ""))`
}

// PrincipalGroups are the groups of the same identity.
const PrincipalGroups = `(has(apiKey.keyId) ? apiKey.groups : (has(jwt.groups) ? jwt.groups : []))`

// Gate is the auth policy's Require rule: a presented key must be valid; otherwise the JWT must be. With services
// false (serviceAccounts.enabled: false), a service account's key is refused too.
func Gate(requireEmail, services bool) string {
	k := ValidKey
	if !services {
		k += ` && !apiKey.email.endsWith("` + ServiceDomain + `")`
	}
	return `(` + PresentedKey + `) ? (` + k + `) : (` + JWTGate(requireEmail) + `)`
}

// JWTGate is the JWT branch alone (keys switched off). A JWT whose identity is in the service account domain is
// refused (guard 1: only a service account's key may carry it): its email, and with requireEmail false also its
// subject, which then identifies a JWT without an email.
func JWTGate(requireEmail bool) string {
	email := `jwt.email.trim().lowerAscii().endsWith("` + ServiceDomain + `")`
	if requireEmail {
		return `has(jwt.sub) && ` + HasEmail() + ` && !` + email
	}
	return `has(jwt.sub) && !jwt.sub.trim().lowerAscii().endsWith("` + ServiceDomain + `") && !(` + HasEmail() + ` && ` + email + `)`
}

// ServiceDomain ends every service account identity (<name>@svc.ambit.internal) and nothing else: the gateways
// refuse it in a JWT, the console refuses it at sign-in, and SCIM refuses it. So a key's email ends with it if and
// only if the key belongs to a service account.
const ServiceDomain = "@svc.ambit.internal"

// IsService reports whether email (in any case or spacing) is in the service account domain.
func IsService(email string) bool {
	c, ok := CanonicalEmail(email)
	return ok && strings.HasSuffix(c, ServiceDomain)
}

// Person is true when the request's principal is not a service account: what an AccessRule's allSignedIn grants
// (decision SA-8 B). Only a key can carry a service account identity (the gate refuses such a JWT), so it looks at
// the key alone and never fails on a JWT without an email.
const Person = `!(has(apiKey.keyId) && apiKey.email.endsWith("` + ServiceDomain + `"))`

// Kind is the access log's ambit.identity: "service account", "key", "invalid key" or "jwt".
const Kind = `(` + ValidKey + `) ? (apiKey.email.endsWith("` + ServiceDomain + `") ? "service account" : "key") : ((` + PresentedKey + `) ? "invalid key" : "jwt")`
