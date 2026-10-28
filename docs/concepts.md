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
| `AccessRule` | Who may use which models and which MCP servers. Rules only grant access. Nothing is allowed by default. | `groups`, `users` or `allSignedIn`, `models`, `servers` |
| `Server` | One MCP server that clients may use. | `url` (a remote server) or `image` (run by ToolHive), `transport`, `mcpPort`, `connection`, `displayName` |
| `Budget` | A daily limit for one identity-provider group, and for each person in it. | `group`, `groupRequestsPerDay`, `groupDollarsPerDay`, `userRequestsPerDay`, `userDollarsPerDay` |
| `ModelRouter` | One model name (for example `auto`) that picks a `Model` for each request. | `model`, `default`, `classifier`, `routes` |
| `Connection` | A work app (a SaaS OAuth registration, for example GitHub) that each person signs in to with their own account. | `clientID`, `clientSecretRef`, `authorizationURL`, `tokenURL`, `scopes`, `serverURL`, `revocationURL` |

In this documentation, **MCP server** means a `Server`. **App** means a `Connection`: the OAuth registration that lets a person connect their own account to a work app. A `Server` with a `connection` field uses that app, so every call carries the calling person's own token.

### Model

`spec.model` is the exact name clients send in the request, for example `google/gemma-4-31b-it:free`. It is not the name of the `Model` object. The credentials are not in the object. `credentialsRef` names a Secret (`ambit-llm-<something>`) in Ambit's namespace, so the `Model` can be kept in Git. See [Models and providers](providers.md).

### AccessRule

A rule names its people with `groups` (groups from the `groups` claim of the identity provider), with `users` (email addresses), or with `allSignedIn: true` (everyone with a valid token). `groups` and `users` can be used together; a person who matches either one is allowed. `allSignedIn: true` cannot be combined with them. A `users` entry is an exact, lowercase email address (no wildcards, no domains). It matches the owner of a personal API key, or the `email` claim of a JWT; a token without a usable email never matches it. People are matched by a verified email. If your identity provider does not send `email_verified`, set `accessRules.usersRequireVerifiedEmail: false` only if users cannot change their own email. Prefer groups; use people for exceptions such as a pilot user or a contractor. Offboarding still refuses a listed person, and budgets still follow the groups in the person's token. A rule names `models`, `servers`, or both. Each entry in `servers` has a `name` and, optionally, `tools`: the tools of that server the people may call. Without `tools`, every tool of the server is allowed, including tools it adds later. A server entry can also carry `arguments`, which limit the values a tool may be called with. That needs `protection.enabled` (see [Install on your cluster](install.md)).

Rules are deny by default, except in audit-only mode (`access.mode: audit`, see [Turn Ambit on without blocking anyone](guides/audit-only.md)). In that mode every signed-in request to a model or an MCP tool is allowed, and every tool is listed to everyone; each audit record says whether the rules in force at that moment would have refused the request. Still enforced in that mode: sign-in, the LLM path allowlist, offboarding, budgets and the refusal of an unpriced model for a group with a dollar limit, and argument limits and tool approval.

### Server

