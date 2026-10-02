# Budgets: requests and dollars per day

A `Budget` limits what one identity-provider group uses per day (UTC), for the whole group and for each
person in it. It can count **requests**, **estimated dollars**, or both; whichever runs out first refuses the
request (HTTP 429) until midnight UTC. Every gateway replica shares the same counters (envoy ratelimit and
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

Admins can set the same fields on the console's Budgets page, which shows today's spend.

## How spending is counted

Each answer costs `input tokens × input price + output tokens × output price`, from the model's price
(`Model.spec.price`, dollars per million tokens), rounded up to $0.00001. That step is also the counting unit,
so a cap can be at most **$42,949.67** a day. Amounts are **estimates from list prices**: cache discounts, batch
prices and contract prices are not applied, so spend is usually a little higher than the invoice.

| Request | Charged? |
|---|---|
| `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, streamed or not | Yes, when the answer reports token counts |
| A stream cut off part-way | For the token counts received before it stopped |
| `/v1/embeddings` | **No** (agentgateway v1.5.0 does not run the charge step for embeddings) |
| An answer without token counts | **No** |
| A refused or failed request | No (no tokens) |

## When a cap is passed

The gateway checks the total before each request and adds the cost after the answer. So a cap can be
exceeded: every request that started while the total was not yet above the cap finishes and is charged
(for example several requests sent at the same moment), and a charge reaches the counter a moment after its
answer. Once the total is above the cap, new requests are refused.

## Models without a price

A model without a price cannot be counted, so it is **refused (403) to every group with a dollar limit**;
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
