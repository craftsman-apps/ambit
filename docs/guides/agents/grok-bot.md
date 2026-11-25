---
title: Grok Bot with Ambit's MCP tools
description: Connect Grok Bot to the MCP servers that an admin put on Ambit's public front door, with a person's own agent key.
---

# Grok Bot with Ambit's MCP tools

**Request shape tested; not yet tested with a live account.** The test `test/stepplatforms.sh` sends the request that this page describes to the public front door of a test cluster. It does not use Grok Bot's own service, so how Grok Bot behaves is not verified (see [What is not verified](#what-is-not-verified)). The [public front door](../../front-door.md) and agents are experimental in 0.6.x.

Grok Bot runs on the internet, so it reaches Ambit through the [public front door](../../front-door.md), not through the company gateway. Each person connects **their own** agent: Grok Bot then acts as that person, with that person's access rules, never more.

## Before you start

1. The [public front door](../../front-door.md) is on (`frontDoor.enabled`), with its DNS name and certificate, and the servers this assistant needs are marked with `frontDoor` and acknowledged ([Choose the servers](../../front-door.md#choose-the-servers)). In this guide the MCP address is `https://mcp-public.example.com/mcp`.
2. Each person signs in to the console and makes an agent **for themselves** (person mode, platform `grok`). Admins do not make it for them.
3. The agent's **scope** lists only the servers or tools this assistant needs. A new agent has an empty scope, which means no tool. See [Agents](../../agents.md).

## Set it up

1. Make the agent and a key ([Agents](../../agents.md#create-an-agent-and-a-key)); platform `grok`. Copy the key.
2. In Grok Bot, add an MCP connector with the address `https://mcp-public.example.com/mcp`.
3. Where the connector asks for headers or an API key, enter the header **name** `Authorization` and the **value** `Bearer ambit_...` (the word `Bearer`, a space, the key). If it has only a field for an API key without a prefix, enter the header name `x-api-key` and the key alone as the value.
4. Ask Grok Bot to list its tools. You should see only the tools in the agent's scope.

## What Grok Bot sends, and what Ambit does with it

Ambit accepts the key in `Authorization: Bearer ambit_...` or in `x-api-key: ambit_...`. A key in the address (a query parameter) is **never** accepted, so do not choose a "query" option.

Check the key from your own computer before you enter it anywhere else:

```sh
curl -si https://mcp-public.example.com/mcp -X POST \
  -H 'Authorization: Bearer ambit_...' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'
```

A valid key answers `200` with a session id. A wrong key, an expired key and a key without the MCP scope answer `403`. A request without a key answers `403`. A path other than `/mcp` answers `404`. The gateway removes the key before the request reaches an MCP server.

## Keep the key safe

- Give the key a **short end date** (30 days or less) and **source ranges** if Grok Bot publishes the addresses it calls from. A key with ranges works only from those addresses.
- The key is shown once. To replace it, make a second key, enter it in Grok Bot, test, then delete the first key.
- To stop the assistant at once, delete its key or the agent. Audit shows every call with the agent's name, the platform it declared (`grok`; declared, never a proof) and the owner at the time the key was made.

## What is not verified

- **The connector's header field.** The names of the fields in Grok Bot's connector page, and whether it can send a custom header at all, are described only by third parties in the material we found. If the connector cannot send `Authorization` or `x-api-key`, it cannot use the front door in this release (OAuth sign-in is a later step).
- Which MCP protocol versions Grok Bot sends, and whether it keeps the MCP session id between requests.
- The addresses it calls from (so source ranges cannot be recommended).
