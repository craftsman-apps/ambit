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

See [api-keys.md](api-keys.md) for the new feature.
