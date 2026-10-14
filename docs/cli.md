---
title: CLI reference
description: Every command, flag and exit code of the ambit CLI, the checks of ambit check, and the permissions they need.
---

# CLI reference

`ambit` starts a local Ambit on kind, checks an install, and validates Ambit objects in files. To install it, see [Quick start](quick-start.md). This page describes release 0.2.0.

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

Every failure prints a line `✗ <what> — <why>` and, on the next line, `next:` with the command to run or a page to read.

## Secrets in the output

The CLI never prints the OpenRouter key, the admin password or tokens: one output layer replaces them with `[redacted]` in text, JSON and `--verbose` output, also inside error messages from the libraries it uses. Keys are read from a hidden prompt, an environment variable or a file, never from a flag value, so they do not end up in your shell history. The local admin password is printed by `ambit local up` only on a terminal (`--no-password` hides it there too), and by `ambit local password`, whose purpose is to print it.

## ambit local up

Creates a local Ambit on a kind cluster, or repairs one that `ambit local up` created. Press Ctrl+C once to stop (the command says how to remove a cluster it created); press it a second time to end the process at once. Steps: preflight (the `L` checks below), cluster, the third-party CRDs, the charts `ambit-crds` and `ambit` (with progress every 10 seconds), a model, the access rule `everyone-local`, a test call, and a summary. See [What ambit local up does](local-up.md) for each object.

| Flag | Default | Meaning |
|---|---|---|
| `--name <name>` | `ambit` | name of the kind cluster; the kubectl context is `kind-<name>`. `ambit local down` and `ambit local password` take it too. |
| `--port <port>` | `8080` | port on `127.0.0.1` for the console and the gateway |
| `--https-port <port>` | `8443` | port on `127.0.0.1` for HTTPS |
| `--openrouter-key-file <file>` | | file with an OpenRouter API key (one line). `OPENROUTER_API_KEY` works too. |
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

`local up` repair, `local down` and `local password` act only on a cluster with both marks of `ambit local up`, a state file and a node label; see [A cluster that ambit did not create](local-up.md#a-cluster-that-ambit-did-not-create).

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

### Permissions

`ambit check` needs read access (`get`, `list`) to the Ambit objects (Models, Servers, AccessRules), Deployments, Pods, PodDisruptionBudgets and StorageClasses in the namespace, to CustomResourceDefinitions, and `get` on Secrets in the namespace: Helm keeps the release values in Secrets, P3 reads the TLS certificate, and P10 checks that each credentials Secret exists (without reading its values). The active checks need `create` on `pods/portforward`: the health probes, and P4 with `--deep`, which reads the collector's metrics on port 8888. R2 uses SelfSubjectAccessReviews, which every signed-in user may create. Without a permission, the check is `?` and names what is missing, for example `cannot get secrets in ambit-system`.

## ambit validate

```sh
ambit validate <file or directory>...
```

Checks every Ambit object (`apiVersion: ambitproject.dev/...`) in the YAML files the way the cluster would on apply: defaults, unknown fields, the schema and its CEL rules. Other objects are skipped. Exit 1 if any object would be refused. In GitHub Actions it also prints `::error` annotations. For CI, the binary is also at `/ambit` in the image `ghcr.io/craftsman-apps/ambit-console`.

## ambit version

Prints the CLI version, the chart version it installs, and, when the cluster answers within 3 seconds, the installed versions of `ambit-crds` and `ambit`. `--output json` prints `{"cli", "charts", "chartRepo", "installed", "cluster"}`.

## Files the CLI writes

- A state file per local cluster: `~/Library/Application Support/ambit/local/<name>.json` (macOS), `~/.config/ambit/local/<name>.json` (Linux), `%AppData%\ambit\local\<name>.json` (Windows). `ambit local down` deletes it.
- kind adds the context `kind-<name>` to your kubeconfig; `ambit local down` removes it.
