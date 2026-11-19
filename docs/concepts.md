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

A rule names its people with `groups` (groups from the `groups` claim of the identity provider), with `users` (email addresses), or with `allSignedIn: true` (every person with a valid token or personal key; not [service accounts](service-accounts.md), which get only what a rule names by their group or identity). `groups` and `users` can be used together; a person who matches either one is allowed. `allSignedIn: true` cannot be combined with them. A `users` entry is an exact, lowercase email address (no wildcards, no domains). It matches the owner of a personal API key, a service account's identity (`<name>@svc.ambit.internal`), or the `email` claim of a JWT; a token without a usable email never matches it. People are matched by a verified email. If your identity provider does not send `email_verified`, set `accessRules.usersRequireVerifiedEmail: false` only if users cannot change their own email. Prefer groups; use people for exceptions such as a pilot user or a contractor. Offboarding still refuses a listed person, and budgets still follow the groups in the person's token. A rule names `models`, `servers`, or both. Each entry in `servers` has a `name` and, optionally, `tools`: the tools of that server the people may call. Without `tools`, every tool of the server is allowed, including tools it adds later. A server entry can also carry `arguments`, which limit the values a tool may be called with. That needs `protection.enabled` (see [Install on your cluster](install.md)). A limit applies to every argument whose name differs only in letter case (`Query` and `QUERY` for a limit on `query`), because MCP servers written in Go read those as the same argument. `notContains` is a text match, not a parser: it ignores letter case, treats every run of spaces, tabs and new lines as one space, and treats SQL comments (`/* ... */`) as a space, so `DROP  TABLE` and `DROP/**/TABLE` contain `DROP TABLE`. Other spellings can still pass it (for example `rm -r -f` for `rm -rf`). For free text such as SQL or shell commands, rely on `oneOf` (a list of allowed values) instead.

Rules are deny by default, except in audit-only mode (`access.mode: audit`, see [Turn Ambit on without blocking anyone](guides/audit-only.md)). In that mode every signed-in request to a model or an MCP tool is allowed, and every tool is listed to everyone; each audit record says whether the rules in force at that moment would have refused the request. Still enforced in that mode: sign-in, the LLM path allowlist, offboarding, budgets and the refusal of an unpriced model for a group with a dollar limit, and argument limits and tool approval.

### Server

A `Server` is either a remote MCP server (`url`) or a container image that ToolHive runs inside your cluster (`image`, pinned by digest). A server with `connection` is either a remote server (`url`) or an image that talks HTTP (`transport: streamable-http` or `sse`). It cannot be an image that speaks `stdio`, because ToolHive's proxy cannot pass a per-person token to a stdio process. See [Run a server from an image](presets.md#run-a-server-from-an-image). Ambit also keeps the definition of each tool when it first sees it (tool pinning). A tool whose definition changes later, and a new tool, is hidden and refused until an admin approves it again. An admin can also set `resultCompression` on a `Server` to make its tool results smaller (see [Compressing tool results](#compressing-tool-results)). See [MCP servers and apps](presets.md).

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
| Service account key | An admin creates a [service account](service-accounts.md) (`<name>@svc.ambit.internal`, one group) and its keys in the console. | CI jobs and background agents |

A personal API key carries the access of its owner, never more: the same access rules, budgets, audit records and offboarding. See [Personal API keys](api-keys.md).

## How a request is checked

Models and MCP servers use two separate paths. Both start at the same gateway (built on agentgateway) and both end in the audit log.

### Request to a model (`/v1`)

Clients call `https://<llm host>/v1/chat/completions` (and the other OpenAI-compatible and Anthropic-compatible paths).

