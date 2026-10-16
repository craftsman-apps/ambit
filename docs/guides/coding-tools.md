---
title: Connect your coding agents
sidebar_label: Connect your coding agents
description: Point Claude Code, Cursor, Codex, opencode, Hermes Agent and any agent that speaks the OpenAI or Anthropic API at Ambit with a personal API key, so every call is checked, counted and recorded under that person.
---

# Connect your coding agents

Works with Claude Code, Cursor, Codex, opencode, Hermes and any agent that speaks the OpenAI or Anthropic API.

Each person uses a personal API key. Ambit checks the person's access, counts the use and records every call. The provider's own key stays inside the cluster.

| Tool | `ambit connect` | Section |
|---|---|---|
| Claude Code | yes | [Claude Code](#claude-code) |
| Codex | yes | [Codex](#codex) |
| opencode | yes | [opencode](#opencode) |
| Cursor | yes, but it only prints the steps and writes nothing | [Cursor](#cursor) |
| Hermes Agent | no | [Hermes Agent](#hermes-agent) |
| Any other agent | no | [Other agents](#other-agents) |

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with a model and a rule that allows everyone. The examples use the quick-start addresses. On your own cluster, use your own.

- LLM address: `http://llm.localtest.me:8080`. MCP address: `http://mcp.localtest.me:8080/mcp`.
- Model name: the provider's model name, not the name of the `Model` object. The console page **Models** shows it. The examples use `nvidia/nemotron-3-super-120b-a12b:free`.
- A key: in the console, open **Keys**, choose **New key**, choose the scope **LLM** (**MCP** or **LLM and MCP** for MCP) and an end date. The key starts with `ambit_`. The console shows it once. Treat it like a password.

`ambit connect <tool> --url <llm> --model <model>` needs the `ambit` CLI 0.2.0 or later ([install it](../quick-start.md#1-install-the-cli)). It asks for the key (hidden), shows a diff, and asks `Write these changes? [y/N]`. Then it stores the key in your system keychain, saves `<file>.ambit.bak`, writes the change, and makes one test call. It never writes the key to a file and never changes a project file. Options: `--dry-run` shows the diff and changes nothing; `--yes` with `--key-file <path>` or `AMBIT_API_KEY` is for scripts. If the file has an entry that defeats Ambit's, the command stops and lists it; `--force` replaces it. See [Where the Ambit CLI keeps the key](../api-keys.md#where-the-ambit-cli-keeps-the-key).

Tested on 2026-10-14 with Nemotron at OpenRouter: Claude Code 2.1.299, Codex 0.159.3, opencode 1.18.34, Hermes Agent v0.13.0.

## Claude Code

- `ambit connect claude-code` writes `apiKeyHelper` and five `env` entries into `~/.claude/settings.json`. Claude Desktop does not run `apiKeyHelper`; see [Claude Desktop with Ambit's models](claude-desktop.md).
- By hand: merge this into `"env"` of `~/.claude/settings.json`, never into a project file. Restart and run `/status`: `Anthropic base URL` must show your address.
- With a model that is not a Claude model, if the provider refuses the request, add `"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"`. "Unknown model" warnings are harmless.
- Ambit answers 403 for `/v1/messages/count_tokens`. Claude Code shows estimates. These 403 entries in **Audit** are not an attack.
- MCP: `claude mcp add --transport http ambit http://mcp.localtest.me:8080/mcp --header "Authorization: Bearer ambit_..."`

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

## Codex

- Codex speaks only the OpenAI Responses format (`/v1/responses`). It worked with an OpenRouter model. Try your own provider with one prompt first.
- `ambit connect codex` writes two blocks, between `# >>> ambit` and `# <<< ambit <<<`, into `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`). Codex reads the key with `ambit key print`, so the file has no key.
- By hand, Codex reads the key from `AMBIT_API_KEY`: `export AMBIT_API_KEY=ambit_...`. Codex does not allow `env_key` and the `auth` command together.
- Put `model` and `model_provider` above the first `[table]` line. The warning "Model metadata not found" is harmless. In a script, add `< /dev/null` to `codex exec`.
- MCP: add the block below. The gateway serves tools only, so Codex's request for MCP resources gets `Method not found`.

```toml
model = "nvidia/nemotron-3-super-120b-a12b:free"
model_provider = "ambit"

[model_providers.ambit]
name = "Ambit"
base_url = "http://llm.localtest.me:8080/v1"
env_key = "AMBIT_API_KEY"
wire_api = "responses"

[mcp_servers.ambit]
url = "http://mcp.localtest.me:8080/mcp"
bearer_token_env_var = "AMBIT_API_KEY"
```

## opencode

- `ambit connect opencode` adds the provider `ambit` and the `model` to `~/.config/opencode/opencode.json`. opencode cannot run a command to read a key, so the command also adds a shell function `opencode` to your shell profile. It sets `AMBIT_API_KEY` for that one process. Choose the shell with `--shell bash|zsh|fish|powershell`, or `--shell none` to write no function.
- Open a new terminal after `connect`. An editor or launcher that starts opencode itself does not see the function.
- If your file uses the opencode v2 format (a top-level `providers`), the command stops. Add the provider by hand.
- By hand: merge this into `opencode.json` and run `export AMBIT_API_KEY=ambit_...`. The first start in a new data directory once hung for 5 minutes; stop it and start again.
- MCP: add a `mcp` block with `"type": "remote"`, `"url": "http://mcp.localtest.me:8080/mcp"`, `"headers": { "Authorization": "Bearer {env:AMBIT_API_KEY}" }` and `"oauth": false`.

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

## Cursor

- `ambit connect cursor` prints the steps and writes nothing.
- Open Settings, then Models. Turn on **OpenAI API Key** and **Override OpenAI Base URL**, enter the values below, and add the model name.
- Cursor sends requests from its own servers. The LLM address must be public HTTPS: `localhost` and `localtest.me` do not work. If the gateway is open to the internet, only keys and tokens protect it.

```text
OpenAI API Key: ambit_...
Override OpenAI Base URL: http://llm.localtest.me:8080/v1
```

## Hermes Agent

- Hermes (Nous Research) uses the Chat Completions format. There is no `ambit connect` for it.
- Put the config below in `~/.hermes/config.yaml` and run `export AMBIT_API_KEY=ambit_...`.
- `key_env` works inside `custom_providers`, not in the `model:` block. Our first config used the `model:` block, sent no key and got 403. Putting `api_key:` in the `model:` block writes the key into the file; we did not test it.
- MCP was not tested. Hermes reads `mcp_servers` with `url` and `headers`; see the [Hermes MCP guide](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp).

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

## Other agents

Not tested by Ambit. Any agent that lets you set a base URL and a key should work. The LLM gateway serves `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/embeddings` and `/v1/models`. Any other path gets 403. Send the key as `Authorization: Bearer ambit_...` or `x-api-key: ambit_...`. Clients send the provider's model name. Where there is a choice, use Chat Completions: it has the most testing in Ambit.

```sh
# OpenAI SDK and OpenAI-compatible tools (Chat Completions)
export OPENAI_BASE_URL=http://llm.localtest.me:8080/v1
export OPENAI_API_KEY=ambit_...

# Anthropic SDK (Messages)
export ANTHROPIC_BASE_URL=http://llm.localtest.me:8080
export ANTHROPIC_API_KEY=ambit_...
```

Settings from vendor documentation, read on 2026-10-14. Nobody has run these against Ambit. Settings change, so check the vendor page first. `<llm>` is your LLM address.

- **GitHub Copilot CLI**: `COPILOT_PROVIDER_BASE_URL=<llm>/v1`, `COPILOT_PROVIDER_API_KEY`, `COPILOT_MODEL`. The model must support tool calls and streaming. [Vendor page](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-byok-models).
- **Cline**, **Roo Code**, **Kilo Code**: provider "OpenAI Compatible", base URL `<llm>/v1`, key, model ID. Roo Code needs a model with native tool calling. [Cline](https://docs.cline.bot/provider-config/openai-compatible), [Roo Code](https://roocodeinc.github.io/Roo-Code/providers/openai-compatible), [Kilo Code](https://kilo.ai/docs/ai-providers/openai-compatible).
- **Continue**: in `config.yaml`, `provider: openai`, `apiBase: <llm>/v1`, `apiKey`, `model`. Set `useResponsesApi: false` for o-series and gpt-5 names. Tab autocomplete uses `/v1/completions`, which Ambit does not serve. [Vendor page](https://docs.continue.dev/customize/model-providers/top-level/openai).
- **Aider**: `OPENAI_API_BASE=<llm>/v1`, `OPENAI_API_KEY`, and `--model openai/<model>`. [Vendor page](https://aider.chat/docs/llms/openai-compat.html).
- **Goose**: `OPENAI_HOST=<llm host>`, `OPENAI_BASE_PATH=v1/chat/completions`, `OPENAI_API_KEY`. [Vendor page](https://goose-docs.ai/docs/getting-started/providers).
- **Zed**: `language_models.openai_compatible.<name>` in `settings.json`, with `api_url: "<llm>/v1"` and an `available_models` list. [Vendor page](https://zed.dev/docs/ai/use-api-access).
- **Factory Droid**: a `customModels` entry in `~/.factory/settings.json`, with `baseUrl: "<llm>/v1"` and `provider: "generic-chat-completion-api"`. [Vendor page](https://docs.factory.com/cli/configuration/byok).
- **Qwen Code**: an entry in `modelProviders.openai` in `~/.qwen/settings.json`, with `envKey` and `baseUrl: "<llm>/v1"`. The schema differs between versions. [Vendor page](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/model-providers/).
- **Crush**: a provider with `type: "openai-compat"` and `base_url: "<llm>/v1"`. The file format is being replaced. [Project page](https://github.com/charmbracelet/crush).
- **pi**: a provider with `baseUrl: "<llm>/v1"` and `api: "openai-completions"`. An open report (issue 3168) says custom providers can stop responding. [Vendor page](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/models.md).
- **Warp**: a custom inference endpoint with the URL `<llm>/v1` and the key. Local agent runs only. [Vendor page](https://docs.warp.dev/agents/inference/custom-inference-endpoint/).

Agents that should not work, or that we cannot say:

- **Gemini CLI and Antigravity CLI**: they speak the Gemini API, and Ambit serves none of its paths. They may send the key as `x-goog-api-key`, which Ambit does not read.
- **Amp, Windsurf, JetBrains AI Assistant, GitHub Copilot in VS Code**: unknown. We found no confirmed custom-endpoint setting.

MCP gateway: some of these agents take a URL and a header. Use `<mcp>/mcp` with `Authorization: Bearer <key>`, and read the vendor's MCP page for the setting names.

## Disconnect, check, revoke

- `ambit disconnect claude-code` (or `codex`, `opencode`, `cursor`, or `--all`) removes exactly what `connect` added. An entry you changed after `connect` stays, and the command says so. **It does not revoke the key.** Revoke it in the console under **Keys** ([Personal API keys](../api-keys.md#revoke-a-key)).
- Check: read the key without showing it, then call the gateway. The answer ends with `HTTP 200`. A wrong key gives `HTTP 403`. For Claude Code, call `/v1/messages` with `x-api-key: $KEY` and `anthropic-version: 2023-06-01`.

```sh
read -rs KEY
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

Then open **Audit** in the console and search for your email address. Each call is listed with the model, the token counts and the key's ID.

Not tested: a real Claude model behind `/v1/messages`; Codex with an OpenAI-type provider; Claude Code without the `ANTHROPIC_DEFAULT_*_MODEL` variables; any model other than Nemotron.

## Related

- [Claude Desktop with Ambit's models](claude-desktop.md) and [Claude Desktop with MCP tools](claude-desktop-mcp.md).
- [Personal API keys](../api-keys.md), [Give an agent tools and apps](mcp-apps.md), [Models and providers](../providers.md), [Someone leaves](someone-leaves.md).
