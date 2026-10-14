---
title: Connect Claude Code and Cursor
description: Give a person a personal API key and point Claude Code, Cursor or an SDK at Ambit, so every call is checked, counted and recorded under that person.
---

# Connect Claude Code and Cursor

**What you get:** a coding tool uses your company's models through Ambit with a personal API key. Ambit checks the person's access, counts the use and records every call. The provider's own key stays inside the cluster.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with a model and a rule that allows everyone. `ambit local up` makes the rule `everyone-local`; the [by-hand steps](../local-up.md) make `everyone-free-model`. The addresses below are the quick-start addresses. On your own cluster, use your own LLM address.

- LLM address: `http://llm.localtest.me:8080`
- Console: `http://console.localtest.me:8080`
- Model name for clients: the provider's model name, not the name of the `Model` object. `ambit local up` prints it in the summary (the `"model"` value of the call command) and on the line `<model> answered:`; it is `local/echo` for the test model. The console page **Models** shows it too. The by-hand steps use `nvidia/nemotron-3-super-120b-a12b:free`. The examples below use that name: replace it with yours.

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

- [Personal API keys](../api-keys.md): scope, end dates, limits, revoking, and how the gateway checks a key.
- [Models and providers](../providers.md): other providers and models.
- [Someone leaves](someone-leaves.md): what happens to a person's keys when they are offboarded.
