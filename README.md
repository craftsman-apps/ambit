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
