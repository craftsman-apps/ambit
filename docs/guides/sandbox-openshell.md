---
title: Run agents in a sandbox such as OpenShell
description: Run a coding agent inside an OpenShell sandbox whose only model endpoint is the Ambit gateway, so the agent cannot call a provider directly and every call is recorded under the person.
---

# Run agents in a sandbox such as OpenShell

**What you get:** a coding agent that runs inside a sandbox on a computer, may reach models only through the Ambit gateway, and does not receive the person's API key (it gets a placeholder). Ambit records every model call under the person.

**Tested on 2026-11-10** with [OpenShell](https://github.com/NVIDIA/OpenShell) v0.1.7, opencode 1.19.5, Docker Desktop on macOS (Apple silicon), and Ambit at commit `d5e432c` (before release 0.4.0). OpenShell is at version 0.1.x; from v0.1.4 to v0.1.7 it published one release each week. Its profile format can change; use the version on this page, or check the steps again with a newer one.

## Two different jobs

Ambit does not confine what an agent does on a machine. It decides which models and MCP servers a person may use, sets limits, and records the use. A sandbox does the other job: it decides which files, programs and network addresses the agent may use on the computer.

Think of an office building. With the policy in this guide, the sandbox is a room whose only door for model requests leads to the company's front desk (Ambit). The front desk checks who is asking and writes down every request.

| | OpenShell | Ambit |
|---|---|---|
| Confines the agent's files, programs and network on the computer | Yes | No |
| Keeps the API key out of the agent's reach | Yes: the agent sees a placeholder; OpenShell adds the key only to requests to the allowed host | The provider keys stay inside Ambit; the person uses a personal key or a sign-in token |
| Decides which models a person or group may use | No | Yes |
| Spending limits per group and person | No | Yes |
| Record of every model call, under the person | No (it records what the sandboxed process did) | Yes |

Together: the sandbox policy allows the Ambit host and denies other hosts, so an agent cannot use a key it finds on disk to call a provider directly. Ambit cannot stop that on its own. The test on this page checked direct calls to OpenAI and OpenRouter.

## Before you start

You need:

- Ambit with model access, and its LLM address reachable from the computer (in this guide `https://llm.example.com`).
- A personal API key of the person who runs the agent ([Personal API keys](../api-keys.md)), and a model the person may use.
- Docker, and OpenShell v0.1.7. Follow the OpenShell [installation guide for v0.1.7](https://github.com/NVIDIA/OpenShell/blob/v0.1.7/docs/about/installation.mdx) and set `OPENSHELL_VERSION=v0.1.7` to install this version. Check with `openshell status`.

## 1. Write the provider profile

A provider profile tells OpenShell which host the agent may reach, which program may reach it, and which stored key OpenShell adds to those requests. Save this as `ambit.yaml` and change `host`, `port` and `binaries`:

```yaml
# OpenShell provider profile: the company's Ambit gateway as the only model endpoint.
# The agent gets a placeholder in AMBIT_API_KEY; OpenShell adds the real personal key
# only to requests that go to the endpoint below.
id: ambit
display_name: Ambit gateway
description: Models through the company's Ambit gateway (OpenAI-compatible API)
category: inference
inference_capable: true
credentials:
  - name: api_key
    description: Ambit personal API key (ambit_...)
    env_vars: [AMBIT_API_KEY]
    required: true
    auth_style: bearer
    header_name: authorization
discovery:
  credentials: [api_key]
endpoints:
  - host: llm.example.com          # your Ambit LLM host
    port: 443
    protocol: rest
    access: read-write
    enforcement: enforce
binaries: [/usr/local/bin/opencode]  # the agent program inside the sandbox image
```

`binaries` names the program that this profile allows to use the host and the key. It does not give other programs in the sandbox access to the host.

## 2. Import the profile and store the key

```sh
openshell profile lint -f ambit.yaml
openshell profile import -f ambit.yaml
read -rs AMBIT_API_KEY && export AMBIT_API_KEY   # paste the personal key; it is not shown
openshell provider create --name ambit --type ambit --credential AMBIT_API_KEY
unset AMBIT_API_KEY
```

OpenShell keeps the key in its credential store. The sandbox never receives it.

## 3. Start the agent in a sandbox

The agent configuration names Ambit as its only provider. opencode reads its configuration from the environment variable `OPENCODE_CONFIG_CONTENT` (the test used this). Use a model name that the person may use in Ambit:

```sh
CFG='{"$schema":"https://opencode.ai/config.json","model":"ambit/MODEL",
  "provider":{"ambit":{"npm":"@ai-sdk/openai-compatible","name":"Ambit",
  "options":{"baseURL":"https://llm.example.com/v1","apiKey":"{env:AMBIT_API_KEY}"},
  "models":{"MODEL":{"name":"MODEL"}}}}}'

openshell sandbox create --name agent \
  --from ghcr.io/anomalyco/opencode:1.19.5@sha256:f4cfe16d91efbbfca01a51c44094fba4cf82e15c7fa17de5f11f9059699b1e6e \
  --provider ambit \
  --env "OPENCODE_CONFIG_CONTENT=$CFG" \
  -- opencode
```

Inside the sandbox, `AMBIT_API_KEY` holds a placeholder that starts with `openshell:`, not the key.

## 4. Check both sides

In Ambit, open **Audit** in the console and search for the person's email. Ambit records each model call as one row, with the person, the model and the key id; in the test, every call of the agent was there.

In OpenShell, `openshell logs agent` shows every connection. A call to Ambit is allowed by the provider policy; a direct call to a provider is denied. These two lines are from [the test we ran](#the-test-we-ran) (timestamps removed), where the Ambit host was `host.openshell.internal:18080`:

```text
[sandbox] [OCSF ] [ocsf] HTTP:POST [INFO] ALLOWED POST http://host.openshell.internal:18080/v1/chat/completions [policy:_provider_ambit engine:l7]
[sandbox] [OCSF ] [ocsf] NET:OPEN [MED] DENIED /usr/local/bin/opencode(0) -> api.openai.com:443 [reason:transparent_tcp_policy_denied]
```

## The test we ran

On 2026-11-10, on one Mac with Docker Desktop:

- Ambit ran in a local kind cluster, with the test model `mock/echo` (a server that answers every request with "pong") and a personal key of the test user `user09@example.com`. The gateway was reachable from the sandbox at `http://host.openshell.internal:18080`, OpenShell's name for the computer that runs the sandbox, so the profile had `host: host.openshell.internal` and `port: 18080`, and Ambit's LLM host (`llm.host` in the chart values) was set to `host.openshell.internal`. HTTPS was not tested.
- OpenShell v0.1.7: the CLI and the gateway from the v0.1.7 release files (checksums checked), gateway with the Docker driver, telemetry off (`OPENSHELL_TELEMETRY_ENABLED=false`).
- opencode 1.19.5 ran `opencode run "Reply with one short sentence."` in the sandbox. The test model does not mark the end of a streamed answer, so opencode asked again until it was stopped. Each of those requests was a model call through Ambit.

Results:

| Check | Result |
|---|---|
| `AMBIT_API_KEY` inside the sandbox | A placeholder starting with `openshell:`, not the key |
| opencode → Ambit | Allowed: `ALLOWED POST http://host.openshell.internal:18080/v1/chat/completions [policy:_provider_ambit engine:l7]` |
| Ambit Audit | Every call recorded under the person, for example `{"time": "2026-11-10T09:29:11.801Z", "email": "user09@example.com", "route": "ambit-system/ambit-llm", "model": "mock/echo", "status": "200", "keyId": "rjlb7wktyivo", "identity": "key", "path": "/v1/chat/completions"}` |
| opencode configured with `https://api.openai.com/v1` instead of Ambit | Denied: `DENIED /usr/local/bin/opencode(0) -> api.openai.com:443 [reason:transparent_tcp_policy_denied]` |
| `wget https://api.openai.com/v1/models` and `wget https://openrouter.ai/api/v1/models` from a shell in the sandbox | Denied: `can't connect to remote host: Permission denied` |

Not tested: MCP servers through the sandbox (they need a second endpoint for the Ambit MCP host), HTTPS to the gateway, other agents, and OpenShell on Kubernetes.
