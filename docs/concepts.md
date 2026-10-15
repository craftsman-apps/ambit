---
title: Concepts
description: The Ambit objects, and the order in which Ambit checks a request to a model and a request to an MCP server.
---

# Concepts

Ambit has six objects. You create them with `kubectl`, with Git (Argo CD), or in the console. All of them are in the API group `ambitproject.dev/v1alpha1` and live in Ambit's namespace (`ambit-system` in this documentation).

Think of Ambit as a building with one front desk. Every request to a model or to an MCP server goes through the desk. The objects below are the lists the desk uses: who is allowed in, where they may go, and how much they may use.

## The objects

| Object | What it says | Main fields |
|---|---|---|
| `Model` | One model that clients may ask for, and where its provider credentials are. | `provider`, `model`, `credentialsRef`, `price`, `displayName` |
| `AccessRule` | Who may use which models and which MCP servers. Rules only grant access. Nothing is allowed by default. | `groups` or `allSignedIn`, `models`, `servers` |
| `Server` | One MCP server that clients may use. | `url` (a remote server) or `image` (run by ToolHive), `transport`, `mcpPort`, `connection`, `displayName` |
| `Budget` | A daily limit for one identity-provider group, and for each person in it. | `group`, `groupRequestsPerDay`, `groupDollarsPerDay`, `userRequestsPerDay`, `userDollarsPerDay` |
| `ModelRouter` | One model name (for example `auto`) that picks a `Model` for each request. | `model`, `default`, `classifier`, `routes` |
| `Connection` | A work app (a SaaS OAuth registration, for example GitHub) that each person signs in to with their own account. | `clientID`, `clientSecretRef`, `authorizationURL`, `tokenURL`, `scopes`, `serverURL`, `revocationURL` |

In this documentation, **MCP server** means a `Server`. **App** means a `Connection`: the OAuth registration that lets a person connect their own account to a work app. A `Server` with a `connection` field uses that app, so every call carries the calling person's own token.

### Model

`spec.model` is the exact name clients send in the request, for example `google/gemma-4-31b-it:free`. It is not the name of the `Model` object. The credentials are not in the object. `credentialsRef` names a Secret (`ambit-llm-<something>`) in Ambit's namespace, so the `Model` can be kept in Git. See [Models and providers](providers.md).

### AccessRule

A rule names its people with `groups` (groups from the `groups` claim of the identity provider) or with `allSignedIn: true` (everyone with a valid token). You set one of the two, never both. A rule names `models`, `servers`, or both. Each entry in `servers` has a `name` and, optionally, `tools`: the tools of that server the people may call. Without `tools`, every tool of the server is allowed, including tools it adds later. A server entry can also carry `arguments`, which limit the values a tool may be called with. That needs `protection.enabled` (see [Install on your cluster](install.md)).

Rules are deny by default, except in audit-only mode (`access.mode: audit`, see [Turn Ambit on without blocking anyone](guides/audit-only.md)). In that mode every signed-in request to a model or an MCP tool is allowed, and every tool is listed to everyone; each audit record says whether the rules in force at that moment would have refused the request. Still enforced in that mode: sign-in, the LLM path allowlist, offboarding, budgets and the refusal of an unpriced model for a group with a dollar limit, and argument limits and tool approval.

### Server

A `Server` is either a remote MCP server (`url`) or a container image that ToolHive runs inside your cluster (`image`, pinned by digest). A server with `connection` must be a remote server, because a per-person token cannot reach a container that speaks `stdio`. Ambit also keeps the definition of each tool when it first sees it (tool pinning). A tool whose definition changes later, and a new tool, is hidden and refused until an admin approves it again. See [MCP servers and apps](presets.md).

### Budget

A group has one `Budget`. Limits count requests, estimated dollars, or both, per day in UTC. A request over a limit gets HTTP 429. Dollars come from the `price` of the `Model`. See [Budgets](budgets.md).

### ModelRouter

A router is a model name that clients can ask for. For each request the gateway picks one of your models by ordered `routes`, and only a model the person may use. See [Model routing](routing.md).

### Connection

A `Connection` holds the OAuth settings of one work app. Each person opens "My connections" in the console and signs in to the app with their own account. Ambit stores that person's token and adds it to their calls. A `Connection` is bound to one MCP server through `serverURL`. When the person leaves the company, Ambit revokes the token at the vendor, if the vendor allows it. See [Offboarding](offboarding.md).

## Who is calling: identity

Ambit accepts three kinds of identity.

| Identity | Where it comes from | Typical use |
|---|---|---|
| Company token | Your identity provider signs people in through a Dex connector, or the gateway trusts the provider's tokens directly (`llm.jwt` and `mcp.jwt`). | People and scripts that can refresh a sign-in |
| Ambit's own Dex | The bundled Dex with the bootstrap admin (`admin@ambit.local`). Set `llm.jwt.dex` and `mcp.jwt.dex` to `true`. | The quick start and first setup |
| Personal API key | A person creates it in the console (**Keys**). It starts with `ambit_`. | Claude Code, Cursor, SDKs and MCP clients that cannot refresh a sign-in |

A personal API key carries the access of its owner, never more: the same access rules, budgets, audit records and offboarding. See [Personal API keys](api-keys.md).

## How a request is checked

Models and MCP servers use two separate paths. Both start at the same gateway (built on agentgateway) and both end in the audit log.

### Request to a model (`/v1`)

Clients call `https://<llm host>/v1/chat/completions` (and the other OpenAI-compatible and Anthropic-compatible paths).

1. **Identity.** The gateway checks the token or the personal API key. A request without valid credentials is refused.
2. **Model access.** An `AccessRule` must allow the requested model for this person. Otherwise the answer is 403. (In audit-only mode the request goes on, and the record says whether it would have been refused.)
3. **Budget.** The ratelimit service and Valkey count the request against the `Budget` of the person's group and against the person's own limit. Over the limit, the answer is 429.
4. **Router (optional).** If the requested name is a `ModelRouter`, Ambit picks one `Model` the person may use.
5. **Provider.** The gateway adds the provider credentials from the Secret and forwards the request.
6. **Audit.** The call is recorded, denied calls included.

### Request to an MCP server (`/mcp`)

Clients call `https://<mcp host>/mcp`.

1. **Identity.** The same three identity sources as above.
2. **Server and tool access.** An `AccessRule` must allow the `Server` and the tool. (In audit-only mode the call goes on, and the record says whether it would have been refused.)
3. **Guard.** With `protection.enabled`, `ambit-guard` refuses a tool whose definition changed (tool pinning), checks the `arguments` limits of the rule, and masks personal data in tool results (for example card numbers and national ID numbers).
4. **Token broker.** For a `Server` that has a `connection`, Ambit adds the calling person's own app token. The console refreshes the token when it expires.
5. **MCP server.** The call reaches the remote server, or the server that ToolHive runs in your cluster. Only the gateway can reach ToolHive's pods.
6. **Audit.** The call is recorded, denied calls included.

Budgets belong to the model path. They count LLM requests.

## Setup path

Requests are not the only flow. Admins change objects in the console (or by pull request, in GitOps mode). The controller `ambit-controller` turns the objects into gateway configuration. Your identity provider reports people who leave to the console by SCIM, and Ambit then stops their keys, sessions and app sign-ins.

## Next

- [Quick start](quick-start.md): create a `Model` and an `AccessRule` and make a first call.
- [Install on your cluster](install.md): the chart values for a real cluster.
