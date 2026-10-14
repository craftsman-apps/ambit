---
title: Other coding agents
description: Settings that should connect other coding agents to Ambit. Ambit has not tested these agents yet; each row links to the vendor's own documentation.
---

# Other coding agents

**Status: not tested by Ambit yet.** The agents in the table below should work with Ambit, because they speak a format that the Ambit LLM gateway serves. The settings come from each vendor's documentation, read on 2026-10-14. Nobody has run these agents against Ambit. A vendor can change its settings at any time, so check the vendor link first.

Agents that Ambit has tested have their own pages: [Claude Code and Cursor](coding-tools.md), and [Codex, opencode and Hermes Agent](codex-opencode-hermes.md).

## What Ambit serves

The LLM gateway accepts exactly these paths: `/v1/chat/completions` (OpenAI Chat Completions), `/v1/messages` (Anthropic Messages), `/v1/responses` (OpenAI Responses), `/v1/embeddings` and `/v1/models`. Any other path is refused with 403. Send the personal key as `Authorization: Bearer ambit_...` or as `x-api-key: ambit_...`. Clients send the provider's model name.

In the settings below, `<llm>` is your LLM address (on the quick start: `http://llm.localtest.me:8080`) and `<key>` is a personal key that starts with `ambit_`. Where an agent has a choice of format, choose OpenAI Chat Completions: it is the format with the most testing in Ambit.

## Agents with settings from the vendor

| Agent | Format | Settings | Vendor documentation |
|---|---|---|---|
| GitHub Copilot CLI | Chat Completions | Environment variables `COPILOT_PROVIDER_BASE_URL=<llm>/v1`, `COPILOT_PROVIDER_API_KEY=<key>`, `COPILOT_MODEL=<model>`. The provider type is `openai` by default. The model must support tool calls and streaming. | [Use your own model key](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-byok-models) |
| Cline | Chat Completions | In the settings, choose the API provider "OpenAI Compatible". Base URL `<llm>/v1`, API key `<key>`, model ID `<model>`. | [OpenAI Compatible](https://docs.cline.bot/provider-config/openai-compatible) |
| Roo Code | Chat Completions | Same three fields as Cline: base URL `<llm>/v1`, API key, model ID. Roo Code uses native tool calling only, so the model must support it. | [OpenAI Compatible](https://roocodeinc.github.io/Roo-Code/providers/openai-compatible) |
| Kilo Code | Chat Completions | Add a custom provider with the base URL `<llm>/v1`, the API key and, if needed, custom headers. Kilo reads the model list from `/v1/models`. Choose "OpenAI Compatible", not the Responses or Anthropic option. | [OpenAI Compatible](https://kilo.ai/docs/ai-providers/openai-compatible) |
| Continue | Chat Completions | In `config.yaml`, a model with `provider: openai`, `model: <model>`, `apiBase: <llm>/v1`, `apiKey: <key>`. For o-series and gpt-5 model names Continue uses the Responses format by default; set `useResponsesApi: false` to avoid that. Tab autocomplete uses `/v1/completions`, which Ambit does not serve, so autocomplete does not work. | [OpenAI provider](https://docs.continue.dev/customize/model-providers/top-level/openai) |
| Aider | Chat Completions | Environment variables `OPENAI_API_BASE=<llm>/v1` and `OPENAI_API_KEY=<key>`, and the option `--model openai/<model>`. | [OpenAI-compatible APIs](https://aider.chat/docs/llms/openai-compat.html) |
| Goose | Chat Completions | Environment variables `OPENAI_HOST=<llm host>`, `OPENAI_BASE_PATH=v1/chat/completions`, `OPENAI_API_KEY=<key>`. Goose can also read a custom provider file in `~/.config/goose/custom_providers/` with `engine`, `base_url`, `api_key_env` and `models`. | [Providers](https://goose-docs.ai/docs/getting-started/providers) |
| Zed | Chat Completions | In `settings.json`, `language_models.openai_compatible.<name>` with `api_url: "<llm>/v1"` and an `available_models` list that you write by hand. Zed takes the key from the environment variable `<NAME>_API_KEY` or from the system keychain. | [Use your own API keys](https://zed.dev/docs/ai/use-api-access) |
| Factory Droid | Chat Completions | In `~/.factory/settings.json`, an entry in `customModels` with `model`, `baseUrl: "<llm>/v1"`, `provider: "generic-chat-completion-api"` and `apiKey`. Factory says that only the official Anthropic and OpenAI APIs are fully tested. | [Bring your own key](https://docs.factory.com/cli/configuration/byok) |
| Qwen Code | Chat Completions | In `~/.qwen/settings.json`, an entry in `modelProviders.openai` with `id`, `envKey` (the name of the variable that holds the key) and `baseUrl: "<llm>/v1"`. The schema differs between versions of the Qwen Code documentation, so compare with the version you installed. | [Model providers](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/model-providers/) |
| Crush | Chat Completions | In the Crush config file, a provider with `type: "openai-compat"`, `base_url: "<llm>/v1"` and `api_key`. The project says that the JSON file format is being replaced, so check the installed version. | [Crush on GitHub](https://github.com/charmbracelet/crush) |
| pi | Chat Completions | In the pi models file, a provider with `baseUrl: "<llm>/v1"`, `api: "openai-completions"`, `apiKey` and `models`. The project has an open report (issue 3168) that custom OpenAI-compatible providers can stop responding; we do not know if it is fixed. | [Models](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/models.md) |
| Warp | Chat Completions | In the Warp settings, add a custom inference endpoint with the URL `<llm>/v1` and the key. It works for local agent runs only, not for cloud agent runs. | [Custom inference endpoint](https://docs.warp.dev/agents/inference/custom-inference-endpoint/) |

For the MCP gateway, most of these agents take a URL and a header. Use `<mcp>/mcp` with the header `Authorization: Bearer <key>`, as in [Connect Codex, opencode and Hermes Agent](codex-opencode-hermes.md). We have not checked the exact setting names of these agents; read the vendor's MCP page.

## Agents that do not work, or that we cannot say

| Agent | Status |
|---|---|
| Gemini CLI and Antigravity CLI | Do not work today. They speak the Gemini API (`/v1beta/models/...:generateContent`) and send the key as `x-goog-api-key`. Ambit refuses those paths and does not read that header. |
| Amp, Windsurf, JetBrains AI Assistant | Unknown. We found no documented setting for a custom endpoint in Amp and Windsurf. JetBrains documents an OpenAI-compatible provider, but we could not confirm which request format and headers it uses. |
| GitHub Copilot in VS Code | Unknown. VS Code has "bring your own key" for models, but we have not confirmed the details. Inline completions never use your own key. |

## Related

- [Connect Claude Code and Cursor](coding-tools.md): create a key and use it.
- [Personal API keys](../api-keys.md): scope, end dates and revoking.
