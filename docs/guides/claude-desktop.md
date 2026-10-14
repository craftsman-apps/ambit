---
title: Claude Desktop with Ambit's models
description: Send the model calls of Claude Desktop through Ambit (third-party inference, gateway mode), with a personal key for each person, settings for macOS, Windows and Linux, and what is open about sign-in with an identity provider.
---

# Claude Desktop with Ambit's models

**What you get:** Claude Desktop sends its model calls to your Ambit LLM gateway instead of to Anthropic. Ambit checks the person, counts the use and records every call under that person's email address. The provider's own key stays in the cluster. Anthropic calls this "Claude Desktop on 3P" (third-party inference), provider `gateway`.

**Tested by hand in Claude Desktop: pending.** The HTTP calls that Anthropic's documentation says Claude Desktop makes in gateway mode were reproduced with `curl` against a real Ambit install on 2026-10-15, with a personal key and an OpenRouter free model (see "What was tested"). A run in Claude Desktop itself has not been done. Claude Desktop 2.37337.1 is installed on the test computer; its settings were not changed. Every statement below about what Claude Desktop does comes from Anthropic's pages, which are linked, and not from a run.

This page is about models. For the tools that Ambit gives you (MCP), see [Claude Desktop with MCP tools](claude-desktop-mcp.md). Both pages can be used together, with two keys or one key that has the scope **LLM and MCP**. When your Claude Desktop is managed as on this page, the MCP page describes a managed option (`managedMcpServers` with `headersHelper`) that can use the same key helper.

## How it works, and the limits

Anthropic's page [Deploy Claude Desktop on 3P with an LLM gateway](https://claude.com/docs/third-party/claude-desktop/gateway) says what the gateway must serve:

| What Claude Desktop needs | What Ambit does | Checked |
|---|---|---|
| `POST /v1/messages`, with streaming and tool use (required) | Serves it for models of your `Model` objects. | `curl`, 2026-10-15 |
| `GET /v1/models` (optional). If it exists, Claude Desktop lists the models it returns. If not, you set `inferenceModels`. | Serves it. It returns the models that exist in the cluster, as `{"data":[{"id":...}]}`. | `curl`, 2026-10-15 |
| The credential as `Authorization: Bearer <key>` (default) or `x-api-key: <key>` (setting `inferenceGatewayAuthScheme`) | Accepts both. A personal key starts with `ambit_`. | `curl`, 2026-10-15 |
| `cache_control` fields should reach the provider | Ambit forwards the request to the provider. Whether a provider uses the fields depends on the provider and the model. | Request with `cache_control` accepted; no cache hit seen with the free model |

What to know before you start:

- **Models are the provider's names.** Claude Desktop must send the model name that your `Model` object has in `spec.model`, for example `nvidia/nemotron-3-super-120b-a12b:free`. A request for `claude-sonnet-5` gets `404 model_not_found` unless a `Model` object has that name. Ambit has no alias from a Claude name to another model. See "Models".
- **The base address should have no `/v1`. Not tested in Claude Desktop.** Use `http://llm.localtest.me:8080` (your LLM address). Anthropic's page shows an address without `/v1` in the console table and an address with `/v1` in its Linux file example, and does not say what Claude Desktop appends. Ambit answers `403 authorization failed` for `/v1/v1/messages`, so a wrong choice looks like a key error. After the first request, look at the first row in **Audit**: its route must be `/v1/messages`.
- **Claude Desktop must reach the gateway.** The address must work from the computer that runs Claude Desktop. The quick-start address `localtest.me` works only on the same computer.
- **Token counts are estimates.** Ambit does not serve `/v1/messages/count_tokens` and answers 403. Claude Code goes on with estimates ([Connect Claude Code and Cursor](coding-tools.md)). Whether Claude Desktop does the same was not checked.
- **A model that is not a Claude model can act differently.** Tool use, thinking fields and long contexts depend on the model. Test the model you choose with the work you plan to do.

