# Upgrading

## To the release with personal API keys

- **Tokens need an email.** Gateway JWTs without a usable `email` claim are refused from this release. To
  accept them again, set `llm.jwt.requireEmail=false` and `mcp.jwt.requireEmail=false`. Offboarding then
  cannot block such tokens before they expire.
- **No token gives 403.** A call without a token now gets 403. It was 401.
- **Kubernetes 1.30 or later is required.** The chart refuses to render on older clusters.
- **The console restarts one pod at a time.** The console Deployment now uses `RollingUpdate` with
  `maxSurge: 0`. The old console stops before the new one starts, so the console is briefly unavailable
  during an upgrade. Gateways keep serving.

See [api-keys.md](api-keys.md) for the new feature.