A `Server` is either a remote MCP server (`url`) or a container image that ToolHive runs inside your cluster (`image`, pinned by digest). A server with `connection` must be a remote server, because a per-person token cannot reach a container that speaks `stdio`. Ambit also keeps the definition of each tool when it first sees it (tool pinning). A tool whose definition changes later, and a new tool, is hidden and refused until an admin approves it again. An admin can also set `resultCompression` on a `Server` to make its tool results smaller (see [Compressing tool results](#compressing-tool-results)). See [MCP servers and apps](presets.md).

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
3. **Guard.** With `protection.enabled`, `ambit-guard` refuses a tool whose definition changed (tool pinning), checks the `arguments` limits of the rule, makes the tool result smaller if the `Server` has `resultCompression`, and masks personal data in tool results (for example card numbers and national ID numbers).
4. **Token broker.** For a `Server` that has a `connection`, Ambit adds the calling person's own app token. The console refreshes the token when it expires.
5. **MCP server.** The call reaches the remote server, or the server that ToolHive runs in your cluster. Only the gateway can reach ToolHive's pods.
6. **Audit.** The call is recorded, denied calls included.

Budgets belong to the model path. They count LLM requests.

## Masking

With `protection.enabled`, `ambit-guard` masks personal data in the results of MCP tools before the client sees them. It reads every text in the result, including the structured content. It does not mask tool arguments, and it does not mask the model path.

- **Default masks.** Korean resident registration numbers and US Social Security numbers become `<NATIONAL_ID>`. Card numbers (a valid Luhn check is required) become `<CARD_NUMBER>`. Email addresses become `<EMAIL>`. These masks need no other service.
- **Presidio adds to the default masks.** If you set `protection.presidio.analyzerURL`, the guard first applies the default masks to each text, and then sends the masked text to [Microsoft Presidio](https://microsoft.github.io/presidio/). Presidio can find names, addresses and more languages. Because the default masks run first, Korean resident registration numbers are masked even when Presidio's own recognizers do not find them. Each Presidio finding is replaced by `<ENTITY_TYPE>`, for example `<PERSON>`. Presidio does not change the default placeholders.
- **Presidio is not part of Ambit.** Ambit does not install it. You run the Presidio analyzer service yourself and give its address in the `protection.presidio` values (see `charts/ambit/values.yaml`). `anonymizerURL` is no longer used. The guard sends the text of one result to the analyzer in pieces of at most 100,000 characters, up to 4 at the same time, and `protection.presidio.timeout` (default 10 s) limits all of those calls together.
- **Fail closed.** If masking fails (Presidio is down, too slow, or returns an error), the guard withholds the whole result and the call fails. The client never receives unmasked text. If the guard itself is down, the call also fails.
- **Size limit.** The guard accepts a tool result up to 32 MiB by message size. It holds a result several times while it masks it. In a test with the default memory limit (`protection.memoryLimitMiB`, 512 MiB), results of 16 MiB and 30 MiB were masked and returned, and the guard used at most 323 MiB and did not restart. If you set a lower limit, plan for about 10 times the size of the largest result plus 50 MiB. A result that is too large for the guard makes the call fail with an error; unmasked text is never returned. While the guard restarts, every MCP tool call fails.

## Compressing tool results

A tool result is sent to the model again on every later turn, like a folder the model must read again before each answer. Compression makes the folder thinner before the client stores it, so each turn costs fewer tokens. It is off unless an admin turns it on for one `Server`. It needs `protection.enabled`, because `ambit-guard` does the work.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata: { name: github }
spec:
  url: https://api.githubcopilot.com/mcp/
  resultCompression:
    mode: lossless      # off (default) | lossless | shorten
    minBytes: 8192      # default; allowed 1024 to 4194304
```

- **`off`** changes nothing. A `Server` without the field behaves as before.
- **`lossless`** removes only layout: whitespace between tokens, padding, blank lines, ANSI color codes. Every key, value, cell and line stays, in the same order. The text looks different, but the information is the same.
- **`shorten`** does what `lossless` does and also leaves out content, and says so in the result. Start with `lossless`, check the counters (below), and move one server at a time to `shorten`.
- **`minBytes`** is the size of one text in the result. A smaller text is not touched. A text larger than 4 MiB is not compressed either (it is still masked).

| Type of text | `lossless` | `shorten` also |
|---|---|---|
| JSON | Removes whitespace. An array of 5 or more objects with the same keys is written as `{"columns":[...],"rows":[[...],...]}` (the key names once). Numbers and strings are copied byte for byte. | Nothing more. |
| CSV, TSV, pipe and Markdown tables | Removes padding around cells, the Markdown separator row and blank lines. | Nothing more. |
| YAML | Removes blank lines and trailing spaces (not inside block scalars). | Removes comment lines and `managedFields` blocks. |
| Logs | Removes trailing spaces and ANSI color codes. | Removes lines that repeat an earlier line (compared without numbers). It keeps the first 20 and the last 20 lines, every WARN, ERROR, FATAL and panic line with its stack, and 2 lines around them. |
| Test output (`go test`, pytest, jest) | Removes ANSI color codes. | Removes the lines of passing tests. It keeps every failure with its message and the summary. |
| Anything else (HTML, prose, source code, base64) | Not changed. | Not changed. |

Neither mode cuts rows of JSON or tables. Nothing tells Ambit which row the model will need, and the guard cannot give the full result back later.

**Order.** The guard compresses first and masks last, so the masker always has the last word, and Presidio gets less text. The compressor never cuts inside a value and never joins digits with a space or dash, so it cannot hide personal data from the masks. While `protection.presidio.analyzerURL` is set, the guard does not write JSON as columns, because Presidio reads names better with their keys next to them.

**What the model sees in `shorten`.** Each place where content was left out has one marker, for example `[ambit: 3940 repeated lines omitted here]`, and the end of the text has one line: `[ambit: this result was shortened from 250 KB to 5 KB. Parts were left out. If you need them, call the tool again with a narrower query (filters, a smaller range, a page number).]`. The wording is fixed and holds only numbers. This is the only way to get the left-out part: a narrower call. If the tool has no filter or paging, the same shortened result comes back. In that case set the `Server` back to `lossless` or `off`.

**What is never compressed.** Results with `isError: true`, non-text content such as images, and `structuredContent` (it must keep the shape that the tool's output schema promises). If a client sends both `content` and `structuredContent` to the model, the saving is smaller.

**If compression fails.** A problem in a compressor never fails a call and never withholds a result: the guard passes the original text (masked as before). A compressor that is too slow (100 ms up to 1 MiB, 400 ms above) is abandoned, and at most 2 texts are compressed at the same time. A third text passes uncompressed. If masking fails, the result is withheld, as before.

**Counters.** The guard serves Prometheus metrics on port 8081 at `/metrics`. With `alerts.enabled` the chart adds a `PodMonitor` for them.

| Metric | Labels |
|---|---|
| `ambit_guard_compress_bytes_in_total`, `ambit_guard_compress_bytes_out_total` | `server`, `type` |
| `ambit_guard_compress_results_total` | `server`, `type`, `outcome` (`compressed`, `skipped_small`, `skipped_unknown`, `skipped_too_large`, `skipped_busy`, `no_gain`, `timeout`, `error`) |
| `ambit_guard_compress_omitted_total` | `server`, `type` (cuts made by `shorten`) |
| `ambit_guard_compress_seconds` | `type` |

Bytes saved for a server are `bytes_in` minus `bytes_out`. These are bytes, not tokens. The guard also writes one log line for each compressed text with the sizes (never the content).

## Setup path

Requests are not the only flow. Admins change objects in the console (or by pull request, in GitOps mode). The controller `ambit-controller` turns the objects into gateway configuration. Your identity provider reports people who leave to the console by SCIM, and Ambit then stops their keys, sessions and app sign-ins.

## Next

- [Quick start](quick-start.md): create a `Model` and an `AccessRule` and make a first call.
- [Install on your cluster](install.md): the chart values for a real cluster.
