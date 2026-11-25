---
title: "Claude.ai shared team setup (lower assurance)"
description: "One shared Ambit key for a whole Claude.ai organization, entered as a custom header. Lower assurance: Audit shows a shared identity, never a person."
---

# Claude.ai shared team setup (lower assurance)

**Request shape tested; not yet tested with a live account.** The test `test/stepplatforms.sh` sends the request that this page describes to the public front door of a test cluster. It does not use Claude.ai's own service, so how Claude.ai behaves is not verified (see [What is not verified](#what-is-not-verified)). The [public front door](../../front-door.md) and agents are experimental in 0.6.x.

> **Lower assurance. Prefer OAuth when it is available.** This page is **not** the main way to connect Claude.ai to Ambit. A shared key is one credential for the whole organization: **Audit shows a shared identity, never a person**, and everyone in the organization who can use the connector has the same access. Use it only when you accept that, for a small, trusted team. Connecting each person with their own sign-in (OAuth) keeps every call attributed to a person; it needs a later step of this release series.

## Before you start

1. The [public front door](../../front-door.md) is on (`frontDoor.enabled`), with its DNS name and certificate, and the servers this assistant needs are marked with `frontDoor` and acknowledged ([Choose the servers](../../front-door.md#choose-the-servers)). In this guide the MCP address is `https://mcp-public.example.com/mcp`, on the HTTPS port 443. The gateway listens on `frontDoor.port` (8443 by default): your load balancer or tunnel must serve port 443 and forward it to that port, unless the platform lets you write the port in the address (`:8443`).
2. An admin makes **one agent in itself mode** (platform `claude-ai`): a service account with an owner. Give it a scope with only what the whole team may use, an end date of 30 days or less, and **source ranges** with the addresses that Anthropic publishes for its connectors.
3. The agent's **scope** lists only the servers or tools this assistant needs. A new agent has an empty scope, which means no tool. See [Agents](../../agents.md).

## Set it up

1. An admin makes the agent and one key, platform `claude-ai`, end date 30 days or less, source ranges = Anthropic's published addresses ([Agents](../../agents.md#create-an-agent-and-a-key-in-the-api)).
2. In the organization's Claude.ai settings, add a custom connector with the address `https://mcp-public.example.com/mcp` and, where the setup allows a static header, the header `Authorization: Bearer ambit_...`.
3. Choose no sign-in for the connector (**No sign-in**): when Claude's own OAuth is on, Claude uses the `Authorization` header for its own token. Screens differ; check Anthropic's [connector authentication](https://claude.com/docs/connectors/custom/add-unlisted#choose-authentication-settings) page.
4. Claude.ai calls from the internet on port 443: the front door must answer on 443 (see Before you start).
5. Tell the team that calls are recorded under the shared agent, not under their names.

## What Claude.ai sends, and what Ambit does with it

Claude.ai's servers call Ambit with the header you configured. Ambit receives `Authorization: Bearer ambit_...`, the same for every person of the organization.

Check the key from your own computer before you enter it anywhere else (if the key has source ranges, make a second, short-lived key **without** ranges for this check and delete it afterwards: a key with ranges is refused from your computer):

```sh
curl -si https://mcp-public.example.com/mcp -X POST \
  -H 'Authorization: Bearer ambit_...' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'
```

A valid key answers `200` with a session id. A wrong key, an expired key and a key without the MCP scope answer `403`. A request without a key answers `403`. A path other than `/mcp` answers `404`. The gateway removes the key before the request reaches an MCP server.

## Keep the key safe

- Give the key the **shortest end date** the console offers (30 days) and **source ranges** if Claude.ai publishes the addresses it calls from. A key with ranges works only from those addresses.
- The key is shown once. To replace it, make a second key, enter it in Claude.ai, test, then delete the first key.
- To stop the assistant, delete its key or the agent: the gateway refuses a deleted key within seconds (measured: under half a second for keys; see [Agents](../../agents.md)). If deleting the agent answers 503, send the request again. Audit shows every call with the agent's name, the platform it declared (`claude-ai`; declared, never a proof) and the owner at the time the key was made.

## What is not verified

- **A header field for a custom connector is a beta for a limited set of organizations.** Whether your organization has it, and where, is Anthropic's decision and can change.
- The addresses Anthropic calls from, and how long they stay valid. Read them from Anthropic's page when you set the ranges, and again when calls start to fail with 403.
- How Claude.ai stores the header.
