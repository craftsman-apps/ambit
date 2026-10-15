---
title: Connect Codex, opencode and Hermes Agent
description: Point OpenAI Codex CLI, opencode or Hermes Agent at Ambit with a personal API key, and give them the Ambit MCP gateway. Each snippet except the Hermes MCP note was run against a live Ambit.
---

# Connect Codex, opencode and Hermes Agent

**What you get:** three more coding agents use your company's models through Ambit, with the same personal API key, access rules, budgets and audit log as [Claude Code and Cursor](coding-tools.md).

Every snippet on this page was run against a real Ambit install, with a real model, except the Hermes MCP note, which comes from the vendor page and was not run. The list of what was not tested is at the end of the page.

## What was tested

| Tool | Version | Request format | Result |
|---|---|---|---|
| Codex CLI | 0.159.3 | OpenAI Responses (`/v1/responses`) | Works. One shell tool call, answer correct. |
| opencode | 1.18.34 | OpenAI Chat Completions (`/v1/chat/completions`) | Works. One file-read tool call, answer correct. |
| Hermes Agent | v0.13.0 (2026.5.7) | OpenAI Chat Completions (`/v1/chat/completions`) | Works with the config below. The first config we tried failed; see [Hermes Agent](#hermes-agent). |

Test setup: Ambit from the [Quick start](../quick-start.md) on kind, the model `nvidia/nemotron-3-super-120b-a12b:free` at OpenRouter, a personal key with the scope **LLM and MCP**, on 2026-10-14. Only this one model was tried. Each tool made one prompt that needs one tool call, and every successful call is in the Audit log with status 200.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), the model, a rule that allows it, and a personal API key (see [Connect Claude Code and Cursor](coding-tools.md), step 1). The addresses below are the quick-start addresses. On your own cluster, use your own.

- LLM address: `http://llm.localtest.me:8080`
- MCP address: `http://mcp.localtest.me:8080/mcp`
- Model name for clients: `nvidia/nemotron-3-super-120b-a12b:free`. Clients send the provider's model name, not the name of the `Model` object.

## Codex CLI

Codex speaks only the OpenAI Responses format. Ambit serves `/v1/responses`.

**With the Ambit CLI.** After you created a key, run:

```sh
ambit connect codex --url http://llm.localtest.me:8080 --model nvidia/nemotron-3-super-120b-a12b:free
```

It asks for the key (hidden), stores it in the keychain of your computer, and writes two blocks into `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`). Each block starts with a line `# >>> ambit: written by ...` and ends with `# <<< ambit <<<`. The first block, before the first `[table]`, holds `model` and `model_provider`. The second, at the end of the file, holds the `[model_providers.ambit]` table. The key is not in the file: the table has `[model_providers.ambit.auth]` with `command` and `args`, and Codex runs `ambit key print --url <llm>` to read the key. This differs from the by-hand setup below, which uses `env_key`: Codex does not allow both. The `auth` command was tested with a local test server and Codex 0.159.3, not with a live Ambit and Codex together. The command ends with a test call to `/v1/responses`.

If your file already sets `model` or `model_provider` to another value, or has a `[model_providers.ambit]` table, the command stops. With `--force`, it keeps your old lines as comments inside a block (`# ambit-replaced: ...`), and `ambit disconnect codex` puts them back. `ambit disconnect codex` removes every Ambit block and keeps the rest of the file as it was.

**By hand.** Add this to `~/.codex/config.toml`:

```toml
model = "nvidia/nemotron-3-super-120b-a12b:free"
model_provider = "ambit"

[model_providers.ambit]
name = "Ambit"
base_url = "http://llm.localtest.me:8080/v1"
env_key = "AMBIT_API_KEY"
wire_api = "responses"
```

Put the key in the environment and run a prompt:

```sh
export AMBIT_API_KEY=ambit_...
codex exec --skip-git-repo-check --sandbox read-only "Run the shell command 'cat note.txt' and tell me what it prints."
```

Codex ran the command and printed the file. Notes from the test:

- Codex prints `Model metadata for ... not found. Defaulting to fallback metadata`. It does not know your model. The warning is harmless.
- The model is not an OpenAI model, and the call still worked. Ambit sent the Responses request on to OpenRouter and OpenRouter answered. We did not check whether Ambit or OpenRouter converts the format, so we cannot say that every provider works this way. Try your own provider with one prompt before you give Codex to your team.
- When `codex exec` runs from a script, it can wait for more input on standard input. Add `< /dev/null` to the command. We saw one run stop without output until we did this, and cannot say for sure that standard input was the cause.

**MCP.** Add the Ambit MCP gateway with the key as a static bearer token:

```toml
[mcp_servers.ambit]
url = "http://mcp.localtest.me:8080/mcp"
bearer_token_env_var = "AMBIT_API_KEY"
```

`codex mcp list` shows `ambit ... enabled ... Bearer token`. In the test, Codex called `time_get_current_time` (the `time` server of the quick start, step 9) and returned the time. Codex then asked the gateway for MCP resources and got `Method not found`. The gateway serves tools only. Codex shows the error and goes on.

## opencode

**With the Ambit CLI.** After you created a key, run:

