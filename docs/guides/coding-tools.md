---
title: Connect Claude Code, Codex, opencode, Cursor and SDKs
description: Give a person a personal API key and point Claude Code, Codex, opencode, Cursor or an SDK at Ambit, so every call is checked, counted and recorded under that person.
---

# Connect Claude Code, Codex, opencode, Cursor and SDKs

**What you get:** a coding tool uses your company's models through Ambit with a personal API key. Ambit checks the person's access, counts the use and records every call. The provider's own key stays inside the cluster.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with a model and a rule that allows everyone. The addresses below are the quick-start addresses. On your own cluster, use your own.

- LLM address: `http://llm.localtest.me:8080`
- Console: `http://console.localtest.me:8080`
- Model name: the provider's model name, not the name of the `Model` object. The console page **Models** shows it. The examples use `nvidia/nemotron-3-super-120b-a12b:free`: replace it with yours.

## 1. Create a key

1. Sign in to the console. On the quick-start cluster, use `admin@ambit.local`.
2. Open **Keys**, choose **New key**, enter a name, choose the scope **LLM** and an end date, and choose **Create**.
3. Copy the key. It starts with `ambit_`. The console shows it once and stores only a hash. If you lose it, revoke it and create a new one.

The console then shows a tab for each tool with your addresses filled in. It is the same as this page. Treat the key like a password.

## 2. Configure your tool

### With the Ambit CLI

