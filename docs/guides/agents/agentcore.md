---
title: "AWS AgentCore Gateway with Ambit's MCP tools"
description: "Add Ambit's public front door as an MCP server target of an Amazon Bedrock AgentCore Gateway, with an API key."
---

# AWS AgentCore Gateway with Ambit's MCP tools

**Request shape tested; not yet tested with a live account.** The test `test/stepplatforms.sh` sends the request that this page describes to the public front door of a test cluster. It does not use AWS AgentCore's own service, so how AWS AgentCore behaves is not verified (see [What is not verified](#what-is-not-verified)). The [public front door](../../front-door.md) and agents are experimental in 0.6.x.

An AgentCore Gateway runs in AWS, so it reaches Ambit through the [public front door](../../front-door.md). The gateway is one shared service of your team, so it uses **one agent of its own** (itself mode): Audit shows a service account, never a person.

## Before you start

1. The [public front door](../../front-door.md) is on (`frontDoor.enabled`), with its DNS name and certificate, and the servers this assistant needs are marked with `frontDoor` and acknowledged ([Choose the servers](../../front-door.md#choose-the-servers)). In this guide the MCP address is `https://mcp-public.example.com/mcp`, on the HTTPS port 443. The gateway listens on `frontDoor.port` (8443 by default): your load balancer or tunnel must serve port 443 and forward it to that port, unless the platform lets you write the port in the address (`:8443`).
2. An admin makes an agent in **itself mode** (platform `agentcore`): it is a [service account](../../service-accounts.md) with an owner (a person or a group). Give it the narrowest scope that the gateway's users need.
3. The agent's **scope** lists only the servers or tools this assistant needs. A new agent has an empty scope, which means no tool. See [Agents](../../agents.md).

## Set it up

1. An admin makes the agent and a key, platform `agentcore`, with an end date of 90 days or less ([Agents](../../agents.md#create-an-agent-and-a-key)).
2. Store the key in AgentCore as an **API key credential provider** (outbound authorization).
3. Add a gateway target of the type **MCP server** with the endpoint `https://mcp-public.example.com/mcp`.
4. For the outbound authorization of the target choose **API key**: credential location **Header**, parameter name `Authorization`, prefix `Bearer`. Set all three explicitly; AWS documents different defaults in different places.
5. Synchronize the target and list its tools through the gateway. You should see only the tools in the agent's scope.

## What AWS AgentCore sends, and what Ambit does with it

AgentCore adds the prefix and a space to the key and sends it in the header you named: with `Authorization` and `Bearer`, Ambit receives `Authorization: Bearer ambit_...`. A location of **Query parameter** does not work with Ambit.

Check the key from your own computer before you enter it anywhere else (if the key has source ranges, make a second, short-lived key **without** ranges for this check and delete it afterwards: a key with ranges is refused from your computer):

```sh
curl -si https://mcp-public.example.com/mcp -X POST \
  -H 'Authorization: Bearer ambit_...' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}'
```

A valid key answers `200` with a session id. A wrong key, an expired key and a key without the MCP scope answer `403`. A request without a key answers `403`. A path other than `/mcp` answers `404`. The gateway removes the key before the request reaches an MCP server.

## Keep the key safe

- Give the key the **shortest end date** the console offers (30 days) and **source ranges** if AWS AgentCore publishes the addresses it calls from. A key with ranges works only from those addresses.
- The key is shown once. To replace it, make a second key, enter it in AWS AgentCore, test, then delete the first key.
- To stop the assistant, delete its key or the agent: the gateway refuses a deleted key within seconds (measured: under half a second for keys; see [Agents](../../agents.md)). If deleting the agent answers 503, send the request again. Audit shows every call with the agent's name, the platform it declared (`agentcore`; declared, never a proof) and the owner at the time the key was made.

## What is not verified

- AWS's documentation ([Set up outbound authorization for your gateway](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/gateway-outbound-auth.html)) is the reference; its defaults for the credential location and the prefix differ between pages, so this guide says to set them yourself.
- Which target types and regions support API key outbound authorization for an MCP server target in your account.
- The addresses AgentCore calls from. If AWS gives your gateway fixed addresses, enter them as the key's source ranges.
- OAuth client credentials (the usual AgentCore option) are not part of this release; they are planned for a later one. Until then, this guide's API key is the way.
