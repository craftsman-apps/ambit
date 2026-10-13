<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/ambit-logo-dark.svg">
    <img alt="Ambit" src="docs/assets/brand/ambit-logo.svg" width="260">
  </picture>
</p>

<p align="center"><b>Ambit is the AI access layer for enterprises.</b></p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/github/license/craftsman-apps/ambit"></a>
  <a href="https://github.com/craftsman-apps/ambit/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/craftsman-apps/ambit/ci.yml?branch=main&label=CI"></a>
  <a href="https://github.com/craftsman-apps/ambit/releases"><img alt="Release" src="https://img.shields.io/github/v/release/craftsman-apps/ambit"></a>
</p>

<p align="center">
  <a href="https://ambitproject.dev/docs/">Documentation</a> ·
  <a href="https://ambitproject.dev/docs/quick-start">Quick start</a> ·
  <a href="https://ambitproject.dev/docs/install">Install on your cluster</a> ·
  <a href="SECURITY.md">Security</a>
</p>

## What Ambit is

Ambit is a self-hosted gateway, built on agentgateway, ToolHive and other open-source projects, that controls a company's use of AI. It decides who may use which AI models and which work apps (through MCP), sets spending limits, keeps an audit record, and removes access when someone leaves the company.

[agentgateway](https://agentgateway.dev) handles the traffic and [ToolHive](https://github.com/stacklok/toolhive) runs the MCP servers. Ambit adds identity-provider access rules, a console, per-person app sign-in, budgets, audit and offboarding, in one Helm chart.

## Why Ambit

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/ambit-why-dark.svg">
  <img alt="Without Ambit: Alice, Dan and Bob share one team API key and call models directly, an AI agent reaches work apps through a shared service account, Bob's key still works after he left, there is no record and the cost appears on next month's bill. With Ambit: everyone passes the Ambit gateway, which checks who they are, what they may use and their daily limit, records every call, and refuses Bob." src="docs/assets/brand/ambit-why.svg" width="100%">
</picture>

| Without Ambit | With Ambit |
|---|---|
| Each team shares one provider API key, copied into scripts and laptops. | People sign in, or use a personal API key. Provider keys stay inside Ambit. |
| Anyone who has the key can use any model. | Access rules based on your identity provider's groups decide which models and MCP servers each team may use. |
| AI agents connect to work apps on their own, often through one shared service account. | Every MCP call passes one gateway. People connect their own app accounts, tools are checked, and personal data in results is masked. |
| The cost shows up on next month's bill. | Daily limits per group and per person, in requests or in dollars. |
| Nobody can say who sent what to which model. | Every call is recorded, denied calls included. |
| Someone leaves, and their keys work until someone remembers to rotate them. | The identity provider disables them, and their keys, sessions and app sign-ins stop at once. |

## Architecture

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/brand/ambit-architecture-dark.svg">
  <img alt="Request path: clients call one Ambit gateway, built on agentgateway, which checks the caller and their access, asks the budget service, guard, token broker and router as needed, then forwards to AI models or MCP servers and sends audit records through the OpenTelemetry Collector to ClickHouse. Setup: your identity provider signs people in through Dex and reports leavers to the console by SCIM; admins use the console, which stores Ambit objects; ambit-controller turns them into gateway configuration." src="docs/assets/brand/ambit-architecture.svg" width="100%">
</picture>

Blue is the path every request takes; grey is how admins and your identity provider set things up.

## Features

- **Approved models for coding tools.** Claude Code, Cursor and SDKs call your models through one address, with a personal API key or a token from your identity provider.
- **Models per team.** Access rules map identity-provider groups to models. Access is denied unless a rule allows it.
- **MCP servers with per-person sign-in.** People connect their own account to an app (for example GitHub or Slack). An admin chooses which tools of each MCP server a group may call.
- **Budgets.** Daily limits per group and per person, in requests and in estimated dollars.
- **Offboarding.** When the identity provider deactivates someone, Ambit revokes their sessions, API keys and stored app grants.

Every call is kept in an audit log, denied calls included, and shown in the console.

## Quick start

On a laptop with Docker, [kind](https://kind.sigs.k8s.io), kubectl and Helm. This installs release v0.1.0 for testing only (plain HTTP, one gateway replica).

```sh
R=https://github.com/craftsman-apps/ambit/releases/download/v0.1.0
curl -fsSLO $R/kind-cluster.yaml -O $R/quickstart.yaml -O $R/crds.yaml
kind create cluster --config kind-cluster.yaml
kubectl apply --server-side -f crds.yaml
helm install ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds --version 0.1.0 -n ambit-system --create-namespace --wait
helm install ambit oci://ghcr.io/craftsman-apps/charts/ambit --version 0.1.0 -n ambit-system -f quickstart.yaml --wait --timeout 10m
```

Open http://console.localtest.me:8080 and sign in as `admin@ambit.local`. `helm install` prints the command that shows the password.
The [full guide](https://ambitproject.dev/docs/quick-start) adds a free model, an access rule and a first call.

## Status: Alpha

Ambit is pre-1.0. The API is `ambitproject.dev/v1alpha1` and can change between releases.

| Tested | Not tested yet |
|---|---|
| Install and all features on kind, in CI on every pull request | Azure OpenAI, Amazon Bedrock and Vertex AI with real cloud accounts |
| Company sign-in through a test Keycloak, external ClickHouse and Valkey, GitOps with Argo CD | A 24-hour soak test (CI runs a 5-minute soak) |

## When not to use Ambit

- You need a hosted service. Ambit runs only in your Kubernetes cluster.
- You do not run Kubernetes. Ambit is installed with Helm and has no other install method.
- You need a stable API today. Wait for a release after `v1alpha1`.
- You need one proxy for a single team or one provider. A plain proxy is simpler.

## Documentation

- [Quick start](https://ambitproject.dev/docs/quick-start) and [Install on your cluster](https://ambitproject.dev/docs/install)
- [Concepts](https://ambitproject.dev/docs/concepts)
- [Models and providers](https://ambitproject.dev/docs/providers) · [Personal API keys](https://ambitproject.dev/docs/api-keys)
- [MCP servers and apps](https://ambitproject.dev/docs/presets) · [Budgets](https://ambitproject.dev/docs/budgets) · [Offboarding](https://ambitproject.dev/docs/offboarding)
- [Upgrading](https://ambitproject.dev/docs/upgrading)

## Credits

Ambit is built on these projects. Each keeps its own licence.

| Project | Use in Ambit | Licence |
|---|---|---|
| [agentgateway](https://github.com/agentgateway/agentgateway) | LLM and MCP traffic | Apache-2.0 |
| [ToolHive](https://github.com/stacklok/toolhive) | Runs MCP servers | Apache-2.0 |
| [Dex](https://github.com/dexidp/dex) | Sign-in and tokens | Apache-2.0 |
| [Gateway API](https://github.com/kubernetes-sigs/gateway-api) | Routing objects | Apache-2.0 |
| [ClickHouse](https://github.com/ClickHouse/ClickHouse) | Audit storage | Apache-2.0 |
| [OpenTelemetry Collector](https://github.com/open-telemetry/opentelemetry-collector-contrib) | Audit pipeline | Apache-2.0 |
| [Valkey](https://github.com/valkey-io/valkey) | Budget counters | BSD-3-Clause |
| [envoyproxy/ratelimit](https://github.com/envoyproxy/ratelimit) | Budget enforcement | Apache-2.0 |
| [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) | Preset checks and test MCP servers | Apache-2.0 (older code MIT) |
| Kubernetes client libraries, controller-runtime, CEL-go | Controller and rules | Apache-2.0 |
| React, TanStack, Tailwind CSS, Radix UI | Console front end | MIT |
| IBM Plex (Fontsource) | Console fonts | SIL OFL 1.1 |

## Contributing, security, licence

- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
- Security: report vulnerabilities privately, see [SECURITY.md](SECURITY.md).
- Licence: Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
