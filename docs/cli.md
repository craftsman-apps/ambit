---
title: CLI reference
description: Every command, flag and exit code of the ambit CLI, the checks of ambit check, and the permissions they need.
---

# CLI reference

`ambit` starts a local Ambit on kind, checks an install, validates Ambit objects in files, and connects coding tools to Ambit. To install it, see [Quick start](quick-start.md). This page describes release 0.2.0.

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--kubeconfig <file>` | `$KUBECONFIG`, else `~/.kube/config` | kubeconfig file |
| `--context <name>` | the current context | kubeconfig context |
| `-n`, `--namespace <name>` | `ambit-system` | namespace of the Ambit releases |
| `--yes` | off | do not ask for confirmation. Required without a terminal. |
| `--output text\|json` | `text` | `json` only for `ambit check` and `ambit version`. Then stdout holds only the JSON; other lines go to stderr. |
| `--no-color` | off | no colour. Colour is used only on a terminal, and never when `NO_COLOR` is set. |
| `--verbose` | off | also print the logs of the Helm and kind libraries |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The command did what it says. For `ambit check`: no check failed (warnings and unknown results are allowed). |
| 1 | A step failed, or (for `ambit check`) at least one check failed. |
| 2 | Usage error: a bad flag, a missing file, a `--context` or `--kubeconfig` that you named and that does not exist (also for `ambit check`), or no `--yes` where the command must ask but there is no terminal. |

Every failure of `ambit local up` and the other commands prints a line `✗ <what> — <why>` and, on the next line, `next:` with the command to run or a page to read. If Docker is not running, `ambit local up` stops before it changes anything: the line reads `✗ Docker is not running — <why>`, and `next:` says `start Docker Desktop` (macOS and Windows) or `start Docker: sudo systemctl start docker` (Linux).

## Secrets in the output

The CLI never prints the OpenRouter key, the admin password or tokens: one output layer replaces them with `[redacted]` in text, JSON and `--verbose` output, also inside error messages from the libraries it uses. Keys are read from a hidden prompt (the question is written to stderr, so it stays on the terminal when you redirect stdout, for example `ambit local up > log`), an environment variable or a file, never from a flag value, so they do not end up in your shell history. The local admin password is printed by `ambit local up` only on a terminal (`--no-password` hides it there too), and by `ambit local password`, whose purpose is to print it.

## ambit local up

Creates a local Ambit on a kind cluster, or repairs one that `ambit local up` created. Press Ctrl+C once to stop (the command says how to remove a cluster it created); press it a second time to end the process at once. Steps: preflight (the `L` checks below), cluster, the third-party CRDs, the charts `ambit-crds` and `ambit` (with progress every 10 seconds), a model, the access rule `everyone-local`, a test call, and a summary. See [Install locally by hand](install-by-hand.md) for each object.

| Flag | Default | Meaning |
|---|---|---|
| `--name <name>` | `ambit` | name of the kind cluster; the kubectl context is `kind-<name>`. `ambit local down` and `ambit local password` take it too. |
| `--port <port>` | `8080` | port on `127.0.0.1` for the console and the gateway |
| `--https-port <port>` | `8443` | port on `127.0.0.1` for HTTPS |
| `--openrouter-key-file <file>` | | file with an OpenRouter API key (one line). `OPENROUTER_API_KEY` works too. For the order, see [Which key does it use?](quick-start.md#which-key-does-it-use). |
| `--no-key` | off | use the test model that repeats your message; do not ask for a key |
| `--model <id>` | automatic | OpenRouter model id, instead of the first free model that answers |
| `--no-password` | off | do not print the admin password in the summary |
| `--timeout <duration>` | `15m` | how long to wait for the pods |
| `--skip-checks` | off | turn the memory failure of preflight into a warning |
| `--chart-dir <dir>` | | install the charts from `charts/` of a checkout. Required for a build from source (`ambit version` says `dev`). |
| `--chart-repo <repository>` | `oci://ghcr.io/craftsman-apps/charts` | a mirror of the chart registry |
| `--registry-config <file>` | Helm's | Helm registry login file, for a private registry |