1. **Identity.** The gateway checks the token or the personal API key. A request without valid credentials is refused.
2. **Model access.** An `AccessRule` must allow the requested model for this person. Otherwise the answer is 403. (In audit-only mode the request goes on, and the record says whether it would have been refused.)
3. **Budget.** The ratelimit service and Valkey count the request against the `Budget` of the person's group and against the person's own limit. Over the limit, the answer is 429.
4. **Router (optional).** If the requested name is a `ModelRouter`, Ambit picks one `Model` the person may use.
5. **Prompt guard (optional).** With `llm.promptGuard`, the gateway checks the prompt and records, refuses or masks what matches. See [Prompt guard](#prompt-guard).
6. **Provider.** The gateway adds the provider credentials from the Secret and forwards the request.
7. **Audit.** The call is recorded, denied calls included.

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

With `protection.enabled`, `ambit-guard` masks personal data in the results of MCP tools before the client sees them. It reads every text in the result, including the structured content: strings, numbers, object keys and links (`uri`). A masked number becomes the placeholder text, for example `"card": 4111111111111111` becomes `"card": "<CARD_NUMBER>"`, so a client that checks the result against the tool's output schema may refuse it; that is better than an unmasked card number. If two object keys mask to the same text, the result is withheld. The results of `resources/read` and `prompts/get` are masked the same way (today access rules grant tools only, so these methods are refused before they reach a server). It does not mask tool arguments, and it does not mask the model path.

- **Default masks.** Korean resident registration numbers and US Social Security numbers become `<NATIONAL_ID>`. Card numbers become `<CARD_NUMBER>`. Email addresses become `<EMAIL>`. These masks need no other service. They find these written forms:

  | Kind | Masked as it is | Masked only with a label just before it |
  |---|---|---|
  | Korean resident registration number (7th digit 1 to 8, also foreign residents; not 9 or 0, people born before 1900) | `900101-1234567` | `9001011234567`, `900101 1234567`: the date must be valid, and a word such as `RRN`, `resident`, `registration`, `주민`, `외국인` or `등록번호` must stand in the 40 characters before it (a JSON key counts) |
  | US Social Security number | `078-05-1120` | `078051120`, `078 05 1120`: it must be a valid SSN (not area 000, 666 or 900 to 999, group 00 or serial 0000), and `SSN` or `social security` must stand in the 40 characters before it |
  | Card number | 13 to 19 digits that pass the Luhn check and start with 2 to 6, written without separators or in groups (first group of 4 digits) with spaces, `-` or `.`; ASCII or full-width digits. A date, time or other number next to it does not stop the mask. | |
  | Email address | `kim@acme.com`, and `kim%40acme.com` in a link | |

  Without a label, a 13-digit or 9-digit number is not masked as a national ID: most such numbers are order numbers, Unix times or amounts. A number with a label before it is masked even when it is not an ID, for example a 9-digit order number right after an SSN in the same record. Dates, times, IP addresses, version strings and decimal numbers are not masked.
- **Presidio adds to the default masks.** If you set `protection.presidio.analyzerURL`, the guard first applies the default masks to each text, and then sends the masked text to [Microsoft Presidio](https://microsoft.github.io/presidio/). Presidio can find names, addresses and more languages. Because the default masks run first, Korean resident registration numbers are masked even when Presidio's own recognizers do not find them. Each Presidio finding is replaced by `<ENTITY_TYPE>`, for example `<PERSON>`. Presidio does not change the default placeholders. While Presidio is set, the guard does not compress JSON, because removing the spaces in JSON hides names from Presidio (see [Order](#compressing-tool-results) under Compressing tool results).
- **Presidio is not part of Ambit.** Ambit does not install it. You run the Presidio analyzer service yourself and give its address in the `protection.presidio` values (see `charts/ambit/values.yaml`). `anonymizerURL` is no longer used. The guard sends the text of one result to the analyzer in pieces of at most 100,000 characters, up to 4 at the same time, and `protection.presidio.timeout` (default 10 s) limits all of those calls together.
- **Fail closed.** If masking fails (Presidio is down, too slow, or returns an error), the guard withholds the whole result and the call fails. The client never receives unmasked text. If the guard itself is down, the call also fails.
- **What is not masked.** The gateway sends the guard only the JSON-RPC **result** of a `tools/call`. Other messages a server sends while it handles the call reach the client unmasked: a JSON-RPC **error** (`error.message` and `error.data`), log notifications (`notifications/message`), progress notifications (`notifications/progress`), and requests from the server to the client (`sampling/createMessage`, `elicitation/create`). Ambit cannot change this without a change in agentgateway (issue #333). A server that puts a customer record in an error message, for example "no customer with email ...", sends that text to the client as it is. Use only MCP servers that you trust not to put personal data in errors or notifications. The audit log does not store the text of errors (see [Compliance](compliance.md#limits)). A result that is not a JSON object cannot be checked, so the guard withholds it.
- **Size limit.** The guard accepts a tool result up to 32 MiB by message size. It holds a result several times while it masks it. In a test with the default memory limit (`protection.memoryLimitMiB`, 512 MiB), results of 16 MiB and 30 MiB were masked and returned, and the guard used at most 323 MiB and did not restart. If you set a lower limit, plan for about 10 times the size of the largest result plus 50 MiB. A result that is too large for the guard makes the call fail with an error; unmasked text is never returned. While the guard restarts, every MCP tool call fails.

## Prompt guard

The prompt guard checks the prompt of a model request before the model receives it. Think of it as a scanner at the door of the model: it looks for text you describe (a card number, a secret key, an internal code name) and records, refuses or blanks it out. It is off by default. Turn it on with the Helm value `llm.promptGuard`:

```yaml
llm:
  promptGuard:
    enabled: true
    mode: audit            # audit (default), reject or mask
    builtins: [CreditCard, Ssn, Email, PhoneNumber]   # the default; also CaSin
    patterns:              # your own regular expressions, at most 1,024 characters each
      - '(?i)ignore (all )?previous instructions'
      - 'AKIA[0-9A-Z]{16}'
    rejectMessage: "The prompt was refused by the prompt guard of this gateway."
    webhook:               # optional: your own guard, a Service in the cluster
      service: ""
      namespace: ""        # empty: the release namespace
      port: 8000
      failureMode: FailClosed
```

- **Modes.** `audit` lets every prompt through unchanged and records which ones the guard would refuse; start here and read the Audit page for a week. `reject` refuses a matching prompt with HTTP 403 and `rejectMessage` (plain text); the model is not called. `mask` replaces each match with a placeholder and sends the rest of the prompt to the model: `<CREDIT_CARD>`, `<EMAIL_ADDRESS>` and the like for the built-in patterns, `<masked>` for your own.
- **What is checked.** Requests to `/v1/chat/completions`, `/v1/messages` and `/v1/responses`, streaming requests included. The patterns read the system prompt, the messages and the tool results in the prompt; the webhook receives the system prompt and the messages. Neither reads the arguments of the tool calls in earlier turns. **Answers are not checked**: a check of a streamed answer would delay every answer. **Embeddings (`/v1/embeddings`) are not checked.**
- **Built-in patterns** come from agentgateway: `CreditCard`, `Ssn` (US Social Security number), `Email`, `PhoneNumber`, `CaSin` (Canadian Social Insurance Number). There is no built-in pattern for Korean resident registration numbers; add your own pattern.
- **Your own patterns** use the syntax of the Rust `regex` crate, the same as Go's: no look-around and no back-references. The chart refuses to install a pattern that Go cannot read, or one longer than 1,024 characters.
- **Webhook.** With `webhook.service`, the gateway also sends each prompt to `POST /request` on that Service, after the patterns ([agentgateway's guardrail webhook protocol](https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/guardrails/webhook/guardrails/)). The webhook answers pass, mask or reject itself. In `audit` mode the gateway only records its answer. `failureMode: FailClosed` refuses the prompt when the webhook does not answer (also in `audit` mode); `FailOpen` lets it through. The webhook's own reason text is not recorded by Ambit.
- **The text stays in the cluster.** Patterns run in the gateway. The webhook must be a Service in your cluster. Guards that send the prompt to a vendor (OpenAI Moderation, Amazon Bedrock Guardrails, Google Model Armor) are not supported yet: each adds a call over the internet before every model request (more time, and one more service that must be up), and each sends the prompt text to that vendor, which is a new data processor for your company even when the model itself runs in your cluster.
- **A pattern filter, not a prompt-injection detector.** It finds the text its patterns describe and nothing else. A pattern such as "ignore previous instructions" stops that exact phrase, not the many other ways to say it.
- **Audit records.** Each guarded request's record says what the guard did, as `phase:guard:action` (for example `request:regex:reject`), never the matched text. The Audit page shows **Refused by prompt guard**, **Prompt guard would refuse** (audit mode) and **Masked by prompt guard**, apart from a refusal by an access rule (also a 403).
- **One guard for every model.** The guard is one `AgentgatewayPolicy` (`ambit-llm-guard`) on the LLM route, so it covers every `Model`, `ModelRouter` and fallback. Ambit sets no guard on a single model: agentgateway would use a model's own guard instead of the route's, not both.
- **Order of the checks** (tested on kind). The access rule runs first: a person who may not use a model gets the access rule's 403, and the guard does not read the prompt. A prompt the guard refuses is **not charged** to the daily request and dollar limits of a `Budget` (they are charged after the answer, and there is none), but it **is counted** by the per-minute limits (`userRequestsPerMinute`, `groupRequestsPerMinute`), which count a request when it arrives.
- **A `ModelRouter` of type `LLM` sends the prompt to its classifier model before the guard runs.** `ambit-router` labels the prompt before the gateway picks the model, and the guard runs when the gateway calls the model. With `reject` or `mask`, the classifier model can still receive a prompt that the guard then refuses or masks. This follows from the order of agentgateway's policies; it is not tested. Use a classifier model in your cluster, or a router of type `Rules`, if that matters.

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
| JSON | Removes whitespace. While `protection.presidio.analyzerURL` is set, JSON is not changed at all (see Order below). An array of 5 or more objects with the same keys is written as `{"columns":[...],"rows":[[...],...]}` (the key names once). Numbers and strings are copied byte for byte. | Nothing more. |
| CSV, TSV, pipe and Markdown tables | Removes padding around cells, the Markdown separator row and blank lines. | Nothing more. |
| YAML | Removes blank lines and trailing spaces (not inside block scalars). | Removes comment lines and `managedFields` blocks. |
| Logs | Removes trailing spaces and ANSI color codes. | Removes lines that repeat an earlier line (compared without numbers). It keeps the first 20 and the last 20 lines, every WARN, ERROR, FATAL and panic line with its stack, and 2 lines around them. |
| Test output (`go test`, pytest, jest) | Removes ANSI color codes. | Removes the lines of passing tests. It keeps every failure with its message and the summary. |
| Anything else (HTML, prose, source code, base64) | Not changed. | Not changed. |

Neither mode cuts rows of JSON or tables. Nothing tells Ambit which row the model will need, and the guard cannot give the full result back later.

**Order.** The guard compresses first and masks last, so the masker always has the last word, and Presidio gets less text. The compressor never cuts inside a value and never joins digits with a space or dash, so it cannot hide personal data from the masks. While `protection.presidio.analyzerURL` is set, the guard does not compress JSON: the text keeps its original layout, byte for byte, and the guard counts it as `skipped_presidio` in `ambit_guard_compress_results_total`. Logs, tables, YAML and test output are compressed as before. The reason is a recall test (`make test-presidio`, Presidio analyzer 2.2.362, 900 known values per type, built-in masks first, then Presidio). Presidio finds 93.2% of the names in JSON as the tool wrote it, but only 10.0% after the spaces after `:` and `,` are removed (this is also what `lossless` writes), and 9.3% in the column form. Email addresses (100.0%) and phone numbers (94.2%) did not change. With JSON left as it is, name recall stays at 93.2% (issue #207). The column form also lowered how many addresses Presidio finds, from 31.6% to 23.4%. Without Presidio, JSON is compressed as described in the table above.

**What the model sees in `shorten`.** Each place where content was left out has one marker, for example `[ambit: 3940 repeated lines omitted here]`, and the end of the text has one line: `[ambit: this result was shortened from 250 KB to 5 KB. Parts were left out. If you need them, call the tool again with a narrower query (filters, a smaller range, a page number).]`. The wording is fixed and holds only numbers. This is the only way to get the left-out part: a narrower call. If the tool has no filter or paging, the same shortened result comes back. In that case set the `Server` back to `lossless` or `off`.

**What is never compressed.** Results with `isError: true`, non-text content such as images, and `structuredContent` (it must keep the shape that the tool's output schema promises). Which of the two a client sends to the model decides if compression helps; see [Measured savings](#measured-savings).

**If compression fails.** A problem in a compressor never fails a call and never withholds a result: the guard passes the original text (masked as before). A compressor that is too slow (100 ms up to 1 MiB, 400 ms above) is abandoned, and at most 2 texts are compressed at the same time. A third text passes uncompressed. If masking fails, the result is withheld, as before.

### Measured savings

Measured on 2026-11-02 with real clients, on a kind cluster with Ambit built from `main`. Model: `openai/gpt-4.1-mini` at OpenRouter. Clients: opencode 2.1.4 and Codex 0.159.3. The test MCP server (`test/compressq`) returns synthetic data of 11 KB to 37 KB per result (a service log, `go test -v` output, issues as JSON, rows as JSON, pods as YAML), as text only. Token numbers are the input tokens that OpenRouter reports for the requests of one session, summed. Each cell is 3 runs (n = 3). The model is not deterministic, so the range is part of the result. Run it again with `test/compressq/run.py`.

The task: call five tools one after another, then answer five questions. Input tokens per session:

| Client | Mode | Median | Range | Saving vs `off` |
|---|---|---|---|---|
| opencode | `off` | 54,998 | 54,790 to 55,036 | |
| opencode | `lossless` | 46,375 | 46,131 to 46,563 | 16% |
| opencode | `shorten` | 27,787 | 27,787 to 27,823 | 49% |
| Codex | `off` | 115,989 | 115,989 to 116,007 | |
| Codex | `lossless` | 112,662 | 112,281 to 112,848 | 3% |
| Codex | `shorten` | 80,301 | 80,283 to 89,588 | 31% |

- `lossless` saves tokens only where there is layout to remove. Here that was the two JSON results (23.7 KB to 12.7 KB, and 19.3 KB to 6.1 KB). The log, the test output, the CSV and the YAML had no layout to remove, so they did not change.
- `shorten` saved the most on the log (36.7 KB to 5.1 KB) and the test output (11.6 KB to 0.5 KB).
- Codex saves less because, in these runs, it cut every tool result larger than about 12 KB (12,021 bytes) before it sent it, also with `off`. The client had already cut such a result, so compression removes fewer tokens there.

**Which part of a result the client sends.** A tool that returns both `content[].text` and `structuredContent` (different content in the test) showed this, in 3 runs each:

| Client | What the model request holds |
|---|---|
| Codex | `structuredContent` only. The text is not sent. Compression of the text has no effect for such a tool. |
| opencode (`codemode: false` on the MCP server) | `content[].text` only. `structuredContent` is not sent. |
| opencode (default in 2.x, Code Mode) | Neither. The model writes code that calls the tool, and only what that code returns goes into the next request. Compression does not change the tokens. |

A tool that returns only text is sent as text by both clients (the token table above). Ambit compresses only `content[].text`. For a tool that also returns `structuredContent`, expect no saving with Codex.

**Answer quality.** 20 questions over the same fixtures (`test/compressq/questions.go`: counts, one row, errors in a log, failed tests, a count printed in a marker, and one question about a line that `shorten` removes), modes `off`, `lossless` and `shorten`, 3 runs each, with opencode. Correct answers in 60 runs per mode: `off` 41, `lossless` 43, `shorten` 43. The difference is within the noise of the model; `gpt-4.1-mini` miscounts some lists (for example it counts 6 or 7 ERROR lines where the log has 8) in every mode.

- No question was lost in `lossless`: every question the model answered correctly in the majority of runs in `off` was also answered correctly in `lossless`.
- `shorten` did as well as `off` on the questions about a failure (9 of 9 runs in both modes), about an error line (10 of 12 runs, `off` 9 of 12) and about a count printed in a marker (3 of 3 runs; `off` 0 of 3, because the marker states the number of omitted passing tests).
- The question about a line that `shorten` had removed was answered "not shown" in 3 of 3 runs, never with a guess. In `off` and `lossless` it was answered correctly.

Limits: one model, one task, synthetic data, 3 runs per cell, and results that fit in the client. Measure your own tools before you move a server to `shorten`. Anthropic's token counting API was not used.

**Counters.** The guard serves Prometheus metrics on port 8081 at `/metrics`. With `alerts.enabled` the chart adds a `PodMonitor` for them.

| Metric | Labels |
|---|---|
| `ambit_guard_compress_bytes_in_total`, `ambit_guard_compress_bytes_out_total` | `server`, `type` |
| `ambit_guard_compress_results_total` | `server`, `type`, `outcome` (`compressed`, `skipped_small`, `skipped_unknown`, `skipped_too_large`, `skipped_busy`, `skipped_presidio`, `no_gain`, `timeout`, `error`) |
| `ambit_guard_compress_omitted_total` | `server`, `type` (cuts made by `shorten`) |
| `ambit_guard_compress_seconds` | `type` |
| `ambit_guard_record_dropped_total` | `reason` (`send_failed`, `queue_full`, `no_groups`) |

**Compression records.** With `evidence.enabled`, the guard also writes one record to the audit trail for each text it compressed (OpenTelemetry log, sent to `ambit-otel-collector:4317`, the same collector the gateway uses). The record holds the Server name, the type, the mode, the bytes before and after, the number of cuts and the caller's groups (`ambit.kind` is `compression`). It holds no email and no content. Sending is best effort: when the collector is down or the queue (1,000 records) is full, the record is dropped, `ambit_guard_record_dropped_total` goes up and the call is not changed. Audit and Usage ignore these records.

Bytes saved for a server are `bytes_in` minus `bytes_out`. These are bytes, not tokens. The [Savings page](savings.md) turns the compression records into estimated tokens and dollars for each group. The guard also writes one log line for each compressed text with the sizes (never the content).

## Setup path

Requests are not the only flow. Admins change objects in the console (or by pull request, in GitOps mode). The controller `ambit-controller` turns the objects into gateway configuration. Your identity provider reports people who leave to the console by SCIM, and Ambit then stops their keys, sessions and app sign-ins.

## Next

- [Quick start](quick-start.md): create a `Model` and an `AccessRule` and make a first call.
- [Install on your cluster](install.md): the chart values for a real cluster.
