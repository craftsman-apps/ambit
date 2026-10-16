---
title: Quick start
description: Install the ambit CLI and start a local Ambit with one command, ambit local up, with a model, an access rule and a first call.
---

# Quick start

`ambit local up` starts Ambit on your computer with one command. It takes from 3 to 20 minutes, because Ambit pulls about 1 GB of images.

- **You need:** Docker, running. Nothing else to install.
- **It uses:** Docker on your computer.
- **It creates:** a kind cluster named `ambit` (kubectl context `kind-ambit`). It installs the CRDs and the Ambit Helm charts into it, sets up a model and an access rule, and makes a test call.
- **To remove it:** `ambit local down`.

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

Each step prints one line with its duration:


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
  Password: (the password is printed here on a terminal)
  kubectl:  kubectl --context kind-ambit -n ambit-system get pods
  Remove:   ambit local down

Call the model from your shell:
  TOKEN=$(curl -s http://console.localtest.me:8080/dex/token -d grant_type=password -d client_id=ambit-cli -d scope="openid email profile" \
    --data-urlencode username=admin@ambit.local --data-urlencode password="$(ambit local password)" | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
  curl -s http://llm.localtest.me:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d '{"model":"local/echo","messages":[{"role":"user","content":"Hello"}]}'
```

### Which key does it use?

Ambit uses the first of these that it finds:

1. `--no-key`: no key. It uses the test model.
2. `--openrouter-key-file <file>`: the key in the file.
3. The `OPENROUTER_API_KEY` environment variable.
4. A key already in the cluster (the Secret `ambit-llm-openrouter`).
5. Otherwise it asks for a key. The input is hidden. Press Enter to use the test model. Without a terminal it does not ask and uses the test model.

A key from 2 or 3 replaces the key in the cluster. `--no-key` with `--openrouter-key-file` is an error.

### Other options

| Flag | What it does |
|---|---|
| `--model <id>` | Use this OpenRouter model. |
| `--no-password` | Do not print the password in the summary. |
| `--name <name>` | Use another cluster name (kubectl context `kind-<name>`). Pass it to `ambit local down` and `ambit local password` too. |

### The summary

- The password line shows the password on a terminal. Without a terminal, or with `--no-password`, it reads ``Password: run `ambit local password` ``.
- On Windows, the call command is for PowerShell.
- With an OpenRouter key, the model in the summary is the one that answered.

### OpenRouter free models

- Ambit uses the first free model on OpenRouter's list that answers. It tries at most three.
- `--model <id>` chooses the model.

### If something fails

- The failed step prints the reason. The next line starts with `next:` and gives the command to run.
- If Docker is not running, the command stops at once: `✗ Docker is not running`, then `next: start Docker Desktop` (on Linux, `next: start Docker: sudo systemctl start docker`).
- Run `ambit local up` again at any time. It repairs the cluster it created. It never changes a kind cluster that it did not create.
- Press Ctrl+C once to stop. If the cluster was created, the command says how to remove it. Press Ctrl+C a second time to end at once.

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

- Connect your coding tool: `ambit connect claude-code --url http://llm.localtest.me:8080 --model local/echo` (or `codex`, `opencode`, `cursor`). Create the key in the console first; see [Connect Claude Code and Cursor](guides/coding-tools.md).
- [Concepts](concepts.md): what each object does, and how Ambit checks a request.
- [Install on your cluster](install.md): company sign-in, HTTPS, your own ClickHouse and Valkey.
- [Models and providers](providers.md) and [Personal API keys](api-keys.md): other providers, and keys for Claude Code and Cursor.

The quick start installs Ambit without budgets. To use a `Budget` (daily request and spending limits), install with
`limits.enabled: true`. [Different models for different teams](guides/models-per-team.md) shows the upgrade command.
