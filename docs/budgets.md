# Budgets: requests and dollars per day, requests per minute

A `Budget` limits what one identity-provider group uses per day (UTC), for the whole group and for each
person in it. It can count **requests**, **estimated dollars**, or both; whichever runs out first refuses the
request (HTTP 429) until midnight UTC. It can also limit **requests per minute**, to stop bursts (see
[Per-minute limits](#per-minute-limits)). Every gateway replica shares the same counters (envoy ratelimit and
Valkey). A group has one Budget; a second one for the same group is reported and ignored.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: eng }
spec:
  group: eng
  groupDollarsPerDay: "50"      # the whole group
  userDollarsPerDay: "5"        # each person
  userRequestsPerDay: 500       # optional: also cap requests
```

Admins can set the same fields on the console's Budgets page, which shows today's spend. Per-person dollar
limits count by the token's email, or by its subject (`sub`) when it has no email.

## How requests are counted

This section is about daily request limits. A request counts against a daily request limit when the gateway has
let it through to the model: the count goes up by
one after the answer, whatever the model answered (an error from the provider counts too). Requests the gateway
refuses are not counted: no `AccessRule` allows the model (403), a model without a price for a group with a dollar
limit (403), or a Budget limit already reached (429).

| Request | Counted? |
|---|---|
| `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, streamed or not, and `/v1/embeddings` | Yes |
| A refused request (403 or 429) | No |

## How spending is counted

Each answer costs `input tokens × input price + output tokens × output price`, from the model's price
(`Model.spec.price`, dollars per million tokens), rounded up to $0.00001. That step is also the counting unit,
so a cap can be at most **$42,949.67** a day. Amounts are **estimates from list prices**: cache discounts, batch
prices and contract prices are not applied, so spend is usually a little higher than the invoice.
A price change applies to requests that start after it; an answer still running when the price changes is
charged at the price from when it started.

| Request | Charged? |
|---|---|
| `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, streamed or not | Yes, when the answer reports token counts |
| A stream cut off part-way | For the token counts received before it stopped |
| `/v1/embeddings` | **No**: an embeddings answer has an input token count but no output token count, and the charge needs both |
| An answer without token counts | **No** |
| A streamed request that turns usage reporting off (`stream_options: {"include_usage": false}`) | Yes: the gateway turns usage reporting back on for every streamed request, so the stream ends with one extra chunk carrying the token counts (standard OpenAI behavior) |
| A refused or failed request | No (no tokens) |

## When a cap is passed

For daily limits, the gateway checks the total before each request and adds the cost (or the one request) after
the answer. So a
cap can be exceeded: every request that started while the total was not yet above the cap finishes and is charged
(for example several requests sent at the same moment), and a charge reaches the counter a moment after its
answer. Once the total is above the cap, new requests are refused.

Requests that run at the same time can go over a request limit by the number of requests in flight: each one was
checked before the others were counted. Dollar limits behave the same.

A refused request gets HTTP 429 and a `Retry-After` header: the seconds until midnight UTC for a daily limit,
until the next minute for a per-minute limit. A per-minute limit is checked first, so when both are used up the
first refusal says the rest of the minute, and the next request after that minute is refused with the seconds to
midnight. The body is an
error in the API's format: OpenAI's for `/v1/chat/completions`, `/v1/completions`, `/v1/responses` and
`/v1/embeddings` (`"type": "insufficient_quota"`, `"code": "budget_exceeded"`), Anthropic's for `/v1/messages`
(`"type": "rate_limit_error"`). The message says that a request limit or a spending limit was reached; it does not
say which one, because the gateway does not pass that on. The gateway removes
`x-ratelimit-*` headers from every response, because their numbers are the rate limit service's internal ones,
not the Budget's.

## Per-minute limits

```yaml
spec:
  group: eng
  userRequestsPerMinute: 30     # each person
  groupRequestsPerMinute: 300   # everyone in eng together
```

- **The window is a clock minute (UTC).** The count starts again at each full minute, so a client can send up to
  twice the limit across a minute boundary (the limit at 10:00:59 and again at 10:01:00).
- **A request counts when it arrives**, also when the gateway then refuses it (403, a daily limit, or this limit).
  This is different from daily request limits, which count after the answer: a per-minute limit is for bursts, and
  requests sent at the same moment must not all pass before any is counted.
- **Counted**: `/v1/chat/completions`, `/v1/messages`, `/v1/responses` and `/v1/embeddings` by members of the group.
  Not counted: `/v1/models` and MCP tool calls.
- Each field is from 1 to 1,000,000. A Budget may have only per-minute limits. Daily and per-minute limits both
  apply: any limit that is over refuses the request.
- The counts are kept in Valkey, which the chart installs without persistence. If Valkey restarts, the counts of the
  current minute (and of the current day) are lost.
- The console's Budgets page shows the per-minute limits but not their use: a minute's count is gone a minute later.

## Applying a change

A change to a Budget takes up to about a minute to apply: the rate limit service reads budgets from a mounted
ConfigMap, which Kubernetes refreshes on the kubelet's sync cycle (1 minute by default).

## Models without a price

A model without a price cannot be counted, so it is **refused (403) to every group with a dollar limit**
(so is a Model whose name has a wildcard, such as `gpt-*`, because requests name the real model);
other groups can still use it. The Models page says which groups it is refused to. A price of `0` is a price:
a free in-house model can be priced at `0` and used. If several Models share one model name with different
prices, that name counts as having no price.

## Prices

Admins edit a model's price on the Models page (Direct mode saves it; GitOps mode opens a pull request). The
page also **suggests** a price from [LiteLLM's model price list](https://github.com/BerriAI/litellm), which
nothing applies until an admin clicks **Use this price**. Self-hosted models (`spec.baseURL`) get no suggestion.

- **Built in:** each Ambit release carries a copy of the list (regenerated with `hack/prices.py`), so
  suggestions work without network access.
- **Mirror (optional):** set `prices.sourceURL` to an HTTPS copy of `model_prices_and_context_window.json`,
  for example in Artifactory, Nexus or an internal Git mirror. The console reads it at start and once a day;
  it follows no redirects, waits at most 20 seconds, reads at most 10 MiB, refuses compressed responses, and
  keeps the last good list if a read fails. Ambit does not restrict the console's outgoing traffic; if your
  cluster does, allow the mirror (the console honours `HTTPS_PROXY`).
- LiteLLM's list is MIT-licensed; its licence ships with the console (`/api/prices/licence`).
