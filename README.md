# Ambit

Ambit is an open-source MCP and LLM gateway stack for enterprises, installed with Helm.
It uses [agentgateway](https://agentgateway.dev) for traffic and adds what enterprises need to operate it:
a console, access rules by identity-provider group, and (in progress) approvals, offboarding and audit.

## What works today

| Area | Status |
|---|---|
| Console sign-in (Dex): bootstrap admin, company SSO (OIDC/SAML/LDAP), roles from IdP groups | done |
| LLM traffic at `<llm.host>/v1` with tokens from your IdP | done |
| `Model` and `AccessRule` objects; per-model access compiled into agentgateway; deny by default | done |
| Console: Access rules and Models screens; GitOps-managed rules are read-only | done |
| MCP servers, approvals, offboarding, audit, budgets | planned |

## Try it locally (kind)

Requires Docker, kind, kubectl, Helm and uv.

```sh
make web                     # build the front end once (the Go binary embeds it)
make kind-up image install   # kind cluster + Ambit
make test                    # acceptance tests for every step
```

Console: http://console.localtest.me:8080 — the bootstrap admin password is printed by `helm install`.
The LLM tests call OpenRouter free models when `~/.config/ambit/openrouter-api-key` exists and skip those calls otherwise.

## Use your own ClickHouse and Valkey

By default the chart runs a ClickHouse (audit) and a Valkey (budget counters) inside the release. To use
managed ones instead (for example ClickHouse Cloud and Amazon ElastiCache), set their address; the chart
then installs neither. TLS is on by default; give a CA Secret (key `ca.crt`) only for a private CA.

```yaml
evidence:
  enabled: true
  clickhouse:
    external:
      host: abc123.eu-west-1.aws.clickhouse.cloud   # native protocol on 9440, HTTPS on 8443
      user: ambit             # needs CREATE TABLE once in the database, then INSERT and SELECT
      database: otel
      passwordSecret: ambit-clickhouse-external   # key "password"
limits:
  enabled: true
  valkey:
    external:
      address: master.ambit.abc123.euw1.cache.amazonaws.com:6379
      passwordSecret: ambit-valkey-external       # key "password"; leave empty without AUTH
```

Create the Secrets in the release namespace first, for example
`kubectl create secret generic ambit-clickhouse-external --from-literal=password=...`.
`test/stepext.sh` installs Ambit against a separate ClickHouse and Valkey with TLS from a private CA.

## Connections

Add a work app from a tested preset (vendor MCP server, per-user sign-in, tool-by-tool access); see
[docs/presets.md](docs/presets.md).

## Budgets

Daily limits per group and per person, in requests and in estimated dollars from each model's price; see
[docs/budgets.md](docs/budgets.md).

## Offboarding

When someone leaves, Ambit revokes their stored SaaS grants at each provider: in the standard (RFC 7009)
format, or in GitHub's, Slack's, Notion's or HubSpot's own. Microsoft and Atlassian offer no way to revoke;
[docs/offboarding.md](docs/offboarding.md) lists each provider and the URL to enter.

## Layout

| Path | Contents |
|---|---|
| `api/v1alpha1` | Ambit API types (`ambitproject.dev`) |
| `cmd/ambit-console`, `internal/console` | Console back end (Go) |
| `cmd/ambit-controller`, `internal/controller` | Controller: Ambit objects → agentgateway objects |
| `web/` | Console front end (Vite, React, TanStack, shadcn-style components) |
| `charts/ambit-crds`, `charts/ambit` | Helm charts (install `ambit-crds` first) |
| `test/` | Acceptance tests per build step |

## Licence

Apache-2.0. See [LICENSE](LICENSE).
