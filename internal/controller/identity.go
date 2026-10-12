package controller

import "github.com/craftsman-apps/ambit/internal/identity"

// RequireEmail mirrors llm/mcp.jwt.requireEmail (flag --require-email). When false, a JWT without an email is
// identified by its subject.
var RequireEmail = true

func principal() string { return identity.Principal(RequireEmail) }

const principalGroups = identity.PrincipalGroups
