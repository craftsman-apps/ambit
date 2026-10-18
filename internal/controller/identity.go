package controller

import "github.com/craftsman-apps/ambit/internal/identity"

// RequireEmail mirrors llm/mcp.jwt.requireEmail (flag --require-email). When false, a JWT without an email is
// identified by its subject.
var RequireEmail = true

func principal() string { return identity.Principal(RequireEmail) }

// UsersRequireVerifiedEmail mirrors accessRules.usersRequireVerifiedEmail (flag --users-require-verified-email):
// an AccessRule's users match a JWT's email only when the token says email_verified: true.
var UsersRequireVerifiedEmail = true

func principalEmail() string { return identity.PrincipalEmail(UsersRequireVerifiedEmail) }

const principalGroups = identity.PrincipalGroups
