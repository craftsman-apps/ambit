---
title: Connect Claude Code and Cursor
description: Give a person a personal API key and point Claude Code, Cursor or an SDK at Ambit, so every call is checked, counted and recorded under that person.
---

# Connect Claude Code and Cursor

**What you get:** a coding tool uses your company's models through Ambit with a personal API key. Ambit checks the person's access, counts the use and records every call. The provider's own key stays inside the cluster.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with the free model and the rule `everyone-free-model`. The addresses below are the quick-start addresses. On your own cluster, use your own LLM address.

- LLM address: `http://llm.localtest.me:8080`
- Console: `http://console.localtest.me:8080`
- Model name for clients: `nvidia/nemotron-3-super-120b-a12b:free`. Clients send the provider's model name, not the name of the `Model` object.

## 1. Create a key

1. Sign in to the console. On the quick-start cluster, sign in as `admin@ambit.local`. Every person who can sign in can create keys.
2. Open **Keys** and choose **New key**.
3. Enter a name, for example `laptop`. Choose the scope **LLM** and an end date. Choose **Create**.
4. Copy the key. It starts with `ambit_`. The console shows it once. It stores only a hash. If you lose the key, revoke it and create a new one.

The console also shows a snippet for each tool, with your addresses filled in. The snippets below are the same.

Treat the key like a password. A key carries the access of its owner and never more.

## 2. Configure your tool

**Claude Code.** Add this to `settings.json`. Claude Code needs the model name too, because its default names are not models at your provider.

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://llm.localtest.me:8080",
    "ANTHROPIC_AUTH_TOKEN": "ambit_...",
    "ANTHROPIC_MODEL": "nvidia/nemotron-3-super-120b-a12b:free",
    "ANTHROPIC_SMALL_FAST_MODEL": "nvidia/nemotron-3-super-120b-a12b:free"
  }
}
```

Claude Code prints a notice that it does not know this model. The notice is harmless.

**Claude Code with a model that is not a Claude model.** Claude Code was tested (version 2.1.299, 2026-10-14) with `nvidia/nemotron-3-super-120b-a12b:free` at OpenRouter, through `/v1/messages`. A prompt that needs one tool call worked, with and without the switch below. What to know:

- *Model variables.* Claude Code uses Claude model names for its background work unless you set the names. `ANTHROPIC_MODEL` and `ANTHROPIC_SMALL_FAST_MODEL` (above) were enough in the test. Claude Code also has `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL` and `ANTHROPIC_DEFAULT_OPUS_MODEL`; set them to your model name if you use the model names `haiku`, `sonnet` or `opus` in Claude Code. We did not test these three.
- *Experimental beta fields.* By default, Claude Code sends the fields `context_management` and `safeguards` in each request, and also `thinking` and `output_config`. A provider that does not know them can refuse the request. With `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`, Claude Code does not send `context_management` and `safeguards`. It still sends `thinking` and `output_config`, and `CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1` did not remove them in our test. Nemotron accepted all of it, so we cannot say which provider needs the switch. If a model refuses Claude Code's requests, set it first. Put it in the same `env` block:

  ```json
  "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  ```

- *Token counts are estimates.* Claude Code asks `/v1/messages/count_tokens` for exact counts, for example when you run `/context`. Ambit does not serve that path and answers 403 `authorization failed`. Claude Code goes on and shows estimated numbers; `/context` printed "Estimated usage by category". Each refused call is listed in **Audit** with status 403, under your email address. A short prompt with a tool call does not call this path.

**MCP with Claude Code.** Add the MCP gateway with a personal key that has the scope **MCP** (or **LLM and MCP**):

```sh
claude mcp add --transport http ambit http://mcp.localtest.me:8080/mcp --header "Authorization: Bearer ambit_..."
```

`claude mcp list` shows `ambit ... Connected`. In the test, Claude Code called `time_get_current_time` of the `time` server (see the [Quick start](../quick-start.md), step 9).

**Cursor.** In the model settings, enter:

```text
OpenAI API Key: ambit_...
Override OpenAI Base URL: http://llm.localtest.me:8080/v1
```

Cursor sends its requests from Cursor's servers, not from your computer. The LLM address must therefore be reachable from the internet over HTTPS. Cursor does not work with `localhost`, with a `localtest.me` address, or with a cluster that has no public address. This applies to your real install, not to the quick-start laptop cluster. Opening the LLM gateway to the internet means that only keys and tokens protect it.

**OpenAI SDK and other OpenAI-compatible tools:**

```sh
export OPENAI_BASE_URL=http://llm.localtest.me:8080/v1
export OPENAI_API_KEY=ambit_...
```

**Anthropic SDK:**

```sh
export ANTHROPIC_BASE_URL=http://llm.localtest.me:8080
export ANTHROPIC_API_KEY=ambit_...
```

## Check it works

Put the key in a shell variable. The first command reads it without showing it on the screen.

```sh
read -rs KEY
```

Call the OpenAI-style endpoint with `Authorization: Bearer`:

```sh
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

Call the Anthropic-style endpoint with `x-api-key`:

```sh
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/messages \
  -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","max_tokens":300,"messages":[{"role":"user","content":"Say hello in five words."}]}'
```

Both commands end with `HTTP 200` and an answer from the model. A new key works at once. With a wrong key, the answer is:

```text
authorization failed
HTTP 403
```

Then start Claude Code with the settings above and ask it a question. In the console, open **Audit** and search for your email address. Each call is listed with the model, the token counts and the key that made it. The audit shows the key's ID, never the key.

## Related

- [Connect Codex, opencode and Hermes Agent](codex-opencode-hermes.md) and [Other coding agents](other-agents.md): more tools.
- [Personal API keys](../api-keys.md): scope, end dates, limits, revoking, and how the gateway checks a key.
- [Models and providers](../providers.md): other providers and models.
- [Someone leaves](someone-leaves.md): what happens to a person's keys when they are offboarded.
