---
title: Copilot Studio with Ambit's MCP tools (API key)
description: Connect a Copilot Studio agent to the MCP servers on Ambit's public front door with an API key. Each user enters their own key.
---

# Copilot Studio with Ambit's MCP tools (API key)

**Request shape tested; not yet tested with a live account.** The test `test/stepplatforms.sh` sends the request that this page describes to the public front door of a test cluster. It does not use Copilot Studio's own service, so how Copilot Studio behaves is not verified (see [What is not verified](#what-is-not-verified)). The [public front door](../../front-door.md) and agents are experimental in 0.6.x.

A Copilot Studio agent runs on Microsoft's service, so it reaches Ambit through the [public front door](../../front-door.md). Copilot Studio asks **each user** for the API key when the user first uses the connection. So each user makes **their own agent** in Ambit and enters its key. This is not delegation: every key acts for its own owner only.

## Before you start

1. The [public front door](../../front-door.md) is on (`frontDoor.enabled`), with its DNS name and certificate, and the servers this assistant needs are marked with `frontDoor` and acknowledged ([Choose the servers](../../front-door.md#choose-the-servers)). In this guide the MCP address is `https://mcp-public.example.com/mcp`.
2. Each person makes an agent **for themselves** (person mode, platform `copilot-studio`) and keeps the key private. Do not share one key between people: Audit would show one person for everyone.
3. The agent's **scope** lists only the servers or tools this assistant needs. A new agent has an empty scope, which means no tool. See [Agents](../../agents.md).

## Set it up

1. Each person makes an agent and a key, platform `copilot-studio` ([Agents](../../agents.md#create-an-agent-and-a-key)).
2. In Copilot Studio, open the agent, then **Tools**, **Add a tool**, **New tool**, **Model Context Protocol**. Use the server URL `https://mcp-public.example.com/mcp`.
3. For authentication choose **API key**, placement **Header**, and the header name `x-api-key`. Enter only the name in that field, with no `Bearer ` and no key.
4. Create the connection. When Copilot Studio asks for the key value, each user pastes their own key (`ambit_...`, without a prefix).
5. Test the tool in the agent. You should see only the tools in the agent's scope.

## What Copilot Studio sends, and what Ambit does with it

Copilot Studio sends the header that you named, with the value the user entered. With the name `x-api-key` and the key alone as the value, Ambit receives exactly `x-api-key: ambit_...`. (If you name the header `Authorization`, the user must enter `Bearer ambit_...` including the word `Bearer`.) Do not choose **Query**: Ambit never accepts a key in the address.

Check the key from your own computer before you enter it anywhere else:

```sh
curl -si https://mcp-public.example.com/mcp -X POST \
  -H 'x-api-key: ambit_...' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'
```

A valid key answers `200` with a session id. A wrong key, an expired key and a key without the MCP scope answer `403`. A request without a key answers `403`. A path other than `/mcp` answers `404`. The gateway removes the key before the request reaches an MCP server.

## Keep the key safe

- Give the key a **short end date** (30 days or less) and **source ranges** if Copilot Studio publishes the addresses it calls from. A key with ranges works only from those addresses.
- The key is shown once. To replace it, make a second key, enter it in Copilot Studio, test, then delete the first key.
- To stop the assistant at once, delete its key or the agent. Audit shows every call with the agent's name, the platform it declared (`copilot-studio`; declared, never a proof) and the owner at the time the key was made.

## What is not verified

- The exact wording and order of Copilot Studio's screens. They change; Microsoft's page [Connect your agent to an existing MCP server](https://learn.microsoft.com/microsoft-copilot-studio/mcp-add-existing-server-to-agent) is the reference.
- Whether the connection is made once for the maker or once for each user in your tenant, and who can see the key value.
- The addresses Copilot Studio calls from (so source ranges cannot be recommended).
- OAuth with dynamic client registration (Copilot Studio's other option) needs the OAuth work of a later step; this guide covers the API key only.
