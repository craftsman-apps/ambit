---
title: "Cloudflare Agents with Ambit's MCP tools"
description: "Connect an agent built with the Cloudflare Agents SDK to the MCP servers on Ambit's public front door, with an API key in a header."
---

# Cloudflare Agents with Ambit's MCP tools

**Request shape tested; not yet tested with a live account.** The test `test/stepplatforms.sh` sends the request that this page describes to the public front door of a test cluster. It does not use Cloudflare Agents's own service, so how Cloudflare Agents behaves is not verified (see [What is not verified](#what-is-not-verified)). The [public front door](../../front-door.md) and agents are experimental in 0.6.x.

An agent built with the Cloudflare Agents SDK runs on Cloudflare Workers, so it reaches Ambit through the [public front door](../../front-door.md). A worker that works alone (a scheduled job) uses an agent in **itself mode**; a worker that acts for one person uses that person's own agent in **person mode**.

## Before you start

1. The [public front door](../../front-door.md) is on (`frontDoor.enabled`), with its DNS name and certificate, and the servers this assistant needs are marked with `frontDoor` and acknowledged ([Choose the servers](../../front-door.md#choose-the-servers)). In this guide the MCP address is `https://mcp-public.example.com/mcp`, on the HTTPS port 443. The gateway listens on `frontDoor.port` (8443 by default): your load balancer or tunnel must serve port 443 and forward it to that port, unless the platform lets you write the port in the address (`:8443`).
2. An admin makes an itself-mode agent for an autonomous worker (platform `cloudflare`), or the person makes their own person-mode agent. Use the narrowest scope and the shortest end date that work.
3. The agent's **scope** lists only the servers or tools this assistant needs. A new agent has an empty scope, which means no tool. See [Agents](../../agents.md).

## Set it up

1. Make the agent and a key, platform `cloudflare` ([Agents](../../agents.md#create-an-agent-and-a-key-in-the-api)).
2. Store the key as a **secret** of the worker (`wrangler secret put AMBIT_KEY`), never in the code.
3. In the agent, connect with the key in the transport headers, with the transport type `streamable-http`:

   ```ts
   await this.addMcpServer("ambit", "https://mcp-public.example.com/mcp", {
     transport: { type: "streamable-http", headers: { Authorization: `Bearer ${this.env.AMBIT_KEY}` } },
   });
   ```

4. List the tools from the agent. You should see only the tools in the agent's scope.

## What Cloudflare Agents sends, and what Ambit does with it

The SDK sends the headers you pass on every request of the transport, so Ambit receives `Authorization: Bearer ambit_...`.

Check the key from your own computer before you enter it anywhere else (if the key has source ranges, make a second, short-lived key **without** ranges for this check and delete it afterwards: a key with ranges is refused from your computer):

```sh
curl -si https://mcp-public.example.com/mcp -X POST \
  -H 'Authorization: Bearer ambit_...' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'
```

A valid key answers `200` with a session id. A wrong key, an expired key and a key without the MCP scope answer `403`. A request without a key answers `403`. A path other than `/mcp` answers `404`. The gateway removes the key before the request reaches an MCP server.

## Keep the key safe

- Give the key the **shortest end date** the console offers (30 days) and **source ranges** if Cloudflare Agents publishes the addresses it calls from. A key with ranges works only from those addresses.
- The key is shown once. To replace it, make a second key, enter it in Cloudflare Agents, test, then delete the first key.
- To stop the assistant, delete its key or the agent: the gateway refuses a deleted key within seconds (measured: under half a second for keys; see [Agents](../../agents.md)). If deleting the agent answers 503, send the request again. Audit shows every call with the agent's name, the platform it declared (`cloudflare`; declared, never a proof) and the owner at the time the key was made.

## What is not verified

- The SDK's option names change between versions (see Cloudflare's [MCP client API](https://developers.cloudflare.com/agents/model-context-protocol/apis/client-api/)); check the version you installed.
- **Cloudflare stores the headers of an MCP server connection in the agent's Durable Object storage, in plain text** (Cloudflare's documentation). Anyone who can read that storage can read the key. So: a short end date (30 days or less), source ranges where you know the addresses your workers call from, a narrow scope, and one key per worker.
- Which transport the SDK chooses when you do not name one.
- **Replacing the key.** Cloudflare keeps the header in the stored connection: changing the worker secret alone does not change it. Remove the MCP server from the agent and add it again with the new key, then delete the old key.
