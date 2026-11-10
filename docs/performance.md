---
title: Performance
description: "How much time the Ambit gateway adds to a model call and to an MCP tool call, measured on one reference machine, and how to repeat the measurement."
---

# Performance

This page shows how much time the Ambit gateway adds to one request. The numbers come from one laptop with a local kind cluster, the **reference environment** below. They are reference numbers on one machine, not a guarantee: your cluster, your network and your provider decide the time a person waits. Repeat the measurement on your own cluster with `test/bench.sh` (see [Repeat the measurement](#repeat-the-measurement)).

## Results

Measured on 2026-11-10 at commit `d5e432c` (main after pull request #271), in the [reference environment](#reference-environment).

In plain words: with a personal API key or a company token, the gateway added about 1.5 ms at the median when requests were 100 ms apart (10 per second), and less than 0.5 ms at 50 and 100 requests per second. An MCP tool call through the guard added 1.5 to 4.5 ms at the median. Compression of the result did not add time to masking: the compressed result is about half the size, which saves time later in the path.

| Added time at the median (p50) | 10 req/s | 50 req/s | 100 req/s |
|---|---|---|---|
| Model call, company token (JWT) | 1.4 ms | 0.2 ms | 0.0 ms |
| Model call, personal API key | 1.3 ms | 0.2 ms | -0.1 ms |
| MCP tool call, masking | 4.3 ms | 2.2 ms | 1.5 ms |
| MCP tool call, masking and compression | 3.1 ms | 2.0 ms | 1.4 ms |

All measurements (milliseconds; the median of 3 runs, with the lowest and highest run in brackets):

| Case | Rate (req/s) | p50 ms | p95 ms | p99 ms | Added p50 ms | Added p95 ms | Added p99 ms | Throughput (req/s) | Errors |
|---|---|---|---|---|---|---|---|---|---|
| llm-direct | 10 | 3.8 [3.4-4.0] | 7.5 [7.5-7.8] | 10.3 [9.0-10.5] | - | - | - | 10.0 | 0 |
| llm-direct | 50 | 3.0 [2.8-3.1] | 5.9 [5.6-6.0] | 8.0 [7.3-8.7] | - | - | - | 50.0 | 0 |
| llm-direct | 100 | 2.5 [2.0-2.8] | 4.9 [4.3-5.2] | 7.1 [5.7-7.6] | - | - | - | 99.9 | 0 |
| llm-jwt | 10 | 5.2 [4.9-6.1] | 10.5 [10.1-11.4] | 13.9 [12.1-14.9] | 1.4 | 3.0 | 3.6 | 10.0 | 0 |
| llm-jwt | 50 | 3.2 [3.1-3.7] | 6.2 [6.0-6.6] | 8.1 [7.9-8.5] | 0.2 | 0.3 | 0.1 | 50.0 | 0 |
| llm-jwt | 100 | 2.5 [2.4-3.0] | 4.6 [4.3-4.9] | 6.3 [6.1-7.0] | 0.0 | -0.3 | -0.8 | 100.0 | 0 |
| llm-key | 10 | 5.1 [4.9-5.5] | 10.0 [9.9-10.4] | 12.5 [11.1-16.1] | 1.3 | 2.5 | 2.2 | 10.0 | 0 |
| llm-key | 50 | 3.2 [3.0-3.2] | 6.1 [6.0-6.1] | 8.0 [8.0-8.2] | 0.2 | 0.2 | 0.0 | 50.0 | 0 |
| llm-key | 100 | 2.4 [2.4-3.2] | 4.6 [4.3-4.9] | 6.5 [5.7-6.5] | -0.1 | -0.3 | -0.6 | 100.0 | 0 |
| mcp-direct | 10 | 2.5 [2.4-2.7] | 6.0 [5.7-6.5] | 8.9 [7.4-14.0] | - | - | - | 10.0 | 0 |
| mcp-direct | 50 | 2.1 [1.9-2.1] | 4.9 [4.7-5.1] | 7.1 [7.1-7.2] | - | - | - | 50.0 | 0 |
| mcp-direct | 100 | 1.9 [1.6-2.3] | 4.4 [4.2-4.8] | 6.5 [6.5-6.5] | - | - | - | 100.0 | 0 |
| mcp-mask | 10 | 6.8 [5.8-6.9] | 12.1 [11.8-12.9] | 15.4 [14.6-17.7] | 4.3 | 6.1 | 6.5 | 10.0 | 0 |
| mcp-mask | 50 | 4.3 [3.9-4.4] | 7.2 [7.2-7.5] | 9.5 [8.9-9.5] | 2.2 | 2.3 | 2.4 | 50.0 | 0 |
| mcp-mask | 100 | 3.4 [3.1-3.9] | 6.0 [5.8-6.3] | 8.0 [7.4-8.0] | 1.5 | 1.6 | 1.5 | 100.0 | 0 |
| mcp-compress | 10 | 5.6 [5.5-6.3] | 11.2 [10.5-11.6] | 13.8 [12.3-15.1] | 3.1 | 5.2 | 4.9 | 10.0 | 0 |
| mcp-compress | 50 | 4.1 [4.1-4.2] | 7.5 [7.4-7.9] | 9.8 [9.1-31.1] | 2.0 | 2.6 | 2.7 | 50.0 | 0 |
| mcp-compress | 100 | 3.3 [3.2-3.6] | 5.8 [5.7-6.1] | 7.4 [7.2-7.8] | 1.4 | 1.4 | 0.9 | 100.0 | 0 |

How to read the table:

- **Every case is faster at 100 requests per second than at 10, the direct cases too.** With 100 ms between requests, the CPUs of the Docker virtual machine go idle, and each request first has to wake them. Compare a case only with its direct baseline at the same rate.
- **An added time near 0, or below 0, means "too small to measure here"**: the difference is smaller than the change between two runs of the same case. The runs of one case differ by up to about 1 ms at p50.
- **p99 is the value that 1 request in 100 exceeds.** In one run of 3,000 requests it rests on 30 requests, so one pause of the laptop moves it (for example the run of 31.1 ms in `mcp-compress` at 50 req/s).
- No request failed: 0 errors in the 54 runs, and the gateway answered at the rate it was sent (Throughput).

Repeatability: a first full run on the same cluster gave medians (p50) within 0.4 ms of these, or at most 16 % apart. In one run of that first run (`mcp-mask` at 50 req/s) all 3,000 requests failed, and the logs did not show why. `test/bench.sh` now checks each MCP session with one call before it measures, and stops when a run gets no answer; the published numbers come from the second full run, in which no request failed.

## What was measured

A request through Ambit takes the time of the direct request plus the time the gateway adds: the check of the token or personal key, the access rule, the budget check, the audit record, and for MCP the guard (masking and, if it is on, compression). To see only that added time, the model and the MCP server are test servers that answer at once, and every request is sent twice: once directly to the test server, and once through the gateway.

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
- A value in the table is the median of the three runs, with the lowest and highest run in brackets. **Added** is the median of the case minus the median of its direct baseline at the same rate. A difference of percentiles is an estimate of the added time, not an exact value per request.
- Throughput is the number of answered requests in the measured 60 seconds, divided by 60. Errors counts every answer that was not HTTP 200, in all three runs.

## Reference environment

| Item | Value |
|---|---|
| Computer | Apple Mac mini, M4 Pro (12 CPU cores), 24 GB memory |
| Operating system | macOS 27.0 |
| Docker | Docker Desktop, Docker Engine 29.6.1; the Linux virtual machine has 4 CPUs and 5.8 GiB memory |
| Kubernetes | kind v0.33.0, one node, Kubernetes v1.37.0 |
| Ambit | commit `d5e432c`, chart values `charts/ambit/values-kind.yaml` and `test/keycloak/values-sso.yaml` (audit, budgets and MCP protection on), one gateway replica |
| agentgateway | v1.7.0 |
| Other load | A second, idle kind cluster ran in the same Docker virtual machine. No other tests or builds ran during the measurement. |

## Repeat the measurement

`test/bench.sh` sets up what it needs on a local kind cluster with Ambit installed (the test identity provider, the mock model, the test MCP server, a `Budget` and a personal API key), runs every case, and prints the table. It removes its objects at the end. Run it alone on the machine: other work on the same computer changes the numbers.

```sh
make kind-up image install
lockf /tmp/ambit-kind.lock test/bench.sh
```

Settings, as environment variables: `RATES` (default `10 50 100`), `RUNS` (default `3`), `DURATION` (default `70s`, warm-up included), `WARMUP` (default `10s`) and `OUT` (a directory for the raw results). A complete run with the defaults takes about 65 minutes.

To measure your own cluster, keep the method: send from a pod in the same cluster, and compare each case with a direct request to the same server.

## What these numbers do not show

- The time of a real model. A model answer takes from hundreds of milliseconds to many seconds; the time the gateway adds is small next to it. The time of the provider's network is not in these numbers.
- A cluster with several nodes, several gateway replicas, or a network between the client and the cluster.
- The highest rate the gateway can carry. The rates here are fixed, and all parts (the gateway, the test servers, the sender, Keycloak and ClickHouse) share 4 CPUs.
- Streaming answers, large prompts, and MCP results much larger than 10 KB. The guard's time grows with the size of a result.