The CLI uses only the kubectl context `kind-<name>` and gives back your previous current context after kind creates the cluster. It keeps an existing OpenRouter Secret on a repair unless you give a new key. Proxies: the CLI and the kind node use `HTTPS_PROXY` and `NO_PROXY` from the environment.

## ambit local down

Deletes the kind cluster that `ambit local up` created, after it asks (`--yes` skips the question). Flag: `--name`. It changes nothing else.

## ambit local password

Prints the password of `admin@ambit.local` of the local cluster. Flag: `--name`.

`local up` repair, `local down` and `local password` act only on a cluster with both marks of `ambit local up`, a state file and a node label; see [A cluster that ambit did not create](install-by-hand.md#a-cluster-that-ambit-did-not-create).

## ambit connect

```sh
ambit connect <claude-code|codex|opencode|cursor> --url <llm address> --model <model id> [flags]
ambit connect <hermes|kilocode|pi> --url <llm address> --model <model id> [--mcp-url <mcp address>] [flags]
ambit connect claude-desktop-mcp --url <mcp address> [flags]
```

Adds Ambit's entries to the user-level config file of a coding tool, stores the key in the keychain of your computer, and ends with one test call in the tool's own request format. It never writes the key to a config file, and it never changes a project file. For what it writes for each tool, see [Connect your coding agents](guides/coding-tools.md#before-you-start). For `cursor` it prints the steps and writes nothing.

For `opencode` it runs `opencode --version` first. opencode 2 sends model calls from a background service that does not see your terminal, so for opencode 2 connect writes the key to the key file `~/.config/ambit/opencode-key` (`$XDG_CONFIG_HOME/ambit/opencode-key` when that variable is set; mode `0600`, readable only by you on Windows too) and the provider reads it with `{file:<path>}`. It writes the opencode 2 format (`providers`), or the older `provider` format when the file uses it; other providers stay. It adds no shell function, removes the function that an earlier connect added for opencode 1, and ends with the line to restart the service: `opencode service restart`. Like the header file of `claude-desktop-mcp`, the key file's content is never printed and the state file records only its SHA-256 hash. When `opencode --version` does not answer, connect sets up opencode 2 and says so. For opencode 1 it writes the `provider` format with `{env:AMBIT_API_KEY}` and adds a shell function that sets the variable, as before; it replaces the entry of an earlier connect for opencode 2 and deletes that connect's key file (a key file changed after connect stays, with a warning).

`hermes`, `kilocode` and `pi` add Ambit as an OpenAI-compatible provider (Chat Completions) and make it the default model, as in [Connect your coding agents](guides/coding-tools.md#hermes-agent):

- `hermes` changes `$HERMES_HOME/config.yaml` (default `~/.hermes/config.yaml`; on Windows `%LOCALAPPDATA%\hermes\config.yaml`). The provider `ambit` under `providers:` reads the key with `key_cmd: "<path of ambit> key print --url <llm>"`. Ambit's lines are marked blocks, as in Codex; a `model:` block that `--force` replaces stays in the file as `# ambit-replaced:` comments and comes back at disconnect. A YAML file that does not parse, or a `providers:` or `mcp_servers:` section that is not a block mapping, stops the command.
- `kilocode` writes the key to `~/.config/ambit/kilo-key` (`$XDG_CONFIG_HOME/ambit/kilo-key` when that variable is set; mode `0600`, readable only by you on Windows too) and changes the global Kilo file in `$XDG_CONFIG_HOME/kilo/` or `~/.config/kilo/` (the first that exists of `kilo.jsonc`, `kilo.json`, `opencode.jsonc`, `opencode.json`, `config.json`, else a new `kilo.jsonc`). The provider `ambit` reads the key with `{file:<path>}`: the Kilo Code extension for VS Code starts Kilo in the background, where the environment of your terminal is not seen, and Kilo cannot run a command for the key.
- `pi` changes `$PI_CODING_AGENT_DIR` (default `~/.pi/agent/`): the provider `ambit` in `models.json` with `"apiKey": "!<path of ambit> key print --url <llm>"`, and `defaultProvider` and `defaultModel` in `settings.json`.
- `--mcp-url <mcp address>` adds the MCP server `ambit` with the same key, which must have the scope LLM and MCP. Hermes reads MCP headers only from its config or from environment variables, so its entry is the `mcp-remote@0.14.3` bridge with the header file `~/.ambit/hermes-mcp-headers.txt` (mode `0600`; Node.js is needed). Kilo Code gets `mcp.ambit` with the header `Bearer {file:<key file>}`. pi gets `mcpServers.ambit` in `mcp.json` with `"auth": { "provider": "ambit" }`, which sends the key of the provider `ambit`; pi allows that only over `https` or to `localhost`, so connect stops for another `http` address.

`claude-desktop-mcp` connects Claude Desktop to Ambit's MCP tools, as in [Claude Desktop with Ambit's MCP tools](guides/claude-desktop-mcp.md). `--url` is the MCP address (for example `http://mcp.localtest.me:8080/mcp`) and `--model` is not used. It adds the entry `mcpServers.ambit` (the `mcp-remote@0.14.3` bridge started with `npx`, with `--allow-http` for an `http` address whose host is not `localhost`, `127.0.0.1` or `::1`) to `claude_desktop_config.json` (macOS `~/Library/Application Support/Claude/`, Windows `%APPDATA%\Claude\`, Linux `$XDG_CONFIG_HOME/Claude/` or `~/.config/Claude/`). It writes the key to the header file `~/.ambit/mcp-headers.txt` (mode `0600`, readable only by you on Windows too), because `mcp-remote` reads the key only from a file or from the config. It is one of the files `ambit connect` writes that hold the key (the others are the opencode 2 and Kilo Code key files and the Hermes header file): its content is never printed, and the state file records only its SHA-256 hash. A header file that Ambit did not write is a conflict. It makes no test call: restart Claude Desktop and check Connectors. `ambit disconnect claude-desktop-mcp` removes the entry and deletes the header file while it still holds what connect wrote.

Steps: read and parse the files (a file that does not parse stops the command, nothing is changed), find conflicts, get the key, print the diff of every file, ask `Write these changes? [y/N]`, store the key, write the files (each existing file is first copied to `<file>.ambit.bak`, mode `0600`, only the latest copy is kept), write the state file, make the test call. If a write fails after another file was written, the written files are put back.

| Flag | Meaning |
|---|---|
| `--url <address>` | Required. The LLM gateway address, for example `https://llm.example.com`. It is normalised: scheme and host in lower case, no trailing `/` and no trailing `/v1`. Must be `http` or `https`. |
| `--model <id>` | Required except for `cursor` and `claude-desktop-mcp`. The model ID that clients send (the provider's ID). |
| `--console-url <address>` | The console address. It is saved so that `ambit disconnect` can print the page where you revoke the key. |
| `--key-file <path>` | Read the key from this file. Without it: `AMBIT_API_KEY`, then a hidden prompt. A key is never a flag value. |
| `--replace-key` | A key is stored for this address: ask for a new one and replace it. Without the flag the stored key is reused and nothing is asked. |
| `--key-store keychain\|file` | Force a store. Default: the keychain, and a `0600` file only on Linux or BSD without a Secret Service (with a warning). |
| `--mcp-url <address>` | `hermes`, `kilocode` and `pi` only (exit 2 for another tool): also add the MCP gateway at this address as the MCP server `ambit`. |
| `--shell bash\|zsh\|fish\|powershell\|none` | opencode 1 only: the shell that gets the function. Default: detected. `none` prints the function and writes nothing. opencode 2 gets no function, so the flag is not used. |
| `--dry-run` | Print the diff of every file and the key action. Write nothing, store nothing, delete nothing, ask for no key and make no test call. Exit 1 when the real run would stop (a conflict, a file that does not parse, an opencode v2 file with opencode 1), else 0. |
| `--force` | Replace a conflicting entry that Ambit did not write. The old value is saved for `ambit disconnect`, except these, which stay only in the backup file and are not put back: `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY` and an old `provider.ambit` or `providers.ambit` of opencode, an old `provider.ambit` or `mcp.ambit` of Kilo Code, and an old `providers.ambit` or `mcpServers.ambit` of pi. In Hermes' `config.yaml`, replaced lines stay as `# ambit-replaced:` comments, as in Codex. |
| `--no-test` | Skip the test call. |

On Windows, `ambit connect opencode` for opencode 1 asks PowerShell for its execution policy. Windows PowerShell 5.1 starts with the policy `Restricted`, and under `Restricted` or `AllSigned` it does not load the profile that holds the function. Then connect still writes the function, prints a `warn` line, and prints `next: Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`. Ambit never changes the policy: run that command yourself, or use `--shell none`.

The key is checked for the format `ambit_` and 43 letters and digits. If the same command ran before and nothing changed, it prints `✓ <tool> is already connected to <url>` and makes only the test call. If the test call fails, the config stays; the output says what failed and ends with `to undo: ambit disconnect <tool>`. When a connect moves a tool to a new `--url` and no other tool uses the key of the old address, that key is deleted from the keychain, with the line that reminds you to revoke it.

`connect`, `disconnect` and `key set` take the lock file `connect.json.lock` next to the state file, so two runs at the same time do not lose a record. A second run waits up to 10 seconds, then stops with exit 1. The lock is the operating system's file lock, so it ends with the process, also after Ctrl-C or a crash; the file itself stays.

## ambit disconnect

```sh
ambit disconnect <claude-code|codex|opencode|cursor|claude-desktop-mcp|hermes|kilocode|pi> [flags]
ambit disconnect --all [flags]
```

Removes exactly what `ambit connect` added, and keeps every other byte of the file when nothing changed in between. For Codex, opencode and Hermes this needs the state file `connect.json`: without it, the blank line that connect added before a block stays. An entry that you changed after connect stays, with a `!` line. A tool that is not recorded in the state file prints `✓ <tool> is not connected (nothing recorded)`; for Codex, opencode and Hermes, a marked block that is found is still removed. For opencode 2 and Kilo Code it also deletes the key file, and for Hermes the header file, while it still holds what connect wrote. When no other tool uses the key, the stored key is deleted. That does not revoke it: the last line says to revoke it in the console. With `--revoke`, each key that is deleted is first revoked at the console (`DELETE /api/keys/self`, see [Personal API keys](api-keys.md#revoke-a-key)). A key the console does not know (revoked or stopped already) gives a warning and is deleted. Any other failure (network, `429`, `5xx`) keeps the stored key and exits 1; the tools are already disconnected, and `ambit disconnect --all --revoke` finishes the work. A key stored with `ambit key set` is deleted only by `--all`.

| Flag | Meaning |
|---|---|
| `--all` | Every tool recorded in the state file. It cannot be used with a tool name (exit 2). |
| `--console-url <address>` | Overrides the console address saved at connect, for the last line. |
| `--keep-key` | Do not delete the stored key, even when no tool uses it. |
| `--revoke` | Revoke each key that is deleted, at the console. Needs the console address (`--console-url`, or the one saved at connect; else exit 2 and nothing changes). Cannot be used with `--keep-key` (exit 2). `--dry-run` prints `would revoke the key for <address> at <console>` and calls nothing. |
| `--dry-run` | Print the diffs and change nothing. |

## ambit key print

```sh
ambit key print --url <llm address> [--reveal]
```

Prints the key that `ambit connect` stored for the address to standard output, with a line end and nothing else. The tools that `ambit connect` configures run this command each time they start. It reads the keychain or the key file only: no network and no cluster. On a terminal it refuses (exit 2) unless you add `--reveal`. If no key is stored, it exits with 1 and writes the reason to standard error. It prints the key whenever its output is not a terminal (that is how the tools read it), so do not run it where its output is logged. It is the only command that prints a key as it is. See [Where the Ambit CLI keeps the key](api-keys.md#where-the-ambit-cli-keeps-the-key) for what the keychain protects and what it does not.

## ambit key set

```sh
ambit key set --url <address> [--key-file <path>] [--key-store keychain|file] [--console-url <address>]
```

Stores a key for the address without connecting a tool, so that `ambit key print --url <address>` prints it. Use it for a helper that runs `ambit key print`, for example the `headersHelper` of a managed Claude Desktop ([Claude Desktop with Ambit's MCP tools](guides/claude-desktop-mcp.md)). The key comes from `--key-file`, else `AMBIT_API_KEY`, else a hidden prompt, and must be in the Ambit format. A key already stored for the address is replaced; when the old key was in the other store, it is deleted there. It prints `✓ key for <address> stored in <store>`, and never the key. A file that holds the key and that `ambit connect` wrote for the address (the opencode 2 and Kilo Code key files, the `claude-desktop-mcp` and Hermes header files) gets the new key too, with the line `✓ wrote the new key to <file>`; a file that was changed after connect keeps its content, with a warning. A running opencode service keeps the old key until you run `opencode service restart`; restart Kilo Code, and run `/reload-mcp` in Hermes. Hermes and pi read the key with `ambit key print`, so their model calls use the new key without a change. A later `ambit connect` to the same address reuses the key. `ambit disconnect --all` deletes a stored key that no tool uses.

`connect`, `disconnect`, `key set` and `key print` print text only: `--output json` is a usage error (exit 2). They use the exit codes 0, 1 and 2, and never 3. A failure, including an error text from the operating system, goes through the same redaction as the other commands.

| Case | Exit |
|---|---|
| Done, including "already connected", warnings, and a `--dry-run` that finds no problem | 0 |
| Unknown tool, missing `--url` or `--model`, `--all` with a tool name, a bad `--url`, a key that is not in the Ambit format, `--output json`, an unknown flag, `--mcp-url` with a tool other than `hermes`, `kilocode` and `pi`, an `http` `--mcp-url` for `pi` whose host is not `localhost`, `--revoke` without a console address or with `--keep-key`; no terminal and no `--yes` | 2 |
| A config file that does not parse, both `opencode.json` and `opencode.jsonc`, an opencode v2 config with opencode 1 | 1 |
| A conflict without `--force`, also under `--dry-run` | 1 |
| A keychain error on macOS or Windows, a locked Secret Service | 1 |
| A write that failed (permission, file held open, file changed while ambit worked) | 1 |
| `ambit` runs from a temporary npx copy | 1 |
| The test call failed | 1 |
| `ambit key print`: no key stored | 1 |
| `--revoke`: the console did not revoke the key (network, `429`, `5xx`) | 1 |

## ambit check

Reads the cluster and prints one line per check: `✓` pass, `!` warning, `✗` failed, `?` unknown (the CLI could not look: a missing permission or an endpoint that does not answer; the line names the permission), `-` n/a (the check does not apply). It changes no object. Two checks are active and say so: the health probes open a port-forward to a pod, and `--deep` sends one test call per model.

Without flags it runs the checks that apply: the pre-install group when Ambit is not installed; else the health group, and the production group unless `console.url` is under `localtest.me`.

| Flag | Meaning |
|---|---|
| `--pre` | the pre-install group |
| `--local` | also the local group (Docker and ports, as `ambit local up` checks them); with `--name`, `--port`, `--https-port` |
| `-f`, `--values <file>` | values file for the pre-install checks R4 and R5 |
| `--production` / `--no-production` | run or skip the production group |
| `--deep` | one test call per Ready model (a few tokens each) and an audit-record check (the OpenTelemetry Collector's count of records sent to ClickHouse must grow within 60 s of a test call). Reads a gateway token from `AMBIT_TOKEN`. |
| `--fail-on-unknown` | exit 1 when a check could not look, for CI that must prove every check ran |

`--output json` prints a list of `{"id", "group", "status", "message", "next", "source"}`.

**In CI and scripts, run `ambit check --fail-on-unknown`.** Without it, a check that could not look is `?` and the exit code stays 0. If `KUBECONFIG` points to a file that does not exist, every check is `?` and the command exits 0. With `--fail-on-unknown` the same run exits 1. A `--kubeconfig` or `--context` that you pass on the command line and that cannot be used exits 2 in both cases.

| ID | Group | Check | Source |
|---|---|---|---|
| R1 | pre | Kubernetes 1.30 or later | live objects |
| R2 | pre | you can create CRDs, namespaces and ClusterRoles | live objects |
| R3 | pre | which of the releases `ambit-crds` and `ambit` are installed, and their versions | live objects |
| R4 | pre | a default StorageClass when the chart's own ClickHouse is on | values (`-f`), live objects |
| R5 | pre | the TLS Secret named in `gateway.tls.certificateSecret` exists | values (`-f`), live objects |
| R6 | pre | third-party CRDs installed by another tool are not older than this version needs | live objects |
| L1 | local | Docker is running | docker |
| L2 | local | the container runtime is supported (not rootless Docker, not Podman) | docker |
| L3 | local | memory for Docker: warning below 6 GB, failure below 4 GB | docker |
| L4 | local | the two ports are free | host |
| L5 | local | no kind cluster with the name, or one that `ambit local up` created | docker |
| H1 | health | both releases are deployed, at the same version | live objects |
| H2 | health | the third-party and Ambit CRDs are Established | live objects |
| H3 | health | every pod is Ready, else the reason | live objects |
| H4 | health | the gateway answers its readiness endpoint (active: port-forward) | live objects |
| H5 | health | Dex answers its discovery document (active: port-forward) | live objects |
| H6 | health | the chart's ClickHouse answers a ping when evidence is on (active: port-forward) | values, live objects |
| H7 | health | every Model, Server and AccessRule is Ready | live objects |
| H8 | health | with `--deep`: each Ready model answers a test call | live objects |
| P1 | production | `gateway.replicas` is 2 or more, with a PodDisruptionBudget and 2 Ready replicas | values, PDB, Deployment |
| P2 | production | `bootstrapAdmin.enabled` is false | values |
| P3 | production | HTTPS: `console.url` uses HTTPS, `gateway.tls` is set, the certificate covers the hosts and is valid for 14 more days | values, Secret (certificate only) |
| P4 | production | evidence is on and its pods are Ready; with `--deep`, the collector sends a test call's audit record to ClickHouse | values, pods; with `--deep`, collector metrics |
| P5 | production | limits are on and `limits.failureMode` is set by you | values you supplied |
| P6 | production | budgets use an external Valkey (warning only) | values |
| P7 | production | protection is on when any MCP Server exists | values, objects |
| P8 | production | alerts are on when the Prometheus Operator is installed | values, CRDs |
| P9 | production | one console replica, Ready | values, Deployment |
| P10 | production | every Model's credentials are in an existing Secret, or in the gateway's cloud identity | objects, Secret existence, values |
| P11 | production | `access.mode` is `enforce` (audit-only mode is a warning, with the days since `modeSince`); with `--deep` in audit mode, it fails when a test call's audit record does not reach the store. A line under it counts the audit-mode call records of the last 24 hours with a missing or failed mark (a warning above 0). While a mode switch or rules change is not active, another line (a warning) names the object agentgateway rejected, the gateway's reason and the next step | values, `ambit-access-mode`, collector metrics; the line reads ClickHouse |
| P12 | production | an AccessRule's `users` match verified emails only: a warning when `accessRules.usersRequireVerifiedEmail` is false | values |

### Permissions

`ambit check` needs read access (`get`, `list`) to the Ambit objects (Models, Servers, AccessRules), Deployments, Pods, PodDisruptionBudgets and StorageClasses in the namespace, to CustomResourceDefinitions, and `get` on Secrets in the namespace: Helm keeps the release values in Secrets, P3 reads the TLS certificate, and P10 checks that each credentials Secret exists (without reading its values). The active checks need `create` on `pods/portforward`: the health probes, and P4 and P11 with `--deep`, which read the collector's metrics on port 8888 (one test call serves both). The mark-missing line under P11 needs `create` on `pods/exec` in the ClickHouse pod: it runs one `SELECT` there with the pod's own credentials, so the CLI never reads the ClickHouse password. With an external ClickHouse, or without that permission, the line is `?`; the Audit page shows the same count. R2 uses SelfSubjectAccessReviews, which every signed-in user may create. Without a permission, the check is `?` and names what is missing, for example `cannot get secrets in ambit-system`.

## ambit validate

```sh
ambit validate <file or directory>...
```

Checks every Ambit object (`apiVersion: ambitproject.dev/...`) in the YAML files the way the cluster would on apply: defaults, unknown fields, the schema and its CEL rules. Other objects are skipped. Exit 1 if any object would be refused. In GitHub Actions it also prints `::error` annotations. For CI, the binary is also at `/ambit` in the image `ghcr.io/craftsman-apps/ambit-console`.

## ambit lint

```sh
ambit lint [--strict] <file or directory>...
```

Reads the YAML files (also a `kind: List`, as `kubectl get accessrules -o yaml` prints it) and prints one warning for each AccessRule setting that the cluster accepts but that gives wide access. It reads files only, no cluster. Other objects are skipped.

| ID | Warning |
|---|---|
| `all-signed-in` | `allSignedIn: true` matches every signed-in user. Name groups or users instead. |
| `all-tools` | A `servers` entry without `tools` allows every tool of the server, including tools the server adds later. List the tools. |

Each line is `! <file>: AccessRule <name>: <warning> [<id>]`, and the last line is `<n> files, <m> AccessRules, <k> warnings`. Exit 0 with or without warnings; with `--strict`, exit 1 when there is a warning. A file that is not valid YAML: exit 2. In GitHub Actions it also prints `::warning` annotations. A warning can be right for a test or a pilot: `ambit lint` does not change or refuse anything.

## ambit version

Prints the CLI version, the chart version it installs, and, when the cluster answers within 3 seconds, the installed versions of `ambit-crds` and `ambit`. `--output json` prints `{"cli", "charts", "chartRepo", "installed", "cluster"}`.

## Files the CLI writes

- A state file per local cluster: `~/Library/Application Support/ambit/local/<name>.json` (macOS), `~/.config/ambit/local/<name>.json` (Linux), `%AppData%\ambit\local\<name>.json` (Windows). `ambit local down` deletes it.
- `ambit connect` records what it wrote in `connect.json` in the `ambit` folder of your user config folder (`~/Library/Application Support/ambit` on macOS, `~/.config/ambit` on Linux, `%AppData%\ambit` on Windows), mode `0600`. The file never holds a key. `ambit connect claude-desktop-mcp` writes the header file `~/.ambit/mcp-headers.txt`, `ambit connect hermes --mcp-url` writes `~/.ambit/hermes-mcp-headers.txt`, and `ambit connect opencode` (opencode 2) and `ambit connect kilocode` write `~/.config/ambit/opencode-key` and `~/.config/ambit/kilo-key`. These files hold the key (mode `0600`). The fallback key file is in `keys/` in the same folder. Each tool's config file that `ambit connect` changed has a copy `<file>.ambit.bak`; `ambit disconnect` does not delete it.
- kind adds the context `kind-<name>` to your kubeconfig; `ambit local down` removes it.
