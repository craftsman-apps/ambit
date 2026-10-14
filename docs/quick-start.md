---
title: Quick start
description: Install the ambit CLI and start a local Ambit with one command, ambit local up, with a model, an access rule and a first call.
---

# Quick start

In this guide you install the `ambit` command and start Ambit on your laptop with one command. `ambit local up` creates a local Kubernetes cluster (kind) inside Docker, installs Ambit, adds a model and an access rule, and makes a first call through the gateway. It takes from 3 to 20 minutes, depending on your network, because Ambit pulls about 1 GB of images.

This install is for testing only: plain HTTP, one gateway replica, and sign-in tokens from Ambit's own Dex. To install on a real cluster, see [Install on your cluster](install.md). To see each object that `ambit local up` creates, or to do the steps by hand, see [What ambit local up does](local-up.md).

## What you need

- Docker: Docker Desktop (macOS, or Windows with WSL 2), OrbStack or Colima (macOS), or Docker Engine (Linux). Rootless Docker and Podman are not supported.
- About 6 GB of memory for Docker. With less than 4 GB, `ambit local up` stops.
- Free ports 8080 and 8443 on your laptop. `--port` and `--https-port` choose other ports.
- Optional: a free [OpenRouter](https://openrouter.ai) API key, for a real model. Without a key, Ambit uses a test model that repeats your message.

You do not need kind, kubectl or Helm. The addresses use `localtest.me`: every name under it resolves to `127.0.0.1`, so you do not change DNS.

## 1. Install the CLI

Use one of these commands.

Homebrew (macOS and Linux):

```sh
brew install craftsman-apps/tap/ambit
```

Install script (macOS and Linux). It downloads the archive for your system, checks its SHA-256 checksum, and installs `ambit` in `~/.local/bin`:

```sh
curl -fsSL https://ambitproject.dev/install.sh | sh
```

PowerShell (Windows):

```powershell
irm https://ambitproject.dev/install.ps1 | iex
```

npm (any system with Node.js). This runs the CLI without installing it; `npm install -g @craftsman-apps/ambit` installs it:

```sh
npx @craftsman-apps/ambit local up
```

Go:

```sh
go install github.com/craftsman-apps/ambit/cmd/ambit@v0.2.0
```

Check the install: `ambit version` prints `ambit 0.2.0`. If no CLI release is published yet, the install scripts say so (`No Ambit CLI release is published yet`) and install nothing. Releases with a `-` in the tag, such as `v0.2.0-rc.1`, are published on GitHub only, not to Homebrew or npm.

The [release page](https://github.com/craftsman-apps/ambit/releases) also has an archive for each system. The binaries are not signed by Apple or Microsoft. A binary installed with the commands above runs without a warning. An archive that you download with a web browser can show a warning: on macOS, open System Settings, then Privacy & Security, and choose "Open Anyway" for `ambit`; on Windows, choose "More info" and then "Run anyway". To check that an archive was built by this project's release workflow: `gh attestation verify ambit_linux_amd64.tar.gz --repo craftsman-apps/ambit`.

## 2. Start Ambit

```sh
ambit local up
```

It asks for an OpenRouter API key and does not show what you type. Press Enter to use the test model. Without a terminal, or to skip the question, pass `--no-key`, or give the key in `OPENROUTER_API_KEY` or in a file with `--openrouter-key-file <file>`. Each step prints one line with its duration:

```text
✓ Preflight (0.4s)
✓ Cluster ambit (created) (41.2s)
✓ CRDs (Gateway API, agentgateway, ToolHive) (6.8s)
  waiting for ambit-console-6f7d9c8b7-x2k4q, ambit-dex-5d8b6c4f9-9mzlp
✓ Charts ambit-crds and ambit (3m12s)
✓ Model local-test (test model) (4.1s)
  Using a test model that repeats your message. Add a real model later: ambit local up --openrouter-key-file <file>
✓ Rule everyone-local (100ms)
✓ Test call (6.3s)
  local/echo answered: You said: Say hello in five words.

Ambit is running.
  Console:  http://console.localtest.me:8080
  Sign in:  admin@ambit.local
  Password: (shown on your terminal)
```

If a step fails, the line says what is wrong, and the next line starts with `next:` and gives the command to run. Run `ambit local up` again at any time: it repairs the cluster it created, and it never changes a kind cluster that it did not create. Press Ctrl+C once to stop `ambit local up`: it stops and says how to remove the cluster if it was created. Press Ctrl+C a second time to end it at once.

To use another cluster name, pass `--name <name>` to `ambit local up`, `ambit local down` and `ambit local password`. The kubectl context is then `kind-<name>`.

With an OpenRouter key, `ambit local up` reads OpenRouter's list of free models and uses the first one that answers, trying at most three. `--model <id>` chooses the model.

## 3. Sign in and see the call

Open <http://console.localtest.me:8080> and sign in as `admin@ambit.local` with the password from the summary. `ambit local password` prints it again. Open **Audit** and search for `admin@ambit.local`: the test call is there, with the model name and the numbers of input and output tokens.

The summary ends with a command for your shell (PowerShell on Windows) that gets a token from Ambit's Dex and calls the model, so you can make more calls. To check the install at any time:

```sh
ambit check --context kind-ambit
```

## 4. Optional: an MCP server that needs no account

A `Server` makes an MCP server available. These commands use `kubectl` (not installed by `ambit`; see [kubectl](https://kubernetes.io/docs/tasks/tools/)) and the `TOKEN` from the summary's call command. This one is a small server that tells the time. ToolHive runs it inside your cluster, and it needs no account.

```sh
kubectl --context kind-ambit -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata: { name: time }
spec:
  displayName: Time
  image: ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949
  transport: stdio
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: everyone-time }
spec:
  allSignedIn: true
  servers:
    - name: time
      tools: [get_current_time]
EOF
kubectl --context kind-ambit -n ambit-system wait servers.ambitproject.dev/time --for=condition=Ready --timeout=300s
```

The rule allows one tool, `get_current_time`. Open an MCP session and list the tools. The gateway shows each tool with the name of its server in front, so the tool appears as `time_get_current_time`.

```sh
MCP=http://mcp.localtest.me:8080/mcp
H='Accept: application/json, text/event-stream'
SID=$(curl -s -D - -o /dev/null $MCP -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H "$H" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"quick-start","version":"1"}}}' \
  | awk 'tolower($1)=="mcp-session-id:"{print $2}' | tr -d '\r')
curl -s -o /dev/null $MCP -H "Authorization: Bearer $TOKEN" -H "Mcp-Session-Id: $SID" -H 'Content-Type: application/json' -H "$H" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s $MCP -H "Authorization: Bearer $TOKEN" -H "Mcp-Session-Id: $SID" -H 'Content-Type: application/json' -H "$H" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

The answer lists the allowed tool. Real MCP clients, such as Claude Code or Cursor, do these steps for you; see [Personal API keys](api-keys.md).

## 5. Clean up

```sh
ambit local down
```

It asks before it deletes the kind cluster, and it deletes only a cluster that `ambit local up` created.

## Windows, and systems that are not tested

On Windows, `ambit local up` needs Docker Desktop with WSL 2. It is tested by hand before each release, not on every change; see [kind's notes on WSL 2](https://kind.sigs.k8s.io/docs/user/using-wsl2/). The arm64 builds for Windows and Linux are built but not tested.

## Next

- [Concepts](concepts.md): what each object does, and how Ambit checks a request.
- [Install on your cluster](install.md): company sign-in, HTTPS, your own ClickHouse and Valkey.
- [Models and providers](providers.md) and [Personal API keys](api-keys.md): other providers, and keys for Claude Code and Cursor.

The quick start installs Ambit without budgets. To use a `Budget` (daily request and spending limits), install with
`limits.enabled: true`. [Different models for different teams](guides/models-per-team.md) shows the upgrade command.