## Before you start

1. An Ambit install with a model and an access rule that allows the person's group, as in the [Quick start](../quick-start.md) or [Different models for different teams](models-per-team.md).
2. A personal API key with the scope **LLM** (or **LLM and MCP**). Make it in the console under **Keys**, as in [Connect Claude Code and Cursor](coding-tools.md#1-create-a-key). The console shows the key once.
3. The LLM address of your install. These examples use the quick-start address `http://llm.localtest.me:8080` and the model `nvidia/nemotron-3-super-120b-a12b:free`. Replace both.
4. Claude Desktop, installed from [claude.com/download](https://claude.com/download). Third-party mode needs a version that has the keys below; the keys were added in versions 1.2581.0 to 2.110.0 ([configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration)).

## The settings

Anthropic's [configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration) lists every key. These are the ones for Ambit:

| Key | Value for Ambit |
|---|---|
| `inferenceProvider` | `gateway`. Setting this key turns on third-party mode. |
| `inferenceGatewayBaseUrl` | Your LLM address, for example `http://llm.localtest.me:8080`, without `/v1`. |
| `inferenceCredentialHelper` | Absolute path of a program that prints the personal key. See "The key". |
| `inferenceCredentialHelperTtlSec` | Seconds for which Claude Desktop keeps the helper's output. Default `3600`. |
| `inferenceGatewayAuthScheme` | `bearer` (default) or `x-api-key`. Ambit accepts both for a personal key. |
| `inferenceModels` | The models to offer. The first is the default. See "Models". |
| `disableDeploymentModeChooser` | `true`: the person cannot choose the claude.ai sign-in instead of the managed settings. |
| `modelCatalogEnabled` | `false` if the computer must not fetch the published model catalog from Anthropic. It changes only the names in the model picker. |

The value of `inferenceGatewayApiKey` (one shared key in the profile) also works, but the profile is plain configuration that every person can read, and a shared key has no person behind it. Use the helper, so that each person has their own key and **Audit** shows the person.

## The key

The helper is a program that prints the key to standard output and exits with 0. Claude Desktop runs it, trims the output, and keeps it for `inferenceCredentialHelperTtlSec` seconds. It ignores static key fields when a helper is set ([`inferenceCredentialHelper`](https://claude.com/docs/third-party/claude-desktop/configuration#inferencecredentialhelper)). The output must be only the key, with no other text. If it fails, it must exit with a number that is not 0 and write the reason to standard error.

Ambit has no command that prints a key (`ambit key print` does not exist in this release), and the console shows a key only once. So each person stores their key once in the keychain of their computer, and a small script reads it. When `ambit connect` ([#95](https://github.com/craftsman-apps/ambit/issues/95)) is released, it is meant to replace the script.

Install the script in a folder that only administrators can write, for example `/usr/local/bin`, because Claude Desktop runs it as the person. Anyone who can change the script can steal the key.

**macOS.** Each person stores the key once. The command asks for the key and does not show it:

```sh
security add-generic-password -a ambit -s ambit-key -w
```

The script `/usr/local/bin/ambit-key`, with `chmod 755`:

```sh
#!/bin/sh
exec /usr/bin/security find-generic-password -a ambit -s ambit-key -w
```

It prints the key from the login keychain. If the keychain item does not exist, `security` writes `The specified item could not be found in the keychain.` and exits with 44. This script was tested with a made-up key (see "What was tested").

**Linux.** Needs `secret-tool` (package `libsecret-tools`) and a running desktop keyring. Each person stores the key once. The command asks for the key:

```sh
secret-tool store --label='Ambit key' service ambit-key
```

The script `/usr/local/bin/ambit-key`, with `chmod 755`:

```sh
#!/bin/sh
exec /usr/bin/secret-tool lookup service ambit-key
```

**Not tested.** There was no Linux computer in the test.

**Windows.** Windows has no built-in program that reads the Credential Manager, so this version keeps the key in a file that only the same user on the same computer can decrypt (Windows data protection, through PowerShell). Each person stores the key once in PowerShell:

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.ambit" | Out-Null
Read-Host 'Ambit key' -AsSecureString | ConvertFrom-SecureString | Set-Content "$env:USERPROFILE\.ambit\key.dat"
```

Two files in a folder that only administrators can write, for example `C:\ProgramData\Ambit`. `ambit-key.ps1`:

```powershell
$s = Get-Content "$env:USERPROFILE\.ambit\key.dat" | ConvertTo-SecureString
[System.Net.NetworkCredential]::new('', $s).Password
```

`ambit-key.cmd`, which starts it under an execution policy that allows local scripts:

```bat
@powershell -NoProfile -ExecutionPolicy RemoteSigned -File "C:\ProgramData\Ambit\ambit-key.ps1"
```

Set `inferenceCredentialHelperWindows` to `C:\ProgramData\Ambit\ambit-key.cmd`. Anthropic's page says Windows devices use this key in place of `inferenceCredentialHelper` ([configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration)). **Not tested.** There was no Windows computer in the test.

When the key ends or is revoked, the gateway answers 403 and the person stores a new key with the same command. Claude Desktop runs the helper again after the TTL, or when the provider answers HTTP 401 ([configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration#inferencecredentialhelper)). Ambit answers a bad key with 403, not 401, so the person may have to restart Claude Desktop. This was not checked.

## Models

If the gateway serves `GET /v1/models`, Claude Desktop lists the models it returns. Ambit returns only the models of its `Model` objects, so with one model the list is one entry. To choose the list yourself, set `inferenceModels`. Each `name` must be the exact ID that `/v1/models` returns ([`inferenceModels`](https://claude.com/docs/third-party/claude-desktop/configuration#inferencemodels)):

```json
[
  {"name": "nvidia/nemotron-3-super-120b-a12b:free", "labelOverride": "Company model"}
]
```

- `labelOverride` sets the name in the model picker.
- `anthropicFamilyTier` (`sonnet`, `opus`, `haiku`, `fable` or `mythos`) and `isFamilyDefault` are listed by Anthropic with these names and no explanation. They suggest that an entry can stand for a Claude tier in the places where Claude Desktop chooses a Claude model. Their exact effect was not checked.

Ambit does not rename models on the way. Claude Desktop sends the `name` of the entry, and the gateway needs it to be the provider's name. **Not tested in Claude Desktop.**

Which model may be used is decided by the access rules, as for any client: a person whose group has no rule gets `403 Model authorization denied` for that model ([Different models for different teams](models-per-team.md)).

## Where the settings go

Claude Desktop reads managed settings from these places. Values on macOS and Windows are strings, so write `"3600"` and `"true"`, and write lists and objects as JSON text in a string ([Deploy with MDM](https://claude.com/docs/third-party/claude-desktop/mdm#4-deploy-the-configuration) and the [configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration)).

| System | Managed settings | Local settings of one person |
|---|---|---|
| macOS | `/Library/Managed Preferences/com.anthropic.claudefordesktop.plist` (all users) or `/Library/Managed Preferences/<user>/com.anthropic.claudefordesktop.plist` (one user, wins). A `.mobileconfig` profile from your MDM writes them. | `~/Library/Application Support/Claude-3p/configLibrary/` |
| Windows | Registry values directly under `HKLM\SOFTWARE\Policies\Claude` (recommended), or under `HKCU\SOFTWARE\Policies\Claude`. If any value exists under `HKLM`, Claude Desktop ignores `HKCU`. Use `REG_SZ`. | `%LOCALAPPDATA%\Claude-3p\configLibrary\` |
| Linux | `/etc/claude-desktop/managed-settings.json`. A real JSON file, so lists and objects are JSON values. It must be a regular file owned by root and not writable by group or others, in a directory with the same rule, or Claude Desktop rejects all of it. | `~/.config/Claude-3p/configLibrary/` |

**Linux** (`/etc/claude-desktop/managed-settings.json`):

```json
{
  "inferenceProvider": "gateway",
  "inferenceGatewayBaseUrl": "http://llm.localtest.me:8080",
  "inferenceCredentialHelper": "/usr/local/bin/ambit-key",
  "inferenceModels": [
    {"name": "nvidia/nemotron-3-super-120b-a12b:free", "labelOverride": "Company model"}
  ],
  "disableDeploymentModeChooser": true
}
```

**Windows** (a `.reg` file; import it with Group Policy, Intune or your tool). The model list is JSON text, so each quote inside it has a backslash:

```text
Windows Registry Editor Version 5.00

[HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Claude]
"inferenceProvider"="gateway"
"inferenceGatewayBaseUrl"="http://llm.localtest.me:8080"
"inferenceCredentialHelper"="C:\\ProgramData\\Ambit\\ambit-key.cmd"
"inferenceCredentialHelperWindows"="C:\\ProgramData\\Ambit\\ambit-key.cmd"
"inferenceModels"="[{\"name\":\"nvidia/nemotron-3-super-120b-a12b:free\",\"labelOverride\":\"Company model\"}]"
"disableDeploymentModeChooser"="true"
```

**macOS.** The profile writes the same keys, as strings, into the domain `com.anthropic.claudefordesktop`. The simplest way to get a correct profile is Anthropic's own export: on a computer that has no managed profile, open Claude Desktop, do not sign in, choose **Help**, **Troubleshooting**, **Enable Developer Mode**, then **Developer**, **Configure Third-Party Inference…**. Enter the values from the table, choose **Apply Changes** to test them on that computer, and choose **Export** for a `.mobileconfig`, `.reg` or ADMX/plist template ([Deploy with MDM](https://claude.com/docs/third-party/claude-desktop/mdm)). Deploy the settings before the app, so that people open Claude Desktop for the first time in managed mode ([Installation and setup](https://claude.com/docs/third-party/claude-desktop/installation)). The settings are not in `~/Library` for managed devices: the window then opens read-only.

Egress: the computer must reach your LLM address. Claude Desktop also needs `downloads.claude.ai` for its workspace files, and other Anthropic hosts depending on your telemetry and update settings ([Telemetry and egress](https://claude.com/docs/third-party/claude-desktop/telemetry#required-egress-paths)). The LLM address of Ambit is the only host that receives conversation content.

## Sign-in with your identity provider (`external-idp`)

Instead of a key in the keychain, Claude Desktop can sign each person in at an OpenID Connect identity provider and send the person's token to the gateway as `Authorization: Bearer <token>` ([gateway page](https://claude.com/docs/third-party/claude-desktop/gateway#single-sign-on-with-your-identity-provider)). The keys are `inferenceCredentialKind` set to `external-idp`, and `inferenceIdpOidc` with `clientId`, `issuer` (and optionally `scopes`, `bearerTokenType`, `redirectPort`) ([configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration)). Claude Desktop discovers `<issuer>/.well-known/openid-configuration`, runs the authorization code flow with PKCE in the system browser, and sends the ID token by default (`bearerTokenType` `id_token`) or the access token (`access_token`).

**Ambit's gateway accepts such a token.** It already accepts tokens from an identity provider next to personal keys. A token must carry a usable `email` claim, unless `llm.jwt.requireEmail` is `false`. The token's person is then the person in **Audit**, in budgets and in offboarding, as for a key ([Personal API keys](../api-keys.md)).

What you set in Ambit (Helm values, [Install on your cluster](../install.md)):

| Case | Ambit values | In Claude Desktop |
|---|---|---|
| Ambit's own Dex | `llm.jwt.dex: true` | `clientId`: `ambit-cli`. `issuer`: `<console URL>/dex`, for example `http://console.localtest.me:8080/dex`. |
| Your identity provider | `llm.jwt.issuer`, `llm.jwt.audiences` (include the `clientId` of the Desktop app, for an ID token), `llm.jwt.jwksURL` | `clientId` and `issuer` of an app that you register at the provider, as a public client with the redirect `http://127.0.0.1:<port>/callback`. |

Open points, which are findings and not yet fixed:

- **No Claude Desktop client in Ambit's Dex.** The Dex of Ambit has one public client, `ambit-cli`, which is meant for the command line. Claude Desktop can use it (see the test below), but then Desktop sign-ins and command-line sign-ins share one client ID, and the access tokens of both have the audience `ambit-cli`. The chart has no value to add a client for Claude Desktop.
- **Only email and password in Ambit's own Dex.** A real install sets connectors in Dex, so sign-in goes to the company provider through Dex. This was not tested.
- **Token lifetime.** Dex answers with a refresh token when the scope `offline_access` is asked for, which is in the default scopes of Claude Desktop. How long the sign-in lasts depends on the Dex and provider settings. Not tested in Claude Desktop.
- **Offboarding.** A token stays valid until it expires. Offboarding blocks the person's email at the gateway within a few seconds, so the token is refused ([Personal API keys](../api-keys.md#offboarding)). This was not tested for tokens that Claude Desktop holds.

**Tested by hand in Claude Desktop: pending.** Claude Desktop was not used for this part. The flow was reproduced with `curl` against Ambit's own Dex (see "What was tested").

## Check it works

1. Check the settings with `curl`, before you start Claude Desktop. Put the key in a variable without showing it:

   ```sh
   read -rs KEY
   curl -s http://llm.localtest.me:8080/v1/models -H "Authorization: Bearer $KEY"
   curl -sN http://llm.localtest.me:8080/v1/messages \
     -H "Authorization: Bearer $KEY" -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
     -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","max_tokens":300,"stream":true,"messages":[{"role":"user","content":"Say hello in five words."}]}'
   ```

   The first command lists the models. The second prints server-sent events, from `event: message_start` to `event: message_stop`.
2. Start Claude Desktop, with the managed settings in place. The sign-in screen offers to start with your configuration (do not choose the claude.ai sign-in). Ask a question.
3. In the Ambit console, open **Audit** and search for the person's email address. The call is listed with the model, the token counts and the key ID.
4. If the app shows the claude.ai sign-in only, it did not read the settings. In Claude Desktop, choose **Help**, **Troubleshooting**, **Generate Diagnostic Report**, then **Export to file**. In the `.zip`, `managed-config.txt` lists the keys it read and the ones it could not parse, and `provider-status.txt` says if the provider settings are complete ([Installation and setup](https://claude.com/docs/third-party/claude-desktop/installation#verifying-the-deployment)). The log is `~/Library/Logs/Claude-3p/main.log` on macOS and `%LOCALAPPDATA%\Claude-3p\Logs\main.log` on Windows.

| What you see | Cause and fix |
|---|---|
| Claude Desktop shows the claude.ai sign-in only | `inferenceProvider` is missing or misspelled, or the app was running when the settings arrived. Quit it completely and start it again. |
| `403 authorization failed` | The key is wrong, revoked, expired, or has the scope **MCP** only. Also the base address ends in `/v1`, or the path is wrong (the gateway gives 403 for an unknown path). Make a new key with the scope **LLM**. |
| `404 model_not_found` | The model name is not the `spec.model` of a `Model` object. Change `inferenceModels`. |
| `403 Model authorization denied` | The token or key is valid, but no access rule allows the person to use the model. See [Different models for different teams](models-per-team.md). |
| The helper fails | Run it in a terminal as the person. It must print only the key and exit with 0. |
| Calls fail from other computers but work on yours | The LLM address is a quick-start address (`localtest.me`). Use a real address. |

## What was tested

Date: 2026-10-15. Ambit built from this branch on a local kind cluster, a personal key with the scope **LLM** made through the console API, and the OpenRouter model `nvidia/nemotron-3-super-120b-a12b:free`. Claude Desktop 2.37337.1 was present on the computer, and was not started and not changed. The addresses below are the LLM address of that cluster.

| Check | Result |
|---|---|
| `GET /v1/models` with `Authorization: Bearer <key>` | `200`, one model: `{"data":[{"id":"nvidia/nemotron-3-super-120b-a12b:free","object":"model",...}],"object":"list"}`. The list has no `display_name`, `has_more` or `first_id` as in Anthropic's own list format. Whether Claude Desktop needs them was not checked. |
| `POST /v1/messages` with `"stream":true`, `Authorization: Bearer`, `anthropic-version: 2023-06-01` | `200`. Server-sent events from `message_start` to `message_stop`, with `thinking_delta` before the text, then `message_delta` with the `usage` counts, and a last line `data: [DONE]` after `message_stop`. |
| The same, not streamed, with `x-api-key`, `anthropic-beta: prompt-caching-2024-07-31,interleaved-thinking-2025-05-14`, a system block with `cache_control`, a tool and `tool_choice: auto` | `200`. The model answered with `stop_reason: tool_use`. The beta header and `cache_control` were accepted. `cache_read_input_tokens` was `0`. |
| `model: claude-sonnet-5` | `404 model_not_found`. |
| A wrong key | `403 authorization failed`. |
| `POST /v1/messages/count_tokens` | `403 authorization failed`, also with a valid key. |
| `POST /v1/v1/messages` with a valid key | `403 authorization failed`. |
| **Audit** (`GET /api/audit` for the person, shown as `ambit.local`, the domain of the test person) | The streamed call, the tool call and the three token calls are listed under the person: the route `ambit-system/ambit-llm`, the model, status 200, token counts, and for key calls the key ID. The `count_tokens` call is listed with status 403. The `/v1/models` call, the `404` call and the call with a wrong key are not listed. |
| Helper script for macOS, started with an empty environment, with a made-up key in a temporary keychain item | It printed only the key and a line end, and exit 0. With the item deleted it exited with 44 and printed nothing on standard output. The item was deleted after the test. |
| Helper scripts for Linux and Windows | Not run. |
| `external-idp`: Dex sign-in with the authorization code flow and PKCE, client `ambit-cli`, redirect `http://127.0.0.1:53180/callback`, scope `openid profile email offline_access`, with `curl` as the browser | Dex accepted the loopback redirect, showed its login form, and returned a code to the redirect. The code exchange returned an ID token, an access token and a refresh token. Dex also accepts `http://localhost:<port>/callback`. |
| The ID token and the access token from that flow, and from the password grant, as `Authorization: Bearer` to `/v1/messages` and `/v1/models` | `200` for both tokens. The ID token has the issuer `<console URL>/dex`, the audience `ambit-cli` and an `email` claim. The calls are listed in **Audit** under the person. |
| Claude Desktop with a real Ambit install | **Pending.** Not run. Every statement about Claude Desktop is from Anthropic's pages. |

## Related

- [Claude Desktop with MCP tools](claude-desktop-mcp.md): the tools of Ambit in Claude Desktop, with the `mcp-remote` bridge, or the managed option for a managed Claude Desktop.
- [Connect Claude Code and Cursor](coding-tools.md): personal keys, and the same key in other tools.
- [Different models for different teams](models-per-team.md): which group may use which model.
- [Personal API keys](../api-keys.md): scope, end dates, revoking, and how the gateway checks a key or a token.
- [Install on your cluster](../install.md): the identity provider and the Helm values for tokens.
- Anthropic: [Claude Desktop on 3P](https://claude.com/docs/third-party/), [LLM gateway](https://claude.com/docs/third-party/claude-desktop/gateway), [configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration), [Deploy with MDM](https://claude.com/docs/third-party/claude-desktop/mdm), [Installation and setup](https://claude.com/docs/third-party/claude-desktop/installation).
