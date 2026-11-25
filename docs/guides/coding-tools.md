---
title: Connect your coding agents
sidebar_label: Connect your coding agents
description: Point Claude Code, Cursor, Codex, opencode, Hermes Agent, Kilo Code, pi and any agent that speaks the OpenAI or Anthropic API at Ambit with a personal API key, so every call is checked, counted and recorded under that person.
---

# Connect your coding agents

Works with Claude Code, Cursor, Codex, opencode, Hermes Agent, Kilo Code, pi and any agent that speaks the OpenAI or Anthropic API.

Each person uses a personal API key. Ambit checks the person's access, counts the use and records every call. The provider's own key stays inside the cluster.

| Tool | `ambit connect` | Section |
|---|---|---|
| Claude Code | yes | [Claude Code](#claude-code) |
| Codex | yes | [Codex](#codex) |
| opencode | yes | [opencode](#opencode) |
| Cursor | yes, but it only prints the steps and writes nothing | [Cursor](#cursor) |
| Hermes Agent | yes | [Hermes Agent](#hermes-agent) |
| Kilo Code (CLI and VS Code extension) | yes | [Kilo Code](#kilo-code) |
| pi | yes | [pi](#pi) |
| Any other agent | no | [Other agents](#other-agents) |

## Before you start

You need the cluster from the [Quick start](../quick-start.md), with a model and a rule that allows everyone. The examples use the quick-start addresses. On your own cluster, use your own.

- LLM address: `http://llm.localtest.me:8080`. MCP address: `http://mcp.localtest.me:8080/mcp`.
- Model name: the provider's model name, not the name of the `Model` object. The console page **Models** shows it. The examples use `nvidia/nemotron-3-super-120b-a12b:free`.
- A key: in the console, open **Keys**, choose **Create key**, choose the scope **LLM** (**MCP** or **LLM and MCP** for MCP) and an end date. The key starts with `ambit_`. The console shows it once. Treat it like a password. The next section shows the steps.

`ambit connect <tool> --url <llm> --model <model>` needs the `ambit` CLI 0.2.0 or later ([install it](../quick-start.md#1-install-the-cli)). It asks for the key (hidden), shows a diff, and asks `Write these changes? [y/N]`. Then it stores the key in your system keychain, saves `<file>.ambit.bak`, writes the change, and makes one test call. It never changes a project file, and it writes the key to a file only for the tools that cannot read it from the keychain: opencode 2 and Kilo Code, and the MCP server of Hermes Agent (see their sections). Options: `--dry-run` shows the diff and changes nothing; `--yes` with `--key-file <path>` or `AMBIT_API_KEY` is for scripts. If the file has an entry that defeats Ambit's, the command stops and lists it; `--force` replaces it. See [Where the Ambit CLI keeps the key](../api-keys.md#where-the-ambit-cli-keeps-the-key).

Tested on 2026-10-14 with Nemotron at OpenRouter: Claude Code 2.1.299, Codex 0.159.3, opencode 1.18.34, Hermes Agent v0.13.0. opencode 2.2.5 was tested on 2026-11-17 with a mock model: `ambit connect opencode`, calls through the opencode service, a key change, and `ambit disconnect opencode`. For Hermes Agent, Kilo Code and pi, only the config files that `ambit connect` writes and removes were tested (2026-11-19), not the tools themselves. Their formats come from the documentation and source code of Hermes Agent v0.23.2, Kilo Code 7.8.30 and pi 1.5.2, read on 2026-11-19.

## Create a key and connect from the console

1. Open the console. In the quick start, the address is `http://console.localtest.me:8080`.
2. Open **Keys**, then choose **Create key**.
3. In the form **New key**, fill in **Name** (for example `my laptop`), **Scope** (**LLM**, **MCP** or **LLM and MCP**) and **End date** (**30 days from now**, **90 days from now** or **365 days from now**).
4. Choose **Create**.
5. The console shows **Key ‘<name>’ created**. It says: "This is the only time the key is shown. Store it now." Choose **Copy** under **Key** and save the key.
6. Under **Connect a tool**, if the scope includes LLM, pick the **Model name**. Then choose the tab of your tool: **Claude Code**, **Codex**, **opencode**, **Hermes Agent**, **Kilo Code**, **pi**, **Cursor**, **SDK** or **MCP**. The **opencode** tab shows opencode 2 first; choose **opencode 1** under **Version** for the older version. For a key with the scope **LLM and MCP**, the command of Hermes Agent, Kilo Code and pi also has `--mcp-url`, which adds the MCP gateway.
7. Choose **Copy** next to the command, and run it in a terminal.
8. Choose **Done**.

The key is shown only once. After **Done**, the console cannot show it again. If you lose it, revoke it and create a new key.

To see the steps again later, choose the key's name in the list on **Keys**. The key's page shows its name, scope, dates, last use and owner, the command `ambit key set --url <llm>` (it stores the key you saved in your keychain), and the same **Connect a tool** tabs. The tabs show `ambit_...` where the key goes: put in the key you saved.

The sections below are the reference for each tool.

## Claude Code

The **Claude Code** tab under **Connect a tool** gives the same command.

- `ambit connect claude-code` writes `apiKeyHelper` and five `env` entries into `~/.claude/settings.json`. Claude Desktop does not run `apiKeyHelper`; see [Claude Desktop with Ambit's models](claude-desktop.md).
- By hand: merge this into `"env"` of `~/.claude/settings.json`, never into a project file. Restart and run `/status`: `Anthropic base URL` must show your address.
- With a model that is not a Claude model, if the provider refuses the request, add `"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"`. "Unknown model" warnings are harmless.
- Ambit answers 404 for `/v1/messages/count_tokens`; it does not serve that path. Claude Code shows estimates. These 404 entries are not an attack.
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

The **Codex** tab under **Connect a tool** gives the same command.

- Codex speaks only the OpenAI Responses format (`/v1/responses`). It worked with an OpenRouter model. Try your own provider with one prompt first.
- `ambit connect codex` writes two blocks, between `# >>> ambit` and `# <<< ambit <<<`, into `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`). Codex reads the key with `ambit key print`, so the file has no key.
- By hand, Codex reads the key from `AMBIT_API_KEY`: `export AMBIT_API_KEY=ambit_...`. Codex does not allow `env_key` and the `auth` command together.
- At start, Codex asks `GET /v1/models?client_version=...` and reads the answer as `{"models": [...]}`. The gateway answers that one request with `{"models":[]}` (it needs a valid key like every other request). Before this change, Codex logged `failed to decode models response` on every run; the run still worked. Other clients get the usual OpenAI list.
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

The **opencode** tab under **Connect a tool** gives the same command. It shows the opencode 2 steps first; choose **opencode 1** for the older version.

opencode 2 sends model calls from a background service (`opencode serve --service`), not from the `opencode` process that you start in a terminal. The service does not see the environment variables of your terminal, and opencode cannot run a command to read a key. So with opencode 2 the key is in a file that only you can read.

- `ambit connect opencode` runs `opencode --version`. For opencode 2 it:
  - writes the key to `~/.config/ambit/opencode-key` (`$XDG_CONFIG_HOME/ambit/opencode-key` when that variable is set). Only your user can read the file: mode `0600`, and on Windows an access list for your user only.
  - adds the provider `ambit` and the `model` to `~/.config/opencode/opencode.json`. The provider reads the key with `"apiKey": "{file:<the key file>}"`. It uses the opencode 2 format (`providers`), or the older `provider` format when your file already uses it: opencode 2 reads both. Your other providers stay as they are.
  - adds no shell function, and removes the function that an earlier `ambit connect opencode` added for opencode 1.
- After `connect`, restart the opencode service so that it reads the new settings: `opencode service restart`.
- To change the key, run `ambit key set --url <llm>` or `ambit connect opencode --replace-key`. Both write the new key into the key file. The running service keeps the old key until you run `opencode service restart`.
- `ambit disconnect opencode` removes the provider and the model and deletes the key file.
- Back to opencode 1: run `ambit connect opencode` again. It writes the opencode 1 entry and the shell function and deletes the key file.
- If `opencode --version` does not answer (opencode is not installed, or not on your `PATH`), the command sets up opencode 2 and says so.
- The key file holds the key as plain text. Like the keychain, it keeps the key from other users of the computer, but not from programs that run as your user.
- By hand: write the key to the file, merge the provider into `opencode.json`, and restart the service. On Windows, use the **PowerShell** command of the **opencode** tab: it also limits the file to your user.

```sh
mkdir -p ~/.config/ambit && touch ~/.config/ambit/opencode-key && chmod 600 ~/.config/ambit/opencode-key && printf '%s' 'ambit_...' > ~/.config/ambit/opencode-key
opencode service restart
```

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "ambit/nvidia/nemotron-3-super-120b-a12b:free",
  "providers": {
    "ambit": {
      "name": "Ambit",
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": { "baseURL": "http://llm.localtest.me:8080/v1", "apiKey": "{file:~/.config/ambit/opencode-key}" },
      "models": { "nvidia/nemotron-3-super-120b-a12b:free": { "name": "nvidia/nemotron-3-super-120b-a12b:free" } }
    }
  }
}
```

- MCP: add a `mcp` block with `"type": "remote"`, `"url": "http://mcp.localtest.me:8080/mcp"`, `"headers": { "Authorization": "Bearer {file:~/.config/ambit/opencode-key}" }` and `"oauth": false`. opencode 1 reads `{file:...}` too.

### opencode 1

- `ambit connect opencode` adds the provider `ambit` and the `model` to `~/.config/opencode/opencode.json`. opencode cannot run a command to read a key, so the command also adds a shell function `opencode` to your shell profile. It sets `AMBIT_API_KEY` for that one process. Choose the shell with `--shell bash|zsh|fish|powershell`, or `--shell none` to write no function.
- Open a new terminal after `connect`. An editor or launcher that starts opencode itself does not see the function.
- If your file uses the opencode v2 format (a top-level `providers`), the command stops: opencode 1 does not read it.
- By hand: merge this into `opencode.json` and run `export AMBIT_API_KEY=ambit_...`. The first start in a new data directory once hung for 5 minutes; stop it and start again.

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

### opencode shows "authorization failed"

- What you see: opencode shows `authorization failed: no Ambit key or token was sent` (HTTP 403). Ambit before 0.5.0 shows only `authorization failed`; there, the gateway log shows `ambit.identity="jwt"` for the call, which means the call had no key.
- Cause: the opencode 2 background service sent no key. This happens when the provider reads the key from an environment variable (`{env:AMBIT_API_KEY}`, the opencode 1 setup): the service does not see the variable of your terminal, and opencode replaces a variable that is not set with an empty string. It also happens when the key file did not exist when the service started, and after a key change: the service keeps the old key until it restarts.
- Fix: run `ambit connect opencode` again (with opencode 2 it switches to the key file), or set up the key file by hand as above. Then run `opencode service restart`.

## Cursor

The **Cursor** tab under **Connect a tool** gives the same steps.

- `ambit connect cursor` prints the steps and writes nothing.
- Open Settings, then Models. Turn on **OpenAI API Key** and **Override OpenAI Base URL**, enter the values below, and add the model name.
- Cursor sends requests from its own servers. The LLM address must be public HTTPS: `localhost` and `localtest.me` do not work. If the gateway is open to the internet, only keys and tokens protect it.

```text
OpenAI API Key: ambit_...
Override OpenAI Base URL: http://llm.localtest.me:8080/v1
```

## Hermes Agent

The **Hermes Agent** tab under **Connect a tool** gives the same command and the steps by hand.

Hermes (Nous Research) runs a command to get a key: `key_cmd`. So `ambit connect hermes` keeps the key in the keychain, and no file holds it.

- `ambit connect hermes --url <llm> --model <model>` changes `~/.hermes/config.yaml` (`$HERMES_HOME/config.yaml` when that variable is set; on Windows without WSL, `%LOCALAPPDATA%\hermes\config.yaml`). It adds:
  - the provider `ambit` under `providers:`, with `api_mode: chat_completions` and `key_cmd: "<path of ambit> key print --url <llm>"`. Hermes runs the command when it needs the key, and again 15 minutes later. A new key is used within 15 minutes, without a restart.
  - the `model:` block with `provider: "custom:ambit"` and `default: "<model>"`. Your own `model:` block is a conflict: `--force` replaces it. The old lines stay in the file as `# ambit-replaced:` comments, and `ambit disconnect hermes` puts them back. While Ambit's block is in place, Hermes does not use the other settings of your old block (for example `context_length`).
- Ambit's lines are between the marker lines `# >>> ambit` and `# <<< ambit <<<`, as in Codex. Your other providers and settings stay.
- The Hermes gateway service (`hermes gateway install`) runs `key_cmd` too. The command holds the full path of `ambit`, so the service does not need `ambit` on its `PATH`. After a change, run `hermes gateway restart`.
- `--mcp-url <mcp>` also adds the MCP server `ambit` under `mcp_servers:`. Hermes reads MCP headers only from `config.yaml` or from environment variables (`${VAR}`), and cannot run a command for them. So the entry starts the bridge `mcp-remote@0.14.3` with `npx` and a header file, as for [Claude Desktop](claude-desktop-mcp.md). Connect writes the key to `~/.ambit/hermes-mcp-headers.txt` (mode `0600`; on Windows an access list for your user only). Node.js is needed. The key must have the scope **LLM and MCP**.
- To change the key, run `ambit key set --url <llm>` or `ambit connect hermes --replace-key`. Both write the new key into the header file. Then run `/reload-mcp` in Hermes, or restart it.
- `ambit disconnect hermes` removes Ambit's lines, puts back the lines that `--force` replaced, and deletes the header file.
- A named profile (`hermes -p <name>`) has its own folder. Set `HERMES_HOME` to that folder when you run `ambit connect hermes`.

By hand, store the key with `ambit key set --url <llm>` (it asks for the key), and merge this into `~/.hermes/config.yaml`:

```yaml
model:
  provider: custom:ambit
  default: nvidia/nemotron-3-super-120b-a12b:free

providers:
  ambit:
    base_url: http://llm.localtest.me:8080/v1
    api_mode: chat_completions
    key_cmd: ambit key print --url http://llm.localtest.me:8080
```

Without the `ambit` CLI, use `key_env: AMBIT_API_KEY` in place of `key_cmd`, and put the line `AMBIT_API_KEY=ambit_...` in `~/.hermes/.env`, the file in which Hermes keeps keys. The gateway service reads that file too. The key is then in that file as plain text.

MCP by hand: write the header file, then merge the block. Hermes replaces `${userHome}` and `${/}` with your home folder and the path separator. Leave out `"--allow-http"` for an `https` address.

```sh
mkdir -p ~/.ambit && touch ~/.ambit/hermes-mcp-headers.txt && chmod 600 ~/.ambit/hermes-mcp-headers.txt && printf 'Authorization: Bearer %s\n' 'ambit_...' > ~/.ambit/hermes-mcp-headers.txt
```

```yaml
mcp_servers:
  ambit:
    command: npx
    args: ["-y", "mcp-remote@0.14.3", "http://mcp.localtest.me:8080/mcp", "--allow-http", "--header-file", "${userHome}${/}.ambit${/}hermes-mcp-headers.txt"]
```

Known limits:

- `key_cmd` and the `providers:` section are in Hermes releases since September 2026. An older Hermes reads only the `custom_providers:` list (with `key_env`), which Ambit tested with v0.13.0 on 2026-10-14; `hermes update` moves that list into `providers:`.
- On Windows, Hermes runs `key_cmd` with `cmd.exe`, so the path of `ambit` and the address are written in double quotes (`cmd.exe` does not read single quotes, which were used for a path with a `$` or a typographic quote). If either has a `%`, `$`, backtick or double quote, connect stops, because no quoting is right for it: install `ambit` in another folder. This was not run on Windows.
- If the person's own `providers:` or `mcp_servers:` section is a list or a value on one line, connect stops and asks you to add the entries by hand.

## Kilo Code

The **Kilo Code** tab under **Connect a tool** gives the same command and the steps by hand.

The Kilo CLI (`kilo`, a fork of opencode) and the Kilo Code extension for VS Code read the same global file. The extension starts Kilo as a background process (`kilo serve`). That process does not see the environment variables of your terminal, and Kilo cannot run a command to read a key. So, as for opencode 2, the key is in a file that only you can read.

- `ambit connect kilocode --url <llm> --model <model>` writes the key to `~/.config/ambit/kilo-key` (`$XDG_CONFIG_HOME/ambit/kilo-key` when that variable is set). Only your user can read the file: mode `0600`, and on Windows an access list for your user only.
- It changes the global Kilo file in `~/.config/kilo/` (`$XDG_CONFIG_HOME/kilo/` when set; on Windows `%USERPROFILE%\.config\kilo\`): the first that exists of `kilo.jsonc`, `kilo.json`, `opencode.jsonc`, `opencode.json` and `config.json`, which is the order Kilo itself writes in, else a new `kilo.jsonc`. It adds:
  - the provider `ambit` under `provider` (`"npm": "@ai-sdk/openai-compatible"`, the format of opencode 1, which Kilo reads), with `"apiKey": "{file:<the key file>}"`.
  - `"model": "ambit/<model>"`. Another model is a conflict: `--force` replaces it, and `ambit disconnect kilocode` puts it back.
  - with `--mcp-url <mcp>`, the MCP server `ambit` under `mcp` (`"type": "remote"`, `"oauth": false`, and the header `"Authorization": "Bearer {file:<the key file>}"`). The key must have the scope **LLM and MCP**.
- After `connect`, restart Kilo so that it reads the settings and the key: in VS Code, run **Developer: Reload Window**; start the CLI again.
- To change the key, run `ambit key set --url <llm>` or `ambit connect kilocode --replace-key`. Both write the new key into the key file. Restart Kilo after the change.
- `ambit disconnect kilocode` removes the entries and deletes the key file.
- Kilo reads `{file:...}` and `{env:...}` only in the global file, not in a project's `kilo.json`. Keep the provider in the global file.

By hand: write the key to the file, merge the provider into `~/.config/kilo/kilo.jsonc`, and restart Kilo. On Windows, use the **PowerShell** command of the **Kilo Code** tab: it also limits the file to your user.

```sh
mkdir -p ~/.config/ambit && touch ~/.config/ambit/kilo-key && chmod 600 ~/.config/ambit/kilo-key && printf '%s' 'ambit_...' > ~/.config/ambit/kilo-key
```

```json
{
  "$schema": "https://app.kilo.ai/config.json",
  "model": "ambit/nvidia/nemotron-3-super-120b-a12b:free",
  "provider": {
    "ambit": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Ambit",
      "options": { "baseURL": "http://llm.localtest.me:8080/v1", "apiKey": "{file:~/.config/ambit/kilo-key}" },
      "models": { "nvidia/nemotron-3-super-120b-a12b:free": { "name": "nvidia/nemotron-3-super-120b-a12b:free" } }
    }
  },
  "mcp": {
    "ambit": {
      "type": "remote",
      "url": "http://mcp.localtest.me:8080/mcp",
      "oauth": false,
      "headers": { "Authorization": "Bearer {file:~/.config/ambit/kilo-key}" }
    }
  }
}
```

Known limits: the Kilo Code extensions for JetBrains and for the browser were not checked. A Kilo setting in the VS Code settings (not in `kilo.jsonc`) is not changed by `ambit connect`.

## pi

The **pi** tab under **Connect a tool** gives the same command and the steps by hand.

pi (the npm package `@earendil-works/pi-coding-agent`, before that `@mariozechner/pi-coding-agent`; the command is `pi`) runs a command to get a key when the `apiKey` value starts with `!`. So `ambit connect pi` keeps the key in the keychain, and no file holds it. pi has no background service.

- `ambit connect pi --url <llm> --model <model>` changes files in `~/.pi/agent/` (`$PI_CODING_AGENT_DIR` when that variable is set; on Windows `%USERPROFILE%\.pi\agent\`):
  - `models.json`: the provider `ambit` with `"api": "openai-completions"`, `"baseUrl": "<llm>/v1"`, the model, and `"apiKey": "!<path of ambit> key print --url <llm>"`. pi runs the command for each request, so a new key is used without a restart.
  - `settings.json`: `"defaultProvider": "ambit"` and `"defaultModel": "<model>"`. Other values are a conflict: `--force` replaces them, and `ambit disconnect pi` puts them back.
  - with `--mcp-url <mcp>`, `mcp.json`: the server `ambit` with `"auth": { "provider": "ambit" }`. pi then sends the key of the provider `ambit` (the output of the command) as `Authorization: Bearer`. pi does this only over `https`, or over `http` to `localhost`, `127.0.0.1` or `[::1]`; for another `http` address, connect stops. The quick-start address `http://mcp.localtest.me:8080/mcp` is such an address. pi allows `auth` only in the user file, not in a project's `.pi/mcp.json`. The key must have the scope **LLM and MCP**.
- `ambit disconnect pi` removes the entries, and deletes a file that connect made.

By hand, store the key with `ambit key set --url <llm>` (it asks for the key), and merge these into the files:

```json
{
  "providers": {
    "ambit": {
      "baseUrl": "http://llm.localtest.me:8080/v1",
      "api": "openai-completions",
      "apiKey": "!ambit key print --url http://llm.localtest.me:8080",
      "models": [{ "id": "nvidia/nemotron-3-super-120b-a12b:free" }]
    }
  }
}
```

```json
{ "defaultProvider": "ambit", "defaultModel": "nvidia/nemotron-3-super-120b-a12b:free" }
```

`~/.pi/agent/mcp.json` (an `https` MCP address):

```json
{ "mcpServers": { "ambit": { "url": "https://mcp.example.com/mcp", "auth": { "provider": "ambit" } } } }
```

Known limits:

- `auth` with a provider is in pi's source code (`mcp-servers.ts`) and is named in its MCP documentation, but the documentation has no section for it.
- On Windows, pi runs a `!` command with Git Bash when it is installed, else with `cmd.exe`, so the path of `ambit` and the address are written in double quotes, which both read. If either has a `%`, `$`, backtick or double quote, connect stops: install `ambit` in another folder. This was not run on Windows.
- An older report for pi (issue 3168 of the earlier `pi-mono` repository) says custom providers can stop responding. It was not checked with pi 1.5.2.

## Other agents

The **SDK** tab under **Connect a tool** gives the OpenAI and Anthropic variables below, and the **MCP** tab gives the MCP settings.

Not tested by Ambit. Any agent that lets you set a base URL and a key should work. The LLM gateway serves `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/embeddings` and `/v1/models`. Any other path gets 404, with or without a key: if you see 404 with "no such path on the Ambit LLM gateway", check the base URL (for example a doubled `/v1`). Send the key as `Authorization: Bearer ambit_...` or `x-api-key: ambit_...`. Clients send the provider's model name. Where there is a choice, use Chat Completions: it has the most testing in Ambit.

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
- **Cline**, **Roo Code**: provider "OpenAI Compatible", base URL `<llm>/v1`, key, model ID. Roo Code needs a model with native tool calling. [Cline](https://docs.cline.bot/provider-config/openai-compatible), [Roo Code](https://roocodeinc.github.io/Roo-Code/providers/openai-compatible).
- **Continue**: in `config.yaml`, `provider: openai`, `apiBase: <llm>/v1`, `apiKey`, `model`. Set `useResponsesApi: false` for o-series and gpt-5 names. Tab autocomplete uses `/v1/completions`, which Ambit does not serve. [Vendor page](https://docs.continue.dev/customize/model-providers/top-level/openai).
- **Aider**: `OPENAI_API_BASE=<llm>/v1`, `OPENAI_API_KEY`, and `--model openai/<model>`. [Vendor page](https://aider.chat/docs/llms/openai-compat.html).
- **Goose**: `OPENAI_HOST=<llm host>`, `OPENAI_BASE_PATH=v1/chat/completions`, `OPENAI_API_KEY`. [Vendor page](https://goose-docs.ai/docs/getting-started/providers).
- **Zed**: `language_models.openai_compatible.<name>` in `settings.json`, with `api_url: "<llm>/v1"` and an `available_models` list. [Vendor page](https://zed.dev/docs/ai/use-api-access).
- **Factory Droid**: a `customModels` entry in `~/.factory/settings.json`, with `baseUrl: "<llm>/v1"` and `provider: "generic-chat-completion-api"`. [Vendor page](https://docs.factory.com/cli/configuration/byok).
- **Qwen Code**: an entry in `modelProviders.openai` in `~/.qwen/settings.json`, with `envKey` and `baseUrl: "<llm>/v1"`. The schema differs between versions. [Vendor page](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/model-providers/).
- **Crush**: a provider with `type: "openai-compat"` and `base_url: "<llm>/v1"`. The file format is being replaced. [Project page](https://github.com/charmbracelet/crush).
- **Warp**: a custom inference endpoint with the URL `<llm>/v1` and the key. Local agent runs only. [Vendor page](https://docs.warp.dev/agents/inference/custom-inference-endpoint/).

Agents that should not work, or that we cannot say:

- **Gemini CLI and Antigravity CLI**: they speak the Gemini API, and Ambit serves none of its paths. They may send the key as `x-goog-api-key`, which Ambit does not read.
- **Amp, Windsurf, JetBrains AI Assistant, GitHub Copilot in VS Code**: unknown. We found no confirmed custom-endpoint setting.

MCP gateway: some of these agents take a URL and a header. Use `<mcp>/mcp` with `Authorization: Bearer <key>`, and read the vendor's MCP page for the setting names.

## Disconnect, check, revoke

- `ambit disconnect claude-code` (or `codex`, `opencode`, `cursor`, `hermes`, `kilocode`, `pi`, or `--all`) removes exactly what `connect` added. An entry you changed after `connect` stays, and the command says so. **It does not revoke the key.** Revoke it in the console under **Keys** ([Personal API keys](../api-keys.md#revoke-a-key)).
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
