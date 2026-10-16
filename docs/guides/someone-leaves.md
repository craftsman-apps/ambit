---
title: Offboard someone who leaves
description: Connect your identity provider over SCIM, deactivate a person, and see their keys, sessions and app grants end, with a record as proof.
---

# Offboard someone who leaves

**What you get:** when your identity provider deactivates a person, Ambit stops their access within seconds. It deletes their personal API keys, ends their console sessions, blocks tokens that are still valid and revokes their connected apps at the vendor. It keeps a record of what it did.

## Before you start

You need:

- the cluster from the [Quick start](../quick-start.md) with the people of [Different models for different teams](models-per-team.md). This guide offboards Dan, who is in `engineering`;
- the model rule `engineering-models` and the tool rule `engineering-time` ([Give an agent tools and apps](mcp-apps.md)), so that Dan has something to lose;
- no budgets that block Dan. If you did [Set spending limits](spending-limits.md), delete its budgets first and wait a minute:

  ```sh
  kubectl -n ambit-system delete budgets.ambitproject.dev engineering sales
  ```

- two personal API keys for Dan, made in the console as in [Connect Claude Code, Codex, opencode, Cursor and SDKs](coding-tools.md): one with the scope **LLM**, in `DAN_KEY`, and one with the scope **MCP**, in `DAN_MCP_KEY`. Keep Dan signed in to the console in a browser. Alice also needs a key, in `ALICE_KEY`.

In real use, your identity provider (Okta, Entra ID) sends the SCIM requests. In this guide you send them yourself with `curl`, so you can see each step.

## 1. Find the SCIM address and token

In the console, open **Settings** and then **Offboarding**. The page shows the base URL of the SCIM endpoint and a bearer token behind **Show token**. In your identity provider, add a SCIM app with this URL and token, and turn on **Deactivate users**.

For this guide, read the token from the cluster. The command puts it in a variable without printing it:

```sh
SCIM=http://console.localtest.me:8080/scim/v2
SCIM_TOKEN=$(kubectl -n ambit-system get secret ambit-console-keys -o jsonpath='{.data.scim-token}' | base64 -d)
```

The endpoint refuses requests without the token:

```sh
curl -s -o /dev/null -w 'HTTP %{http_code}\n' $SCIM/Users
```

```text
HTTP 401
```

## 2. Provision Dan

Your identity provider creates a user in Ambit before it can deactivate one. Create Dan the way the provider does:

```sh
curl -s -X POST $SCIM/Users -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json' \
  -d '{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"dan@acme.test","active":true,"emails":[{"value":"dan@acme.test","primary":true}]}' | tee dan-scim.json
ID=$(sed -E 's/.*"id":"([^"]+)".*/\1/' dan-scim.json)
```

The answer has `"active":true` and an `id`. The address in `userName` and `emails` must be the address Dan signs in with.

## Check that Dan has access

Before you offboard Dan, check that his keys and his session work. The function `mcp` is the one from [Give an agent tools and apps](mcp-apps.md).

```sh
curl -s -o /dev/null -w 'Dan LLM key: HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $DAN_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"hi"}]}'
mcp "$DAN_MCP_KEY" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

The first command prints `Dan LLM key: HTTP 200`. The second lists the tool `time_get_current_time`. In the console, **Settings** and **Keys** list Dan's keys as **Active**, and Dan's browser is signed in.

## 3. Deactivate Dan

```sh
curl -s -w '\nHTTP %{http_code}\n' -X PATCH $SCIM/Users/$ID -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json' \
  -d '{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}'
```

The answer has `"active":false` and ends with `HTTP 200`. Deleting the user in the identity provider (`DELETE $SCIM/Users/$ID`) has the same effect.

## Check it works

Run the same checks as before. The gateway needs a few seconds to learn about the block, so repeat a command if it still shows 200.

```sh
curl -s -o /dev/null -w 'Dan LLM key: HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $DAN_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"hi"}]}'
mcp "$DAN_MCP_KEY" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

```text
Dan LLM key: HTTP 403
authorization failed
HTTP 403
```

Dan's console session ended. Reload the console in his browser, and it shows the sign-in page. The console API answers `401` for his session cookie. His keys are deleted, not only blocked: as an admin, open **Settings** and **Keys** and see that Dan has none. Alice is not affected:

```sh
curl -s -o /dev/null -w 'Alice LLM key: HTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $ALICE_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"hi"}]}'
```

```text
Alice LLM key: HTTP 200
```

### The record

Ambit keeps a record of each offboarding. As an admin, open **Settings** and **Offboarding**, or read the same data from the console API. The record for Dan looks like this. Your times and counts are different:

```json
{
  "email": "dan@acme.test",
  "at": "2026-10-13T08:20:07.193204178Z",
  "trigger": "scim PATCH active=false",
  "grants": [],
  "sessionsEnded": 2,
  "gatewayBlocked": true,
  "keysDeleted": 3,
  "completeSuccess": true
}
```

- `sessionsEnded`: the console sessions that were ended.
- `gatewayBlocked`: the gateways now refuse the person's email, even for a token that has not expired.
- `keysDeleted`: the personal API keys that were deleted.
- `grants`: one line for each connected app, with `revokedAtProvider` set to `true` when the vendor confirmed the revocation. The list is empty here because Dan connected no app. A revocation that a vendor does not confirm shows its error and marks the record `completeSuccess: false`. Which vendors truly revoke is in [Offboarding](../offboarding.md).

## If the person comes back

Reactivating the user in the identity provider lifts the block. The old keys and app grants stay deleted. The person signs in and creates new keys.

While a user is deactivated, the console refuses to make keys for that person with `this person is offboarded`.

## Related

- [Offboarding](../offboarding.md): which vendors truly revoke a person's grants.
- [Personal API keys](../api-keys.md): how keys follow the identity, and what offboarding does to them.
- [Give an agent tools and apps](mcp-apps.md): connected apps.