This is the fastest way. It needs the `ambit` CLI, version 0.2.0 or later ([install it](../quick-start.md#1-install-the-cli)).

```sh
ambit connect claude-code --url http://llm.localtest.me:8080 --model nvidia/nemotron-3-super-120b-a12b:free
```

The command asks for the key from step 1 (the input is hidden), shows a diff of each file it will change, and asks `Write these changes? [y/N]`. Then it stores the key, copies each file to `<file>.ambit.bak`, writes the change, and makes one test call. Add `--dry-run` to see the diff and change nothing. In a script, use `--key-file <path>` or the variable `AMBIT_API_KEY`, and add `--yes`.

| Tool | Command | What it changes |
|---|---|---|
| Claude Code | `ambit connect claude-code` | `~/.claude/settings.json`: `apiKeyHelper` and five `env` entries. |
| Codex | `ambit connect codex` | `~/.codex/config.toml`: a block between `# >>> ambit` and `# <<< ambit <<<`. |
| opencode | `ambit connect opencode` | `opencode.json` and a function in your shell profile. See [opencode](codex-opencode-hermes.md#opencode). |
| Cursor | `ambit connect cursor` | Nothing. It prints the Cursor steps below. |

Add `--url <llm> --model <model>` to each command.

- **The key is never in a config file.** Each tool runs `ambit key print --url <llm>` when it starts. The key is in the macOS Keychain, the Windows Credential Manager, or the Linux Secret Service. See [Where the Ambit CLI keeps the key](../api-keys.md#where-the-ambit-cli-keeps-the-key).
- **Only your user-level files change**, never a project file. A second `connect` for the same address reuses the stored key.
- **Conflicts.** If the file has an entry that would defeat Ambit's (another `apiKeyHelper`, a different `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, or a Bedrock, Vertex or Foundry switch), the command stops and lists them. Run it again with `--force` to replace them. Ambit does not put the secrets back on `disconnect`: they stay in the `.ambit.bak` file. A second `--force` that would remove another such value stops, because Ambit keeps one backup per file.
- **Claude Desktop** does not run `apiKeyHelper`. See [Claude Desktop with Ambit's models](claude-desktop.md).

### By hand

**Claude Code.** Merge these values into the `"env"` object of `~/.claude/settings.json` (Windows: `%USERPROFILE%\.claude\settings.json`). Do not replace the file, and never use a project's `.claude/settings.json`: it is shared. Restart Claude Code and run `/status`: `Anthropic base URL` must show your address.

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://llm.localtest.me:8080",
    "ANTHROPIC_AUTH_TOKEN": "ambit_...",
    "ANTHROPIC_MODEL": "nvidia/nemotron-3-super-120b-a12b:free",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "nvidia/nemotron-3-super-120b-a12b:free",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "nvidia/nemotron-3-super-120b-a12b:free",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "nvidia/nemotron-3-super-120b-a12b:free"
  }
}
```

If Claude Code says it does not know the model, that is harmless. Two things to know with a model that is not a Claude model (tested with Claude Code 2.1.299 and Nemotron at OpenRouter, 2026-10-14):

- If the provider refuses the request, add `"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"` to the same `env` block. It stops two request fields (`context_management`, `safeguards`). Claude Code still sends `thinking` and `output_config`. Nemotron accepted all of it, so we could not show which provider needs the switch.
- Ambit does not serve `/v1/messages/count_tokens` and answers 403. Claude Code shows estimated token counts instead. Each refused call appears in **Audit** with status 403 under your email address, so a count of 403 entries is not an attack.

**Claude Code with MCP.** Use a key with the scope **MCP** (or **LLM and MCP**):

```sh
claude mcp add --transport http ambit http://mcp.localtest.me:8080/mcp --header "Authorization: Bearer ambit_..."
```

**Codex.** Add this to `~/.codex/config.toml`, with `model` and `model_provider` above the first `[table]` line. Codex reads the key from `AMBIT_API_KEY`.

```toml
model = "nvidia/nemotron-3-super-120b-a12b:free"
model_provider = "ambit"

[model_providers.ambit]
name = "Ambit"
base_url = "http://llm.localtest.me:8080/v1"
env_key = "AMBIT_API_KEY"
wire_api = "responses"
```

**opencode.** Merge this into `~/.config/opencode/opencode.json`. opencode reads the key from `AMBIT_API_KEY`.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "ambit/nvidia/nemotron-3-super-120b-a12b:free",
  "provider": {
    "ambit": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Ambit",
      "options": { "baseURL": "http://llm.localtest.me:8080/v1", "apiKey": "{env:AMBIT_API_KEY}" },
      "models": { "nvidia/nemotron-3-super-120b-a12b:free": { "name": "nvidia/nemotron-3-super-120b-a12b:free" } }
    }
  }
}
```

Set the variable in the terminal where you start Codex or opencode:

```sh
export AMBIT_API_KEY="ambit_..."
```

**Cursor.** Open Settings, then Models. Turn on **OpenAI API Key** and **Override OpenAI Base URL**, enter these values, and add the model name with **Add model**:

```text
OpenAI API Key: ambit_...
Override OpenAI Base URL: http://llm.localtest.me:8080/v1
```

Cursor sends requests from its own servers. The LLM address must be public HTTPS: `localhost` and `localtest.me` do not work, so the quick-start laptop cluster cannot be used. Opening the gateway to the internet means that only keys and tokens protect it.

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

## Disconnect and revoke

`ambit disconnect claude-code` (or `codex`, `opencode`, `cursor`, or `--all`) removes exactly what `ambit connect` added and keeps the rest of the file. An entry you changed after `connect` stays, and the command says so. For Codex and opencode it needs the state file `connect.json`.

**This does not revoke the key.** The key works until you revoke it in the console under **Keys**. See [Personal API keys](../api-keys.md#revoke-a-key).

## Check it works

Read the key into a variable without showing it, then call the gateway:

```sh
read -rs KEY
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

The answer ends with `HTTP 200`. With a wrong key it is `authorization failed` and `HTTP 403`. For Claude Code, call `/v1/messages` with the header `x-api-key: $KEY` and `anthropic-version: 2023-06-01` instead. Then open **Audit** in the console and search for your email address: each call is listed with the model, the token counts and the key's ID.

## Related

- [Connect Codex, opencode and Hermes Agent](codex-opencode-hermes.md) and [Other coding agents](other-agents.md): more tools.
- [Claude Desktop with Ambit's models](claude-desktop.md) and [Claude Desktop with MCP tools](claude-desktop-mcp.md): the Claude Desktop app.
- [Personal API keys](../api-keys.md): scope, end dates, limits and revoking.
- [Models and providers](../providers.md) and [Someone leaves](someone-leaves.md).
