# Upgrading

## To the release with personal API keys

- **Tokens need an email.** Gateway JWTs without a usable `email` claim are refused from this release. To
  accept them again, set `llm.jwt.requireEmail=false` and `mcp.jwt.requireEmail=false`. Offboarding then
  cannot block such tokens before they expire.
- **A call without a token gets 403, not 401**, while personal keys are on (the default). With
  `apiKeys.enabled=false` the gateways check tokens strictly again. Update scripts that test for 401.
- **Kubernetes 1.30 or later is required.** The chart refuses to render on older clusters.
- **The console restarts one pod at a time.** The console Deployment now uses `RollingUpdate` with
  `maxSurge: 0`. The old console stops before the new one starts, so the console is briefly unavailable
  during an upgrade. LLM requests, MCP requests and key checks keep working during a console upgrade, because the gateways read keys from Secrets. Requests that need a Connection token (SaaS access through the console's token broker) fail until the new console is ready.
- **Emails are lower-case in budgets and audit records.** The gateways now identify a person by their email
  with `A`-`Z` lower-cased. Per-person budget counters for an email with capitals start again from zero. Audit
  records from this release show the lower-case email: search for it in lower case. Older records keep the
  email as it was sent.
- **Sessions from before the upgrade cannot create keys.** People see "Sign in again to create keys" until
  they sign out and sign in again.

See [api-keys.md](api-keys.md) for the new feature.