```sh
ambit connect opencode --url http://llm.localtest.me:8080 --model nvidia/nemotron-3-super-120b-a12b:free
```

It asks for the key (hidden), stores it in the keychain of your computer, and adds the provider `ambit` and the `model` to `~/.config/opencode/opencode.json` (or `opencode.jsonc`, or the folder in `XDG_CONFIG_HOME`). It reads the key from the variable `AMBIT_API_KEY`, as the by-hand setup below does. opencode has no setting that runs a command to read a key. So the command also adds a function named `opencode` to your shell profile (`~/.zshrc`, `~/.bashrc` or `~/.bash_profile`, a file in `~/.config/fish/functions`, or your PowerShell profile). The function runs `ambit key print`, puts the key into `AMBIT_API_KEY` for that one opencode process only, and starts opencode. It is in a block between `# >>> ambit` and `# <<< ambit <<<` lines. The shell is detected; choose another with `--shell bash|zsh|fish|powershell`, or `--shell none` to print the function and write nothing. On Windows PowerShell 5.1 the default execution policy does not load profile scripts; connect then prints the command `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` for you to run, and does not run it itself.

Open a new terminal after the command. **The function works only when opencode is started from a shell that has it.** An editor or a launcher that starts opencode by itself does not see it, and opencode then has no key. If your file uses the opencode v2 format (a top-level `providers`, or a provider with `package` or `settings`), the command stops and changes nothing: this version of ambit writes the v1 format that was tested. Add the provider by hand then.

`ambit disconnect opencode` removes the provider, the model and the function.

**By hand.** opencode needs a custom provider that uses the OpenAI-compatible package. Put this in `opencode.json` in your project, or in `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "ambit/nvidia/nemotron-3-super-120b-a12b:free",
  "provider": {
    "ambit": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Ambit",
      "options": {
        "baseURL": "http://llm.localtest.me:8080/v1",
        "apiKey": "{env:AMBIT_API_KEY}"
      },
      "models": {
        "nvidia/nemotron-3-super-120b-a12b:free": { "name": "nvidia/nemotron-3-super-120b-a12b:free" }
      }
    }
  }
}
```

```sh
export AMBIT_API_KEY=ambit_...
opencode run "Use the read tool to read note.txt and tell me what it says."
```

The first start in a new data directory did not answer for more than 5 minutes in our test, and we stopped it. The second start answered normally. We did not find the cause. If the first start seems stuck, stop it and start again.

**MCP.** Add a remote server with the key as a header:

```json
{
  "mcp": {
    "ambit": {
      "type": "remote",
      "url": "http://mcp.localtest.me:8080/mcp",
      "headers": { "Authorization": "Bearer {env:AMBIT_API_KEY}" },
      "oauth": false
    }
  }
}
```

`opencode mcp list` shows `ambit connected`, and the model called `ambit_time_get_current_time`. The same configuration without `"oauth": false` also worked, because the gateway accepts the static header and never asks for OAuth. We keep `"oauth": false`, so that opencode does not start an OAuth sign-in if a gateway ever answers 401.

## Hermes Agent

Hermes Agent (Nous Research) uses the Chat Completions format with a custom provider. Put this in `~/.hermes/config.yaml`:

```yaml
model:
  default: nvidia/nemotron-3-super-120b-a12b:free
  provider: custom:ambit
custom_providers:
  - name: ambit
    base_url: http://llm.localtest.me:8080/v1
    key_env: AMBIT_API_KEY
    api_mode: chat_completions
```

```sh
export AMBIT_API_KEY=ambit_...
hermes chat -Q -t file -q "Use a tool to read the file note.txt in the current directory and tell me what it says."
```

**What failed first.** Our first config put `provider: custom`, `base_url` and `key_env: AMBIT_API_KEY` directly in the `model:` block. Hermes ran, sent no key, and ended with `Error: authorization failed`. The gateway answered 403. In Hermes, `key_env` works inside `custom_providers` not in the `model:` block. The config above is the one that worked. The other way, `api_key:` in the `model:` block, puts the key itself into the file, and we did not test it.

We did not test Hermes with MCP. Hermes reads MCP servers from `mcp_servers` in the same file, with `url` and `headers`; see the [Hermes MCP guide](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp). Treat that as not tested by Ambit yet.

## Not tested

- Hermes with MCP, and Hermes with the `anthropic_messages` format.
- A real Claude model behind `/v1/messages`.
- Codex with a model of an OpenAI-type provider. Only an OpenRouter model was used.
- Claude Code without `ANTHROPIC_SMALL_FAST_MODEL` or the `ANTHROPIC_DEFAULT_*_MODEL` variables.
- Any model other than `nvidia/nemotron-3-super-120b-a12b:free`.

## Check it in the console

Open **Audit** and search for your email address. Every call of the three tools is listed with the model, the token counts and the key that made it. Tool calls through the MCP gateway are listed with the tool name.

## Related

- [Connect Claude Code and Cursor](coding-tools.md): the key, Claude Code and Cursor.
- [Other coding agents](other-agents.md): snippets for agents that Ambit has not tested yet.
- [Give an agent tools and apps](mcp-apps.md): servers, tools and access rules for MCP.
