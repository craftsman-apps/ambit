# Savings

The **Savings** page (Evidence → Savings, admins only) shows what Ambit saved in the last 30 days, for each group, server and router. It has two sources:

| Source | What is saved | How it is measured |
|---|---|---|
| [Tool result compression](concepts.md#compressing-tool-results) | Tokens that a model does not have to read, and the dollars those tokens would have cost | The guard writes one record for each compressed text, with the bytes before and after and the caller's groups |
| [Model routing](routing.md) | The price difference between the most expensive model a router can choose and the model it chose | The audit record of each LLM request that went through a `ModelRouter`, and the current Model prices |

**Every number on the page is an estimate, and the page says so.** Ambit sees bytes and token counts reported by providers. It does not see your provider's invoice, the tokenizer of your model, or how often a client sends a result again.

## What you need

- `evidence.enabled: true`. The page reads the audit trail in ClickHouse. Without it the page says so.
- For compression: `protection.enabled: true` and `resultCompression` set to `lossless` or `shorten` on a Server. With `evidence.enabled`, the guard then writes the records. See [Compression records](concepts.md#compressing-tool-results).
- For dollars: a price on the Models (`spec.price`). Budgets and the Usage page use the same prices.

## Where the numbers are stored

The console keeps two small summary tables in ClickHouse, filled by materialized views when the collector writes a block of records:

| Table | One row for | Holds |
|---|---|---|
| `ambit_llm_daily_v1` | UTC day, group, router, model | requests, input tokens, output tokens |
| `ambit_compress_daily_v1` | UTC day, group, server, type | compressed texts, bytes before, bytes after, cuts |

The page reads about 30 days of summary rows instead of every record of the month. The summaries hold counts only: no email, no tool content, no prompt. Group names are the only identifying text in them.

- **Counted since.** The views see only records written after the console created them. There is no backfill, because a copy from `otel_logs` cannot be exact (the table has no insert time, so a copy can miss records or count one twice). The page shows the date the counting began: the day the views were created, or the day of the oldest summary row if that is earlier. Compression has no earlier history anyway.
- **Retention.** `evidence.summaryRetention`, default `26280h` (three years). It is rounded up to whole days and must be at least `24h`. A new value applies at the next console start. Raw records follow `evidence.retention`.
- **ClickHouse rights.** The console creates the tables and views itself, once, and retries until the collector has created `otel_logs`. With the chart's own ClickHouse nothing is needed. With an external ClickHouse the user needs `CREATE TABLE`, `CREATE VIEW`, `ALTER` (to change the retention), `INSERT` and `SELECT` in the database, and `SELECT` on `system.tables`. If the creation fails, the Savings page shows the error; Usage and Audit are not affected.
- **Usage page.** It reads `ambit_llm_daily_v1` (group rows only) for every day the summary holds completely: the days after the creation day, which is partial. Older days still come from `otel_logs`, so the page shows the same numbers as before an upgrade and the scan shrinks each day until it is gone. The summary is per UTC day, so the first day of a period counts whole.
- **Query cost.** The page reads the summaries, not `otel_logs`. The pull request that added them records the measured rows, bytes and time of both queries for a seeded 30-day data set.

## Tokens from bytes (compression)

The guard knows the size of a text, not its tokens. The page converts:

```
tokens saved = (bytes before - bytes after) / bytes per token of the type
```

| Type | Bytes per token used |
|---|---|
| json | 2.9 |
| table | 3.0 |
| yaml | 3.4 |
| logs | 2.4 |
| test | 2.7 |
| other | 3.0 |

These values come from an offline check on generated text with one public tokenizer. They are a heuristic. The page shows the point value and a range of 25% below and above it, and the range is **not a proven bound**: real data and the tokenizer of your model can differ more, in either direction. Replace the values once you have measured your own tools.

**What was measured with real clients.** The measured token savings of a whole session are in [Measured savings](concepts.md#measured-savings): with opencode, `lossless` used 16% fewer input tokens and `shorten` 49% fewer; with Codex, 3% and 31%. That run used 3 repetitions, one model and synthetic tool results of 11 KB to 37 KB. It shows how much compression can save in a session, and why a client that cuts large results itself saves less. It does not measure the bytes-per-token values above, and the page does not use it to inflate any figure.

## Dollars (compression)

Saved tokens are input tokens of a later model call, but the guard does not know which model reads the result. The page prices them in this order, for each group:

1. The group's own **average input price** over the period: its LLM requests through Ambit, weighted by input tokens, over the models that have a price.
2. If the group has no priced LLM requests through Ambit (for example, a client that talks to its provider directly and uses Ambit only for MCP): the Helm value `console.savings.referenceInputPricePerMillion`, in US dollars per million input tokens. It is empty by default.
3. If both are missing, the page shows tokens only and says: "No price: set a price on a Model or the reference price."

**Prices are applied when the page is read, at the current Model prices.** The summaries store tokens, never dollars. A price change therefore changes past days too, as on the Usage page.

## Later turns that carry each result

A tool result is part of the conversation. The client usually sends it again with every later request until the conversation is cut or compacted. A saving of 1,000 tokens on a result that is sent 6 more times is 7,000 tokens of input.

Ambit cannot know this. It sees a result once, when it passes the guard. It cannot see how many later requests carry it, whether the client compacts, or how much your provider's prompt cache lowers the price of repeated input.

So the headline counts **the first send only**. The page has one control, "Later turns that carry each result", default 0. Set it to N and the compression figures become:

```
tokens (and dollars) x (1 + N)
```

The page then says "as if each result is sent N more times (your assumption, not measured)". The control changes the compression figures only. Routing is a price difference per request and is never multiplied. Choose N from your own data, for example the average number of turns of a session after a tool call.

## Routing

For each LLM request that went through a router, the page counts:

```
saving = cost of the most expensive priced model the router can choose - cost of the model that answered
```

Both costs use the same tokens, those of the model that answered. The router and its candidate Models (`spec.default` and every route) are read when the page opens. The row is the unit: the most expensive model is taken for the tokens of one day, group, router and model, and the saving is never negative, because the answering Model is always a candidate.

What this means, and what it does not:

- **The baseline is high.** Nobody would send everything to the most expensive model. Read the figure as the value of having a router, against the dearest choice, not as a measured bill.
- **It can overstate for some people.** The candidate list does not depend on the person. Someone who may not use the dearest model falls through to a cheaper one, and is still credited against the dearest.
- **Current configuration, not history.** A changed router or price changes past days. A router that no longer exists is listed as "router no longer exists" and is not priced. A Model without a price is not a baseline, and the page names it. If the answering Model has no price or is gone, its requests are named and left out of the dollar figure.
- **Gross.** The cost of the classifier of an `LLM` router is not subtracted. Its tokens are in Prometheus only (`ambit_router_classifier_tokens_total`) and not in budgets.
- Not counted: model fallback (`spec.fallback`), refused requests (they carry no tokens), prompt cache discounts, provider batch discounts.

## Groups and totals

A person in two groups counts toward **both** groups, as on the Usage page. So the group rows can add up to more than the total, and the page says so: the group chart shows each group's share of the total (for example 50%), and the shares of all groups can add up to more than 100%. The CSV export has plain numbers. The tiles, the daily chart and the server and router tables read one row per request (the all-groups row), so no total counts anyone twice. The group chart shows the top ten groups; the table lists all of them.

## What the page does not show

- Savings per person. The summaries hold no email, and the guard record has none.
- Savings of tool results that were not compressed, or that Ambit could not record. Records are sent on a best-effort basis: when the collector cannot be reached or the guard's queue (1,000 records) is full, the record is dropped and `ambit_guard_record_dropped_total` goes up. The page cannot see drops, so its totals can be incomplete. The alert `AmbitGuardRecordsDropped` (with `alerts.enabled`) tells you.
- Compression of `structuredContent`, which the guard never changes, and savings of clients that do not send the text (see [Measured savings](concepts.md#measured-savings)).
