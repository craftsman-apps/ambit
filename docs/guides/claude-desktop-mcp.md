---
title: Claude Desktop with Ambit's MCP tools
description: Use the tools that Ambit gives you from Claude Desktop, with the mcp-remote bridge and a personal Ambit key sent as a header. An interim way until Ambit's gateway offers OAuth.
---

# Claude Desktop with Ambit's MCP tools

**What you get:** Claude Desktop can call the MCP tools that an Ambit access rule gives you, through the Ambit gateway. Each call is checked and recorded under your name, as for any other client.

**Managed Claude Desktop (gateway mode): use the managed option instead.** When your organization manages Claude Desktop as in [Claude Desktop with Ambit's models](claude-desktop.md), Anthropic's configuration reference has the key `managedMcpServers` (added in Claude Desktop 1.2581.0). An entry can name a remote server (`transport` `http` or `sse`, and an `https` `url`) and a `headersHelper`: a script that prints the header as a JSON object, for example `{"Authorization": "Bearer ambit_..."}`. Claude Desktop runs it before requests and reuses the result for `headersHelperTtlSec` seconds (default 300) ([configuration reference](https://claude.com/docs/third-party/claude-desktop/configuration#managedmcpservers)). The helper can read the key from the keychain, as the helper for models does in [The key](claude-desktop.md#the-key). This needs no Node.js and no `mcp-remote`. **Not tested with Ambit.** The `url` must be `https`, so the quick-start address `http://mcp.localtest.me:8080/mcp` cannot be used with it. The rest of this page is for people whose Claude Desktop is not managed. They still need `mcp-remote`.

**Tested by hand in Claude Desktop: pending.** The header and key handling of the bridge was tested on 2026-10-15 against a local stand-in MCP server (see "What was tested"). A run in Claude Desktop against a real Ambit install has not been done yet. Claude Desktop 2.37337.1 is installed on the test computer; its settings were not changed.

## How it works, and the limits

In the local file `claude_desktop_config.json`, Claude Desktop starts local programs as MCP servers and has no field for a personal key to a remote MCP address. [`mcp-remote`](https://github.com/punkpeye/mcp-remote) is a small local program that Claude Desktop starts. It forwards everything to the Ambit gateway and adds a header `Authorization: Bearer <your key>` to each request.

- **Claude Desktop only.** This works in the Claude Desktop app on macOS and Windows. It does not work in claude.ai in a browser or in the Claude mobile apps. They reach a remote connector from Anthropic's servers, with OAuth sign-in, and Ambit's MCP gateway does not offer OAuth yet. That is the issue [#97](https://github.com/craftsman-apps/ambit/issues/97). When it is done, the connector route will replace this page.
- **Your computer must reach the gateway.** The address must work from the computer that runs Claude Desktop.
- **Node.js is required.** Claude Desktop starts the bridge with `npx`. Install the LTS version from [nodejs.org](https://nodejs.org/) and check `node --version`.
- **The key stays on this computer.** Anyone who can read your user files can read the key, so protect it as in [Personal API keys](../api-keys.md).
- **`mcp-remote` is a third-party program** (MIT license; npm package [`mcp-remote`](https://www.npmjs.com/package/mcp-remote), source [github.com/punkpeye/mcp-remote](https://github.com/punkpeye/mcp-remote)). It handles your key. Pin a version, as the examples do, and change it only after you have read its release notes.
- **Tools only.** This page is about MCP tools. For models, see [Claude Desktop with Ambit's models](claude-desktop.md); the models of Ambit are a different gateway and a different key scope.

## Before you start

1. An Ambit install, and an access rule that gives your group at least one tool. If you followed [Give an agent tools and apps](mcp-apps.md), Alice has the tool `time_get_current_time`.
2. A personal API key with the scope **MCP** or **LLM and MCP**. Make it in the console under **Keys**, as in [Connect Claude Code and Cursor](coding-tools.md). The console shows the key once.
3. The MCP address of the gateway. On the quick-start cluster it is `http://mcp.localtest.me:8080/mcp`. On a production install it is the `https` address that the **MCP** tab of the console shows. These examples use the quick-start address.
4. Node.js, as above.

## 1. Put the key in a file

The `mcp-remote` option `--header-file` reads headers from a file, one `Name: value` per line. With it, the key is neither in `claude_desktop_config.json` nor in the arguments of the process, where other users of the computer can read it ([mcp-remote README, "Custom Headers"](https://github.com/punkpeye/mcp-remote#custom-headers)).

**macOS.** Replace `ambit_...` with your key. The command makes the file readable only by you:

```sh
mkdir -p ~/.ambit
printf 'Authorization: Bearer %s\n' 'ambit_...' > ~/.ambit/mcp-headers.txt
chmod 600 ~/.ambit/mcp-headers.txt
```

The file is now `/Users/<you>/.ambit/mcp-headers.txt`. Clear the command from your shell history if your shell keeps it, or type the key into the file with a text editor.

**Windows (PowerShell).** A folder under your user profile is by default not readable by other standard users:

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.ambit" | Out-Null
Set-Content -Path "$env:USERPROFILE\.ambit\mcp-headers.txt" -Value 'Authorization: Bearer ambit_...'
```

## 2. Edit claude_desktop_config.json

In Claude Desktop, open **Settings**, then **Developer**, then **Edit Config**. This opens the file, and makes it if it does not exist ([MCP: Connect to local MCP servers](https://modelcontextprotocol.io/quickstart/user)). The file is at:

| System | Path |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

Add an entry to `mcpServers`. If the file already has other servers, keep them and add this one next to them. Do not replace the file. Use the full path of the header file, not a relative path ([MCP: Connect to local MCP servers](https://modelcontextprotocol.io/quickstart/user)).

**macOS:**

```json
{
  "mcpServers": {
    "ambit": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote@0.14.3",
        "http://mcp.localtest.me:8080/mcp",
        "--allow-http",
        "--header-file",
        "/Users/<you>/.ambit/mcp-headers.txt"
      ]
    }
  }
}
```

**Windows.** Write each backslash twice in JSON:

```json
{
  "mcpServers": {
    "ambit": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote@0.14.3",
        "http://mcp.localtest.me:8080/mcp",
        "--allow-http",
        "--header-file",
        "C:\\Users\\<you>\\.ambit\\mcp-headers.txt"
      ]
    }
  }
}
```

Replace `<you>` with your user name. Two details:

- `--allow-http` is needed only for an `http://` address that is not `localhost`. `mcp-remote` otherwise stops with "Non-HTTPS URLs are only allowed for localhost or when --allow-http flag is provided". Remove the flag when your address is `https://`.
- `-y` lets `npx` download the bridge without asking, because Claude Desktop cannot answer a question. The MCP documentation uses it in the same way.
- The first part of the name, `ambit`, is the label that Claude Desktop shows. Tool names that the gateway returns start with the name of the Ambit server, for example `time_get_current_time`.

Save the file. Quit Claude Desktop completely and start it again. Closing the window is not enough ([MCP: Connect to local MCP servers](https://modelcontextprotocol.io/quickstart/user)).

### Other form: the key in an environment variable

`mcp-remote` also replaces `${NAME}` in a header with the value of the environment variable `NAME`, and the `env` object of the entry sets that variable ([mcp-remote README](https://github.com/punkpeye/mcp-remote#custom-headers)). This keeps the key out of the process arguments, but **the key is then in `claude_desktop_config.json`**. The README says that Claude Desktop on Windows mangles spaces inside `args`, so write the header without spaces around the colon and put the space in the variable:

```json
{
  "mcpServers": {
    "ambit": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote@0.14.3",
        "http://mcp.localtest.me:8080/mcp",
        "--allow-http",
        "--header",
        "Authorization:${AMBIT_AUTH}"
      ],
      "env": {
        "AMBIT_AUTH": "Bearer ambit_..."
      }
    }
  }
}
```

Use the header file when you can. The two forms are the same for the gateway.

## Check it works

1. Start Claude Desktop again. Click the "Add files, connectors, and more" plus icon in the chat input, point to **Connectors**, and choose **Manage connectors**. The entry `ambit` is listed with its tools ([MCP: Connect to local MCP servers](https://modelcontextprotocol.io/quickstart/user)). With the quick-start rule you see `time_get_current_time`. A person without a rule sees no tools.
2. Ask Claude: "What time is it in London?" Claude asks your permission to use the tool. Allow it. The answer holds the time.
3. In the Ambit console, open **Audit** and search for your email address. The call appears with the server `time` and the tool `get_current_time`.

If the entry is missing or shows an error, read the logs. They are in `~/Library/Logs/Claude` on macOS and `%APPDATA%\Claude\logs` on Windows. `mcp.log` has the connection events, and `mcp-server-ambit.log` has the output of the bridge ([MCP: Connect to local MCP servers](https://modelcontextprotocol.io/quickstart/user#troubleshooting)).

| What you see | Cause and fix |
|---|---|
| No entry, and the log says `npx` was not found | Node.js is not installed, or Claude Desktop cannot find `npx`. Install Node.js and restart Claude Desktop. |
| `Non-HTTPS URLs are only allowed for localhost or when --allow-http flag is provided` | The address starts with `http://`. Add `--allow-http`, or use the `https` address. |
| `Could not read the header file` | The path is wrong. Use the full path. `mcp-remote` stops instead of sending the request without the key. |
| The gateway answers `authorization failed` (HTTP 403) | The key is wrong, revoked, expired, or has the scope **LLM** only. Make a new key with the scope **MCP** and change the file. |
| The entry is connected but has no tools | No access rule gives your groups a tool. See [Give an agent tools and apps](mcp-apps.md). |

To test the key without Claude Desktop, use the `mcp` function in [Give an agent tools and apps](mcp-apps.md#check-it-works).

## What was tested

| Check | Result |
|---|---|
| Flags of `mcp-remote` 0.14.3: `--header`, `--header-file`, `--allow-http`, and `${NAME}` replacement in a header | Read in the package's README and code (`npm pack mcp-remote@0.14.3`, integrity `sha512-M8eMA+LQIYbVNpntDhbCzFYNxwF9O9KozOuDxOrGTtRXbHRq/m4NuFZ6892hmJcU4Zhv4qPfpxrXyG2FPWNMKg==`). The program's own `--help` lists only the address and `--debug`, so it is not a complete list. |
| `mcp-remote` 0.14.3 against a stand-in MCP server on `127.0.0.1` that printed the `Authorization` header of each request, with a made-up key | With `--header 'Authorization:Bearer ${AMBIT_KEY}'` and the variable set, with `--header 'Authorization: Bearer ${AMBIT_KEY}'` (with spaces), and with `--header-file`, every request to `/mcp` carried `Authorization: Bearer <key>`. The initialize and tools/list requests worked through the bridge. The program does not print the key. |
| The same, with `http://mcp.localtest.me:8080/mcp` and no `--allow-http` | Refused with the message in the table above. |
| `mcp-remote` before it sends the first request | It also asks for OAuth discovery addresses under `/.well-known/` without a header, and goes on when the server does not know them. How the Ambit gateway answers these requests was not checked. |
| Claude Desktop with a real Ambit install | **Pending.** Not run: the quoted config and menu names come from the MCP documentation. |

## Related

- [Claude Desktop with Ambit's models](claude-desktop.md): send the model calls of Claude Desktop through Ambit.
- [Connect Claude Code and Cursor](coding-tools.md): personal keys, and the same key in other tools.
- [Give an agent tools and apps](mcp-apps.md): the access rules that decide which tools you see.
- [Personal API keys](../api-keys.md): scopes, end dates and revoking a key.
