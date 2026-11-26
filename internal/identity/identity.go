// Package identity decides who a gateway request belongs to. The CEL fragments here are the single source for
// the controller's generated rules and the chart's policies (identity_test.go checks the chart uses them).
package identity

import (
	"regexp"
	"strconv"
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

// SourceRanges is an extra Require rule on the MCP route: a key that names source ranges (an agent key, at most 10
// CIDRs) is accepted only from an address inside one of them. A key without ranges, and every JWT, passes. A range
// that does not parse is an error, and an error in a Require refuses the request. The client address is
// source.address (frontDoor.clientAddress says how the Gateway learns it).
const SourceRanges = `!has(apiKey.sourceRanges) || apiKey.sourceRanges.exists(r, cidr(r).containsIP(source.address))`

func scopeMatch(list string) string {
	return `(mcp.tool.target in ` + list + ` || (mcp.tool.target + "/" + mcp.tool.name) in ` + list + `)`
}

// AgentScope is the one clause the MCP access policy adds to every rule: what an agent may use, as carried by its
// credential (a list of "server" and "server/tool" entries, at most 50), on top of its owner's AccessRules. It does
// not depend on the number of agents. A key of an agent without a scope list, and with tokenIssuer set a token of
// that issuer without one, may use nothing (fail closed); an empty list matches nothing; a scope that is not a list
// (CEL's `in` also finds the keys of a map) matches nothing. A key without an agent and
// a JWT of another issuer are not limited. tokenIssuer is the issuer whose tokens may carry agent claims ("" in
// 0.6.0 until OAuth is on: no JWT is then limited).
func AgentScope(tokenIssuer string) string { return AgentScopeFor(tokenIssuer, false) }

// AgentScopeFor is AgentScope for one backend. On the public backend (public true) every JWT is an OAuth token of Ambit's own sign-in host (the
// public route accepts no other), so without a token issuer, or with another one than the token's, it may use nothing: a controller that was
// started without the issuer (a failed or slow upgrade), or with an issuer that was changed, must not let a token past its scope.
func AgentScopeFor(tokenIssuer string, public bool) string {
	key := `has(apiKey.keyId) ? (!has(apiKey.agent) || (has(apiKey.agentScope) && type(apiKey.agentScope) == list && ` + scopeMatch("apiKey.agentScope") + `)) : `
	if tokenIssuer == "" {
		if public {
			return key + `!has(jwt.iss)`
		}
		return key + `true`
	}
	other := `true`
	if public {
		other = `!has(jwt.iss)` // every JWT on the public route is an OAuth token: one of another issuer (an issuer that was changed) may use nothing
	}
	return key + `((has(jwt.iss) && jwt.iss == ` + strconv.Quote(tokenIssuer) + `) ? (has(jwt.ambit_scope) && type(jwt.ambit_scope) == list && ` + scopeMatch("jwt.ambit_scope") + `) : ` + other + `)`
}

// AgentAttr is one audit attribute of an agent call, read only from a verified credential: the metadata field
// keyField of a valid key ("" when keys have no such field), or the claim of a token whose validated issuer is
// tokenIssuer (claim "" or tokenIssuer "": none). "" otherwise.
func AgentAttr(keyField, claim, tokenIssuer string) string {
	jwt := `""`
	if claim != "" && tokenIssuer != "" {
		jwt = `((has(jwt.iss) && jwt.iss == ` + strconv.Quote(tokenIssuer) + ` && has(jwt.` + claim + `)) ? jwt.` + claim + ` : "")`
	}
	if keyField == "" {
		return jwt
	}
	return `((` + ValidKey + `) && has(apiKey.` + keyField + `)) ? apiKey.` + keyField + ` : ` + jwt
}

// TokenIssuer is the access log's ambit.token_issuer: the validated issuer of a JWT, "" for keys and for requests
// that carry no token.
const TokenIssuer = `((` + PresentedKey + `) || !has(jwt.iss)) ? "" : jwt.iss`

// StopList is one chunk of the gateway's stop policy (docs/agents.md, "Stopping an agent"): a Require rule that is true
// for every request except those of a stopped agent or grant. It is the authoritative check for every accepted
// credential: a key of a listed agent is refused although its Secret may not be deleted yet, and so is a token (when
// tokenIssuer is set: only a token of that issuer names an agent or a grant). agents and grants are quoted here; a name
// is at most 63 characters of [a-z0-9-], and a grant ID is the one Hydra makes.
func StopList(agents, grants []string, tokenIssuer string) string {
	quote := func(l []string) string {
		q := make([]string, len(l))
		for i, n := range l {
			q[i] = strconv.Quote(n)
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	var hit []string
	if len(agents) > 0 {
		hit = append(hit, `(has(apiKey.agent) && apiKey.agent in `+quote(agents)+`)`)
	}
	if tokenIssuer != "" {
		jwt := []string{}
		if len(agents) > 0 {
			jwt = append(jwt, `(has(jwt.ambit_agent) && jwt.ambit_agent in `+quote(agents)+`)`)
		}
		if len(grants) > 0 {
			jwt = append(jwt, `(has(jwt.ambit_grant) && jwt.ambit_grant in `+quote(grants)+`)`)
		}
		if len(jwt) > 0 {
			hit = append(hit, `(has(jwt.iss) && jwt.iss == `+strconv.Quote(tokenIssuer)+` && (`+strings.Join(jwt, " || ")+`))`)
		}
	}
	if len(hit) == 0 {
		return "true"
	}
	return "!(" + strings.Join(hit, " || ") + ")"
}
