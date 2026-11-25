package controller

import "github.com/craftsman-apps/ambit/internal/identity"

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
