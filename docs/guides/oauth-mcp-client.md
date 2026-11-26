---
title: Connect an OAuth MCP client to Ambit
description: The requests that an MCP client (or a hosted platform) sends to connect to Ambit's public MCP address with OAuth: discovery, registration, sign-in, token, refresh and revocation. Standards only, no named platform. Experimental.
---

# Connect an OAuth MCP client to Ambit

> **Experimental in 0.6.x.** The sign-in host, its endpoints and the token claims may change in 0.7.0, with an upgrade note
> ([OAuth sign-in](../oauth.md)). This page describes standards-based OAuth for MCP clients. **No named platform has been
> tested with a live account.** Every request on this page was sent to a test installation on kind, as written here (see
> [What was tested](#what-was-tested)).

**Who this is for.** You build or set up an MCP client, or a hosted platform, that cannot send a fixed Ambit API key. It signs
the person in with the company account, and then calls Ambit's public MCP address with a short-lived token. If your platform
can send a key in a header, use [a key](../front-door.md) instead: it is simpler.

**What you need from the Ambit admin.** Two addresses, and a decision about the client:

| You need | Example in this page | Where the admin finds it |
|---|---|---|
| The **MCP address** | `https://mcp.example.com/mcp` | `frontDoor.host` + `/mcp` |
| The **sign-in host** (the OAuth issuer) | `https://auth.example.com` | `frontDoor.authHost` |
| A **client**: either the admin creates one in Hydra and gives you its `client_id` (a public client, PKCE, no secret), or your client **registers itself** (below) | `CLIENT_ID` | [OAuth sign-in, "Registering clients"](../oauth.md#registering-clients) |

You never need to know the sign-in host in advance: a call to the MCP address without a token tells you where to go (step 1).

## The whole flow in one list

1. Call the MCP address without a token: **401** with a pointer to the resource metadata.
2. Read the resource metadata: it names the authorization server.
3. Read the authorization server metadata: it lists the endpoints.
4. Register a client (only if you have none).
5. Send the person's browser to the authorization endpoint. The person signs in at the company and decides what to offer.
6. Receive the code at your redirect address. Check `state` and `iss`.
7. Exchange the code for tokens.
8. Call the MCP address with the access token.
9. Refresh before the access token ends (every 15 minutes). Keep the newest refresh token.
10. When the person disconnects, revoke.

## 1. Call without a token

<!-- shape: unauthenticated -->
```
curl -i -X POST https://mcp.example.com/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"my-client","version":"1"}}}'
```

```
HTTP/2 401
www-authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
```

Take the address from `resource_metadata`. A **stopped or expired token gets the same answer**: start again at step 2 only if a
refresh (step 9) fails.

## 2. Resource metadata (RFC 9728)

<!-- shape: protected-resource -->
```
curl https://mcp.example.com/.well-known/oauth-protected-resource/mcp
```

```json
{
  "resource": "https://mcp.example.com/mcp",
  "authorization_servers": ["https://auth.example.com"],
  "bearer_methods_supported": ["header"],
  "scopes_supported": ["mcp", "offline_access"]
}
```

`resource` is the exact value you must send as the `resource` parameter later. There is one authorization server.

## 3. Authorization server metadata (RFC 8414)

<!-- shape: as-metadata -->
```
curl https://auth.example.com/.well-known/oauth-authorization-server
```

The answer lists `authorization_endpoint` (`/oauth2/auth`), `token_endpoint` (`/oauth2/token`), `revocation_endpoint`
(`/oauth2/revoke`), `jwks_uri`, and `registration_endpoint` (`/register`, only when the admin allows registration). It also
says what Ambit accepts, and nothing else works:

| Field | Value |
|---|---|
| `response_types_supported` | `["code"]` |
| `response_modes_supported` | `["query"]` (no `fragment`, no `form_post`) |
| `grant_types_supported` | `["authorization_code", "refresh_token"]` |
| `code_challenge_methods_supported` | `["S256"]` (PKCE is required; `plain` is refused) |
| `token_endpoint_auth_methods_supported` | `none` (a public client with PKCE) and `client_secret_basic` |
| `scopes_supported` | `["mcp", "offline_access"]` |
| `authorization_response_iss_parameter_supported` | `true` (RFC 9207: **check `iss`**, step 6) |

## 4. Register a client (RFC 7591)

Skip this step when the admin gave you a `client_id`. Registration works for **redirect addresses on a list that the admin
keeps** (by default `claude.ai` and `chatgpt.com`) and for native clients on a loopback address.

<!-- shape: register -->
```
curl -X POST https://auth.example.com/register -H 'Content-Type: application/json' -d '{
  "client_name": "My MCP client",
  "redirect_uris": ["https://chatgpt.com/connector_platform_oauth_redirect"],
  "grant_types": ["authorization_code", "refresh_token"],
  "response_types": ["code"],
  "token_endpoint_auth_method": "none"
}'
```

```json
{"client_id": "…", "client_id_issued_at": 1764000000, "client_name": "My MCP client",
 "redirect_uris": ["https://chatgpt.com/connector_platform_oauth_redirect"],
 "grant_types": ["authorization_code", "refresh_token"], "response_types": ["code"],
 "token_endpoint_auth_method": "none", "scope": "mcp offline_access", "application_type": "web"}
```

The rules, all checked: 1 to 5 redirect addresses; `https` on a listed host (exact name, port 443, no user name, no fragment) or
`http` on `localhost`, `127.0.0.1` or `[::1]`; a public client only (no secret is issued); no other grant type or scope. The
person later sees `client_name` on the consent page, so use a name they know. A client that nobody uses for 30 days is deleted:
register again when you get `invalid_client`.

<!-- shape: register-refused -->
```
curl -X POST https://auth.example.com/register -H 'Content-Type: application/json' \
  -d '{"client_name":"My MCP client","redirect_uris":["https://client.example.org/callback"]}'
```

```json
{"error": "invalid_client_metadata", "error_description": "…"}
```

HTTP 400. A refusal because of the limits is HTTP 429 (see [Limits](#limits)).

## 5. Send the person's browser to the authorization endpoint

Make a PKCE pair: `code_verifier` is 43 to 128 random characters of `A-Z a-z 0-9 - . _ ~`;
`code_challenge` is `BASE64URL(SHA256(code_verifier))` without padding. Keep the verifier and a random `state`.

<!-- shape: authorize -->
```
https://auth.example.com/oauth2/auth
  ?response_type=code
  &client_id=CLIENT_ID
  &redirect_uri=https%3A%2F%2Fchatgpt.com%2Fconnector_platform_oauth_redirect
  &code_challenge=CODE_CHALLENGE
  &code_challenge_method=S256
  &state=STATE
  &resource=https%3A%2F%2Fmcp.example.com%2Fmcp
  &scope=mcp%20offline_access
```

(One line in practice.) Open it in the person's browser. The person then sees, in this order: the company's own sign-in page (the
person's identity provider), and Ambit's **consent page**: the name of your client, where it redirects, and the list of servers
the person may offer. **Nothing is ticked.** The person ticks what to offer and chooses Allow or Deny. A connection with
nothing ticked is allowed and can call no tool.

Ambit refuses the request, with HTTP 400 and no redirect, when:

<!-- shape: authorize-refused -->

| Mistake | Answer |
|---|---|
| no `code_challenge`, or `code_challenge_method` is not `S256` | `{"error":"invalid_request","error_description":"PKCE with code_challenge_method=S256 is required"}` |
| `resource` missing, repeated, or not the MCP address of step 2 | `{"error":"invalid_request","error_description":"invalid_target: the resource parameter must be given once, and be …"}` |
| `response_type` is not `code`, `response_mode` is not `query`, a parameter is repeated, `request` or `request_uri` is sent | `invalid_request` |
| a scope other than `mcp` and `offline_access` | `invalid_request` (`invalid_scope`) |

An unknown `client_id` or a `redirect_uri` that is not registered exactly is shown to the person as a page, never redirected.

## 6. Receive the answer

The browser comes back to your `redirect_uri`.

<!-- shape: authorize-response -->
```
https://chatgpt.com/connector_platform_oauth_redirect?code=CODE&state=STATE&iss=https%3A%2F%2Fauth.example.com
```

Keep each sign-in as one transaction: its own random `state`, used once, stored together with its PKCE verifier. When the browser returns, look up the transaction by `state`, and drop
it whatever the outcome (an old Deny that arrives late must not end a newer sign-in). Check, before you use the code: `state` is the one you sent, and **`iss` equals the issuer** of step 3
(`https://auth.example.com`, exactly). A different `iss` means the answer came from another authorization server: drop it.
The code lives one minute and works once.

If the person chose Deny, or the company refused the sign-in (the same checks of `state` and `iss` apply to this answer, exactly one of each):

<!-- shape: authorize-denied -->
```
https://chatgpt.com/connector_platform_oauth_redirect?error=access_denied&state=STATE&iss=https%3A%2F%2Fauth.example.com
```

## 7. Exchange the code for tokens

<!-- shape: token-code -->
```
curl -X POST https://auth.example.com/oauth2/token \
  -d grant_type=authorization_code \
  -d code=CODE \
  -d redirect_uri=https://chatgpt.com/connector_platform_oauth_redirect \
  -d client_id=CLIENT_ID \
  -d code_verifier=CODE_VERIFIER \
  -d resource=https://mcp.example.com/mcp
```

```json
{"access_token": "eyJ…", "token_type": "bearer", "expires_in": 899, "refresh_token": "ory_rt_…", "scope": "mcp offline_access"}
```

The access token is a signed JWT whose `aud` is the MCP address; it is valid for **15 minutes** (`expires_in`). Store the refresh
token as a secret. A code that was used, expired, or sent with another verifier or redirect address is answered like this. **Never send a code twice:** when a code is used a second time, the server also revokes the tokens that its first use issued (RFC 6749, 4.1.2), and the connection ends.

<!-- shape: token-error -->
```json
{"error": "invalid_grant", "error_description": "…"}
```

HTTP 400.

## 8. Call the MCP address

<!-- shape: mcp-call -->
```
curl -X POST https://mcp.example.com/mcp \
  -H 'Authorization: Bearer ACCESS_TOKEN' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

(After `initialize` and `notifications/initialized` in the same MCP session, as for any MCP client.) The tool list holds **only what the
person ticked**, and only what the person's access rules allow, checked on every call. A tool outside that list is not
callable. A token that is wrong, expired, changed, or stopped gets the 401 of step 1:

<!-- shape: invalid-token -->
```
HTTP/2 401
www-authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"
```

## 9. Refresh

Refresh when the access token has a minute left, or after a 401.

<!-- shape: refresh -->
```
curl -X POST https://auth.example.com/oauth2/token \
  -d grant_type=refresh_token \
  -d refresh_token=REFRESH_TOKEN \
  -d client_id=CLIENT_ID
```

Do not send `resource` on a refresh: the audience of the new token stays the MCP address of the connection.

The answer has the shape of step 7, with a **new access token and a new refresh token**. The refresh token **rotates**: store the new
one before you do anything else, and never send the old one again. **Refresh one connection from one place at a time** (a lock per connection): two refreshes at once, or a retry after an answer
that you did not get (a proxy that answered 502 after the service had already rotated the token), spend the same token twice, and the second one is refused. If a refresh ends in an answer that you
did not see, do not send the same token again at once: read whether the connection still works with the newest access token you have, and start the sign-in again if it does not.

<!-- shape: refresh-reused -->
```json
{"error": "invalid_grant", "error_description": "…"}
```

HTTP 400: this is what an old (already used) refresh token gets. A connection that Ambit ended on purpose (the person or an admin stopped it, the person left the company, the grant is older than the
limit) answers **`access_denied`** instead. When a refresh answers `invalid_grant` or `access_denied`, the connection is over
(the person disconnected it, an admin stopped it, the person left the company, the token was spent, or the grant is older than the
limit that the admin set, 30 days by default): **send the person through step 5 again.** A refresh that answers 5xx or
`temporarily_unavailable` is a failure of the service: try again later with the same token.

## 10. Revoke (RFC 7009)

<!-- shape: revoke -->
```
curl -X POST https://auth.example.com/oauth2/revoke \
  -d token=REFRESH_TOKEN -d token_type_hint=refresh_token -d client_id=CLIENT_ID
```

HTTP 200, empty answer. The refresh token is dead at once. Access tokens that were already issued work until they end (at most 15
minutes) unless an admin stops the connection in Ambit, which the gateway applies within seconds.

## The keys (optional)

A client that checks the access token itself can read the signing keys. They change only in an emergency; fetch them again when a
token names a key ID that you do not know.

<!-- shape: jwks -->
```
curl https://auth.example.com/.well-known/jwks.json
```

The answer is `{"keys": [...]}` with RSA public keys (`alg` `RS256`, a `kid` in every key).

## Limits

Ambit counts requests **per address of the caller**, as its gateway sees it, in separate buckets (docs: [Rate limits](../front-door.md#rate-limits)).
Over a limit the answer is **HTTP 429**: wait and try again; do not retry in a loop. The defaults:

| Bucket | Requests | Limit per address |
|---|---|---|
| discovery | the metadata documents, the key list, a call without a token | 600 a minute |
| sign-in | `/oauth2/auth`, `/login`, `/callback`, `/consent`, `/error` (about 7 for one sign-in) | 300 a minute (about 40 sign-ins) |
| token | `/oauth2/token`, `/oauth2/revoke` | 600 a minute |
| registration | `/register` | 10 an hour |
| MCP calls | calls with a token or a key | 120 a minute |

A hosted platform that serves many people from a few addresses must tell the Ambit admin: the admin raises the limits for those
addresses' sake (the buckets are values of the chart). Refresh each connection only when needed: a platform with 3,000 connections
refreshes about 200 times a minute.

## What Ambit does not support

Implicit and password grants, client credentials, device flow, `response_mode=fragment` or `form_post`, request objects (`request`,
`request_uri`), `plain` PKCE, client secrets for registered clients, Client ID Metadata Documents, and DPoP. Dynamic
registration does not work for redirect addresses that are not on the admin's list.

## If it does not work

| You see | Likely reason |
|---|---|
| 400 `invalid_request` at the authorization endpoint | a parameter of step 5 is wrong or missing: read `error_description` |
| the person sees "The sign-in request cannot be completed" | `client_id` unknown (it was deleted after 30 days of no use) or `redirect_uri` not exactly as registered |
| the person is refused after signing in at the company | the account has no verified email, or the person left the company |
| 401 at the MCP address with a fresh token | the connection was stopped, or the person is offboarded: refresh once, then start over |
| `invalid_grant` at the token endpoint | the code or refresh token was already used, expired, or revoked; or the verifier or redirect address differs from the request |
| 429 | a [limit](#limits) |
| 503 at the token endpoint | the database of the sign-in service is down: tokens that you already have keep working for up to 15 minutes; try again later |

## What was tested

Every request above (the shapes marked in this page) is sent by `test/oauthtest/shapes.py` to a test installation on kind, with a
test identity provider in place of the company's, in the continuous integration test of OAuth (`test/stepoauth.sh`, check 20). The
test fails when this page names a shape that it does not send. **Not tested:** any named platform or client product, a real
company identity provider, an installation on the internet. A platform may add steps of its own that this page does not know.
