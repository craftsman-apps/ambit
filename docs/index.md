---
title: Ambit documentation
description: Ambit is a self-hosted gateway that controls a company's use of AI models and MCP servers.
slug: /
---

# Ambit documentation

**Ambit is the AI access layer for enterprises.**

Ambit is a self-hosted gateway, built on agentgateway, ToolHive and other open-source projects, that controls a company's use of AI. It decides who may use which AI models and which work apps (through MCP), sets spending limits, keeps an audit record, and removes access when someone leaves the company.

[agentgateway](https://agentgateway.dev) handles the traffic and [ToolHive](https://github.com/stacklok/toolhive) runs the MCP servers. Ambit adds identity-provider access rules, a console, per-person app sign-in, budgets, audit and offboarding, in one Helm chart.

## Why Ambit

![Without Ambit: Alice, Dan and Bob share one team API key and call models directly, an AI agent reaches work apps through a shared service account, Bob’s key still works after he left, there is no record and the cost appears on next month’s bill. With Ambit: everyone passes the Ambit gateway, which checks who they are, what they may use and their daily limit, records every call, and refuses Bob.](./assets/brand/ambit-why.svg#gh-light-mode-only)
![Without Ambit: Alice, Dan and Bob share one team API key and call models directly, an AI agent reaches work apps through a shared service account, Bob’s key still works after he left, there is no record and the cost appears on next month’s bill. With Ambit: everyone passes the Ambit gateway, which checks who they are, what they may use and their daily limit, records every call, and refuses Bob.](./assets/brand/ambit-why-dark.svg#gh-dark-mode-only)

| Without Ambit | With Ambit |
|---|---|
| Each team shares one provider API key, copied into scripts and laptops. | People sign in, or use a personal API key. Provider keys stay inside Ambit. |
| Anyone who has the key can use any model. | Access rules based on your identity provider's groups decide which models and MCP servers each team may use. |
| AI agents connect to work apps on their own, often through one shared service account. | Every MCP call passes one gateway. People connect their own app accounts, tools are checked, and personal data in results is masked. |
| The cost shows up on next month's bill. | Daily limits per group and per person, in requests or in dollars. |
| Nobody can say who sent what to which model. | Every call is recorded, refusals included. Model refusals do not yet show who made them (an agentgateway bug; see Known limitations). |
| Someone leaves, and their keys work until someone remembers to rotate them. | The identity provider disables them, and their keys, sessions and app sign-ins stop at once. |

## Architecture

![Request path: clients call one Ambit gateway, built on agentgateway, which checks the caller and their access, asks the budget service, guard, token broker and router as needed, then forwards to AI models or MCP servers and sends audit records through the OpenTelemetry Collector to ClickHouse. Setup: your identity provider signs people in through Dex and reports leavers to the console by SCIM; admins use the console, which stores Ambit objects; ambit-controller turns them into gateway configuration.](./assets/brand/ambit-architecture.svg#gh-light-mode-only)
![Request path: clients call one Ambit gateway, built on agentgateway, which checks the caller and their access, asks the budget service, guard, token broker and router as needed, then forwards to AI models or MCP servers and sends audit records through the OpenTelemetry Collector to ClickHouse. Setup: your identity provider signs people in through Dex and reports leavers to the console by SCIM; admins use the console, which stores Ambit objects; ambit-controller turns them into gateway configuration.](./assets/brand/ambit-architecture-dark.svg#gh-dark-mode-only)

Blue is the path every request takes; grey is how admins and your identity provider set things up.

## Status: Alpha

Ambit is pre-1.0. The API is `ambitproject.dev/v1alpha1` and can change between releases.

| Tested | Not tested yet |
|---|---|
| Install and all features on kind, in CI on every pull request | Azure OpenAI, Amazon Bedrock and Vertex AI with real cloud accounts |
| Company sign-in through a test Keycloak, external ClickHouse and Valkey, GitOps with Argo CD | A 24-hour soak test (CI runs a 5-minute soak) |

## Known limitations

When the model router refuses a request (status 403 or 404), Ambit records the refusal but not who made it. This is a bug in agentgateway v1.6.0, which answers before it takes the request snapshot that holds the caller's identity. The record keeps the time, the path and the status code, but not the caller or the model. Ambit has no workaround: a check before the router would not cover unknown models (404), and it would change the refusal message. The limitation ends when Ambit moves to an agentgateway version that takes the snapshot before it answers.

## When not to use Ambit

- You need a hosted service. Ambit runs only in your Kubernetes cluster.
- You do not run Kubernetes. Ambit is installed with Helm and has no other install method.
- You need a stable API today. Wait for a release after `v1alpha1`.
- You need one proxy for a single team or one provider. A plain proxy is simpler.

## Next steps

- [Quick start](quick-start.md): install Ambit on a laptop with kind, add a free model and make a first call.
- [Concepts](concepts.md): the Ambit objects and how a request is checked.
- [Install on your cluster](install.md): single sign-on, TLS, your own ClickHouse and Valkey, Argo CD and a production checklist.
- Guides: [coding tools](guides/coding-tools.md) ([Codex, opencode and Hermes](guides/codex-opencode-hermes.md), [other agents](guides/other-agents.md)), [models per team](guides/models-per-team.md), [MCP servers with per-person sign-in](guides/mcp-apps.md), [asking for an MCP app and approving it](guides/request-an-app.md), [Claude Desktop with MCP tools](guides/claude-desktop-mcp.md), [spending limits](guides/spending-limits.md), and [offboarding](guides/someone-leaves.md).
- Reference: [Models and providers](providers.md), [Personal API keys](api-keys.md), [MCP servers and apps](presets.md), [Budgets](budgets.md), [Model routing](routing.md), [Offboarding](offboarding.md), [Alerts](alerts.md), [Compliance and audits](compliance.md) and [Upgrading](upgrading.md).
