package controller

import (
	"strconv"

	"github.com/craftsman-apps/ambit/internal/identity"
)

// RequireEmail mirrors llm/mcp.jwt.requireEmail (flag --require-email). When false, a JWT without an email is
// identified by its subject.
var RequireEmail = true

func principal() string { return identity.Principal(RequireEmail) }

// person is what allSignedIn grants: any principal that is not a service account.
func person() string { return identity.Person }

// UsersRequireVerifiedEmail mirrors accessRules.usersRequireVerifiedEmail (flag --users-require-verified-email):
// an AccessRule's users match a JWT's email only when the token says email_verified: true.
var UsersRequireVerifiedEmail = true

func principalEmail() string { return identity.PrincipalEmail(UsersRequireVerifiedEmail) }

const principalGroups = identity.PrincipalGroups

// AgentTokenIssuer mirrors agents.tokenIssuer (flag --agent-token-issuer): the issuer whose tokens may carry agent
// claims. Empty until OAuth is on: no token is then limited by an agent scope.
var AgentTokenIssuer = ""

// agentScope is the clause every MCP access rule is combined with (docs/agents.md): what the calling agent's
// credential allows. It does not depend on the number of agents.
func agentScope() string { return identity.AgentScope(AgentTokenIssuer) }

// publicAgentScope is the same for the public backend: a token there is always an OAuth token, so without the issuer it is refused (identity.AgentScopeFor).
func publicAgentScope() string { return identity.AgentScopeFor(AgentTokenIssuer, true) }

// agentHeader is the value of x-ambit-agent that the gateway sends to the token broker, from the verified credential only:
//   - a valid key: the key's agent name, or "-" when the key is not an agent's;
//   - with an agent token issuer, a token of that issuer: the claim ambit_agent;
//   - anything else (a person's token): "-".
//
// A name is accepted only in the shape of an agent name; one that is not (an empty or missing claim, "-", odd characters)
// becomes "?", which no agent has, so the broker gives no token. "-" therefore comes only from a credential that is a
// person's own.
func agentHeader() string {
	const name = `^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`
	key := `(` + identity.ValidKey + `) ? (has(apiKey.agent) ? (type(apiKey.agent) == string && apiKey.agent.matches('` + name + `') ? apiKey.agent : "?") : "-") : `
	if AgentTokenIssuer == "" {
		return key + `"-"`
	}
	return key + `((has(jwt.iss) && jwt.iss == ` + strconv.Quote(AgentTokenIssuer) + `) ? (has(jwt.ambit_agent) && type(jwt.ambit_agent) == string && jwt.ambit_agent.matches('` + name + `') ? jwt.ambit_agent : "?") : "-")`
}
