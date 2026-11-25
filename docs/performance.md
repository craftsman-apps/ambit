---
title: Performance
description: "How much time the Ambit gateway adds to a model call and to an MCP tool call, measured on one reference machine, and how to repeat the measurement."
---

# Performance

This page shows how much time the Ambit gateway adds to one request. The numbers come from one laptop with a local kind cluster, the **reference environment** below. They are reference numbers on one machine, not a guarantee: your cluster, your network and your provider decide the time a person waits. Repeat the measurement on your own cluster with `test/bench.sh` (see [Repeat the measurement](#repeat-the-measurement)).

## Results

Measured on 2026-11-25 on main at `feb597b` (after pull request #315) plus the two changes of pull request #389 (the benchmark script), in the [reference environment](#reference-environment). The first measurement, on 2026-11-10 at commit `d5e432c` (main after pull request #271), gave the same picture; the differences between the two are listed under Repeatability.

**These are reference numbers on one machine, not a guarantee.**

In plain words: at 10 requests per second (100 ms apart) the gateway added about 1.3 ms at the median (p50) to a model call with a personal API key. With a company token the added time was below what this method can measure (under 1.4 ms). At 50 and 100 requests per second the added p50 of a model call was smaller than the difference between two runs of the same direct request, so this method cannot measure it (the p95 of the company token was 0.8 and 0.4 ms). An MCP tool call through the guard added 1.5 to 4.0 ms at p50, and 1.5 to 3.8 ms with compression on.

Added time in milliseconds: the median of 3 runs of the case, minus the median of 3 runs of its direct baseline at the same rate. **Below noise (< X)** means the added time was smaller than X, the larger spread (highest minus lowest run) of the case and of its direct baseline in that cell; the method cannot tell such a time from 0, and it is not 0.

| Case | Rate (req/s) | Added p50 | Added p95 | Added p99 |
|---|---|---|---|---|
| llm-jwt | 10 | below noise (< 1.4) | below noise (< 2.3) | below noise (< 42.9) |
| llm-jwt | 50 | below noise (< 0.6) | 0.8 | below noise (< 1.0) |
| llm-jwt | 100 | below noise (< 0.5) | 0.4 | below noise (< 1.3) |
| llm-key | 10 | 1.3 | below noise (< 2.3) | below noise (< 42.9) |
| llm-key | 50 | below noise (< 0.6) | below noise (< 0.7) | below noise (< 1.0) |
| llm-key | 100 | below noise (< 0.6) | below noise (< 0.3) | below noise (< 0.8) |
| mcp-mask | 10 | 4.0 | 4.9 | 5.8 |
| mcp-mask | 50 | 2.2 | 3.0 | 3.1 |
| mcp-mask | 100 | 1.5 | 1.8 | 2.1 |
| mcp-compress | 10 | 3.8 | 5.2 | 6.8 |
| mcp-compress | 50 | 2.3 | 3.1 | 3.3 |
| mcp-compress | 100 | 1.5 | 1.8 | 2.2 |

All measurements, in milliseconds; the median of 3 runs, with the lowest and highest run in brackets:

| Case | Rate (req/s) | p50 | p95 | p99 | Throughput (req/s) | Errors |
|---|---|---|---|---|---|---|
| llm-direct | 10 | 3.9 [3.6-4.7] | 7.4 [7.2-9.5] | 11.3 [8.2-51.1] | 10.0 | 0 |
| llm-direct | 50 | 3.3 [2.9-3.5] | 5.6 [5.0-5.7] | 7.4 [6.9-7.9] | 50.0 | 0 |
| llm-direct | 100 | 2.6 [2.3-2.7] | 4.5 [4.3-4.6] | 6.4 [5.9-6.6] | 99.9 | 0 |
| llm-jwt | 10 | 5.0 [4.5-5.9] | 9.0 [8.9-9.9] | 11.8 [10.8-11.9] | 10.0 | 0 |
| llm-jwt | 50 | 3.9 [3.6-3.9] | 6.4 [6.1-6.7] | 7.8 [7.7-8.4] | 50.0 | 0 |
| llm-jwt | 100 | 3.0 [2.6-3.1] | 4.9 [4.8-5.0] | 5.8 [5.8-7.1] | 100.0 | 0 |
| llm-key | 10 | 5.2 [5.1-5.6] | 9.3 [9.1-9.3] | 11.0 [10.8-11.0] | 10.0 | 0 |
| llm-key | 50 | 3.6 [3.4-3.9] | 6.2 [6.0-6.3] | 7.7 [7.7-7.7] | 50.0 | 0 |
| llm-key | 100 | 2.6 [2.4-3.0] | 4.6 [4.4-4.7] | 5.6 [5.4-5.7] | 99.9 | 0 |
| mcp-direct | 10 | 2.8 [2.5-2.9] | 5.6 [5.6-6.1] | 7.1 [7.0-8.3] | 10.0 | 0 |
| mcp-direct | 50 | 2.3 [2.1-2.4] | 5.0 [4.8-5.1] | 6.7 [6.4-7.0] | 50.0 | 0 |
| mcp-direct | 100 | 2.0 [1.7-2.3] | 4.6 [4.1-4.8] | 6.2 [5.9-6.3] | 100.0 | 0 |
| mcp-mask | 10 | 6.8 [6.5-6.8] | 10.5 [10.4-10.9] | 12.9 [12.9-13.1] | 10.0 | 0 |
| mcp-mask | 50 | 4.5 [4.4-4.8] | 8.0 [7.8-8.0] | 9.8 [9.7-9.9] | 50.0 | 0 |
| mcp-mask | 100 | 3.5 [3.4-3.5] | 6.4 [6.3-6.4] | 8.3 [8.2-8.5] | 100.0 | 0 |
| mcp-compress | 10 | 6.6 [5.5-6.9] | 10.8 [10.1-11.3] | 13.9 [12.4-15.9] | 10.0 | 0 |
| mcp-compress | 50 | 4.6 [4.1-4.7] | 8.1 [7.5-8.2] | 10.0 [9.4-10.5] | 50.0 | 0 |
| mcp-compress | 100 | 3.5 [3.4-3.5] | 6.4 [6.4-6.7] | 8.4 [8.4-9.4] | 100.0 | 0 |

How to read the tables:

- **Every case is faster at 100 requests per second than at 10, the direct cases too**, and so the added time is also smaller at the higher rates. The cause was not measured. Because the direct requests, which do not pass Ambit, slow down in the same way, the likely cause is the test machine and not Ambit: with 100 ms between requests the CPUs of the Docker virtual machine can go idle, and connections between the parts are less often in use. This is a guess, not a result.
- **Masking and compression together was not slower than masking alone.** At 10 req/s the added p50 (3.8 and 4.0 ms) differ by less than the spread of the runs: the runs of `mcp-mask` lay between 6.5 and 6.8 ms, the runs of `mcp-compress` between 5.5 and 6.9 ms. At 50 and 100 req/s the two were 0.1 and 0 ms apart. So the difference is within the run-to-run spread; it does not show that compression makes a call faster.
- **p99 is the value that 1 request in 100 exceeds.** In one run of 600 requests (10 req/s) it rests on 6 requests, so one pause of the laptop moves it, as in the run of 51.1 ms in `llm-direct` at 10 req/s.
- Every measured request got HTTP 200: 0 errors in the 54 runs, and the gateway answered at the rate it was sent (Throughput). The sender checks the HTTP status only; one tool call of each MCP session is checked for a result before the run starts.

Repeatability: this run and the first one (2026-11-10, commit `d5e432c`) were made on the same machine with the same method. The p50 of every cell was within 1.0 ms of the first run (the largest: `mcp-compress` at 10 req/s, 6.6 ms against 5.6 ms), and within 22 % in relative terms. The added p50 of the MCP cases agree within 0.7 ms (for example `mcp-mask` at 10 req/s: 4.0 ms against 4.3 ms). For the model cases at 10 req/s the personal key gave 1.3 ms in both runs, and the company token gave 1.4 ms in the first run and was below noise (under 1.4 ms) in this one; the other model cells are below noise in both runs.

On 2026-11-10, in a first full run that was not published, one run (`mcp-mask` at 50 req/s) lost all 3,000 requests, and the logs did not show why. The cause (found 2026-11-25, issue #274) was in the test script, not in Ambit: `kubectl exec -i ... cat > file`, which copies the access token into the sender pod, delivers an empty file about once in 75 times (4 of 300 copies in a test for pull request #389). The run of 2026-11-10 recorded no status codes, so the cause cannot be proven for that one run; the symptoms are the same (no answer at all, and in the test 403 and no session id). With no token the gateway answered 403 to the MCP `initialize`, no session was opened, and every call of the run failed the same way. The same token works from outside the pod. `test/bench.sh` now checks the size of every file it copies into the pod with `put` and copies again, and `test/load` prints why requests failed. In this run the check repeated one copy (the log says so), and no request of the 54 runs failed. The numbers published on 2026-11-10 came from a second full run, in which no request failed, so they were not affected.

## Prompt guard

Measured on 2026-11-19 on the branch of the pull request that added the guard (main at `d23898b` plus the guard), in the [reference environment](#reference-environment), with `SUITE=guard test/bench.sh`. **Reference numbers on one machine, not a guarantee.** Another Ambit kind cluster ran in the same Docker virtual machine during the measurement (more noise than the table above).

In plain words: for prompts up to about 100 KB the [prompt guard](concepts.md#prompt-guard) added less time than the difference between two runs of the same request without it, so this method cannot measure it (under 1.5 ms at p50). For a 1 MB prompt it added about 5 ms. Coding agents send the whole conversation again with every request, so a long session sends prompts of hundreds of kilobytes.

What was measured: model requests through the gateway with a personal API key, to the mock model, 10 requests per second, 3 interleaved runs of 70 seconds (10 seconds of warm-up), without and with the guard. The guard had the four default built-in patterns (`CreditCard`, `Ssn`, `Email`, `PhoneNumber`) and 3 patterns of its own (`(?i)ignore (all )?(previous|prior) instructions`, `AKIA[0-9A-Z]{16}`, `(?i)\bproject[- ]falcon\b`), mode `reject`. The prompts are a coding conversation (a system prompt, then user and assistant turns with code and build output) of 1.3 KB, 101 KB and 1.0 MB, and match no pattern: the usual case, in which the guard reads all of the text and lets it through. **Added** is the median of the case with the guard minus the median of the same prompt without it.

| Prompt size | Without guard p50 / p99 | With guard p50 / p99 | Added p50 | Added p99 |
|---|---|---|---|---|
| 1.3 KB | 5.3 / 10.9 | 4.8 / 11.9 | below noise (< 1.5) | below noise (< 3.8) |
| 101 KB | 5.8 / 12.8 | 7.0 / 13.1 | below noise (< 1.5) | below noise (< 2.4) |
| 1.0 MB | 9.2 / 16.7 | 14.4 / 22.4 | 5.2 | 5.7 |

All measurements, in milliseconds; the median of 3 runs, with the lowest and highest run in brackets:

| Case | Rate (req/s) | p50 | p95 | p99 | Throughput (req/s) | Errors |
|---|---|---|---|---|---|---|
| llm-1kb | 10 | 5.3 [5.0-5.5] | 8.9 [8.6-9.2] | 10.9 [10.8-14.6] | 10.0 | 0 |
| llm-1kb-guard | 10 | 4.8 [4.6-6.1] | 8.8 [8.2-9.5] | 11.9 [9.8-13.5] | 10.0 | 0 |
| llm-100kb | 10 | 5.8 [5.4-6.8] | 9.9 [9.6-10.4] | 12.8 [12.0-14.4] | 10.0 | 0 |
| llm-100kb-guard | 10 | 7.0 [6.3-7.8] | 10.6 [10.4-10.8] | 13.1 [13.0-13.2] | 10.0 | 0 |
| llm-1mb | 10 | 9.2 [8.7-11.1] | 14.3 [13.3-14.4] | 16.7 [15.1-17.8] | 10.0 | 0 |
| llm-1mb-guard | 10 | 14.4 [14.3-16.0] | 19.5 [19.2-21.6] | 22.4 [21.5-25.9] | 10.0 | 0 |

The pull request had a gate at 100 KB: added p50 under 2 ms and added p99 under 10 ms. It passed (the added time was below the noise bounds of 1.5 ms and 2.4 ms). Prompts that match a pattern were not measured: in `reject` mode they end before the model, and in `mask` mode the guard also rewrites the prompt.

## What was measured

A request through Ambit takes the time of the direct request plus the time the gateway adds: the check of the token or personal key, the access rule, the budget check, the audit record, and for MCP the guard (masking and, if it is on, compression). To see only that added time, the model and the MCP server are small test servers with no real model or app behind them, and each case through the gateway is compared with a separate measurement of direct requests to the same test server.

| Case | Request | Path |
|---|---|---|
| `llm-direct` | `POST /v1/chat/completions` to the mock model server (`test/llm/mock.yaml`) | Directly to the mock, no gateway. The baseline for the two model cases. |
| `llm-jwt` | The same request | Through the gateway, with a company token (a JWT from the test identity provider, Keycloak). |
| `llm-key` | The same request | Through the gateway, with a personal API key (`ambit_...`). |
| `mcp-direct` | `tools/call` of `get_rows` (50 JSON rows, about 10 KB, one email address) on the test MCP server `crm` (`test/crm`) | Directly to the server, no gateway. The baseline for the two MCP cases. |
| `mcp-mask` | The same call | Through the gateway and the guard. The guard masks the email address. |
| `mcp-compress` | The same call | As `mcp-mask`, with `resultCompression: lossless` on the Server. The guard also compresses the result (9,876 to 5,015 bytes). |

What was on in the gateway, as in a company install: audit records (gateway access log to ClickHouse), budgets (one `Budget` for the caller's group, with a limit far above the load, so every model request is checked against Valkey), and MCP protection (the guard). One gateway replica.

Method:

- A pod inside the cluster sends the requests (`test/load`, open loop: it sends at a fixed rate, and a slow answer does not slow the sender). The sender does not cross the Docker port mapping of the laptop, so the numbers do not include it.
- Each run lasts 70 seconds. The first 10 seconds are a warm-up and are not counted. Latency is the time until the first byte of the answer.
- Three fixed rates: 10, 50 and 100 requests per second.
- Three runs of every case at every rate. The runs are interleaved: run 1 of every case and rate, then run 2, then run 3, so a slow change of the machine affects all cases alike.
- A value in the table is the median of the three runs, with the lowest and highest run in brackets. **Added** is the median of the case minus the median of its direct baseline at the same rate; an added time smaller than the spread between runs (of the case or of its baseline) is shown as below noise. A difference of percentiles is an estimate of the added time, not an exact value per request.
- Throughput is the number of answered requests in the measured 60 seconds, divided by 60. Errors counts every answer that was not HTTP 200, in all three runs.

## Reference environment

| Item | Value |
|---|---|
| Computer | Apple Mac mini, M4 Pro (12 CPU cores), 24 GB memory |
| Operating system | macOS 27.0 |
| Docker | Docker Desktop, Docker Engine 29.6.1; the Linux virtual machine has 4 CPUs and 5.8 GiB memory |
| Kubernetes | kind v0.33.0, one node, Kubernetes v1.37.0 |
| Ambit | `feb597b` plus the benchmark script of pull request #389, chart values `charts/ambit/values-kind.yaml` and `test/keycloak/values-sso.yaml` (audit, budgets and MCP protection on), one gateway replica |
| agentgateway | v1.7.0 |
| Other load | The owner's kind cluster (idle) and, for part of the time, test clusters of other work ran in the same Docker virtual machine. This is more noise than in the first measurement, and the results are close to it. |

## Repeat the measurement

`test/bench.sh` sets up what it needs on a local kind cluster with Ambit installed (the test identity provider, the mock model, the test MCP server, a `Budget` and a personal API key), runs every case, and prints the table. At the end it deletes the objects it created, and the objects of `test/protection/objects.yaml`, as steps 9 and compress do. Run it alone on the machine: other work on the same computer changes the numbers.

```sh
make kind-up image install
lockf /tmp/ambit-kind.lock test/bench.sh
```

To measure the prompt guard instead (the table under [Prompt guard](#prompt-guard), about 30 minutes): `lockf /tmp/ambit-kind.lock env SUITE=guard test/bench.sh`.

Settings, as environment variables: `SUITE` (`latency`, the default, or `guard`), `RATES` (default `10 50 100`; `10` for `guard`), `RUNS` (default `3`), `DURATION` (default `70s`, warm-up included), `WARMUP` (default `10s`) and `OUT` (a directory for the raw results). A complete run with the defaults took about 65 minutes on the reference machine, setup included.

To measure your own cluster, keep the method: send from a pod in the same cluster, and compare each case with a direct request to the same server.

## What these numbers do not show

- The time of a real model. A model answer takes from hundreds of milliseconds to many seconds; the time the gateway adds is small next to it. The time of the provider's network is not in these numbers.
- A cluster with several nodes, several gateway replicas, or a network between the client and the cluster.
- The highest rate the gateway can carry. The rates here are fixed, and all parts (the gateway, the test servers, the sender, Keycloak and ClickHouse) share 4 CPUs.
- Streaming answers, and MCP results much larger than 10 KB. Large prompts are measured only with and without the prompt guard (above). The guard's time grows with the size of a result.
