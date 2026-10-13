---
title: Quick start
description: Install Ambit on a laptop with kind, add a free model through OpenRouter, allow it with an access rule, and make a first call.
---

# Quick start

In this guide you install Ambit release v0.1.0 on a laptop, add a free model, allow people to use it, and make a first call through the gateway. The install takes from 2 to 20 minutes, depending on your network, because Ambit pulls about 1 GB of images.

This install is for testing only: plain HTTP, one gateway replica, and sign-in tokens from Ambit's own Dex. To install on a real cluster, see [Install on your cluster](install.md).

## What you need

- Docker, [kind](https://kind.sigs.k8s.io), `kubectl` and Helm 3. Kubernetes 1.30 or later is required; kind's default image meets this.
- Free ports 8080 and 8443 on your laptop.
- A free [OpenRouter](https://openrouter.ai) account and an API key. Free models need no credit.
- About 6 GB of memory for Docker.

The addresses in this guide use `localtest.me`. Every name under `localtest.me` resolves to `127.0.0.1`, so you do not change DNS.

## 1. Create the cluster

Download the three files of the release. The cluster file maps port 8080 on your laptop to the Ambit gateway.

```sh
R=https://github.com/craftsman-apps/ambit/releases/download/v0.1.0
curl -fsSLO $R/kind-cluster.yaml -O $R/quickstart.yaml -O $R/crds.yaml
kind create cluster --config kind-cluster.yaml
```

The cluster is named `ambit`. If you already have a kind cluster of that name, delete it first or change `name:` in `kind-cluster.yaml`.

## 2. Install Ambit

Install the CRDs of Ambit's dependencies, then the two charts.

```sh
kubectl apply --server-side -f crds.yaml
helm install ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds --version 0.1.0 -n ambit-system --create-namespace --wait
helm install ambit oci://ghcr.io/craftsman-apps/charts/ambit --version 0.1.0 -n ambit-system -f quickstart.yaml --wait --timeout 10m
```

The last command waits until every pod is ready. The first time, it pulls about 1 GB of images, so it can take several minutes. If your network or computer is slow, the command can stop after 10 minutes with `INSTALLATION FAILED ... context deadline exceeded`. The pods keep starting. Run `kubectl -n ambit-system get pods` and wait until every pod shows `1/1`; then go on. Do not install again.

## 3. Sign in to the console

Print the password of the bootstrap admin:

```sh
kubectl -n ambit-system get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d; echo
```

Open <http://console.localtest.me:8080> and sign in as `admin@ambit.local` with that password. The console is empty: no models, no servers, and no rules. Ambit denies everything until a rule allows it.

## 4. Add a free model

Ambit keeps provider keys in a Kubernetes Secret. The name of the Secret starts with `ambit-llm-`. Create it from your OpenRouter key. The first command reads the key without showing it on the screen:

```sh
read -rs OPENROUTER_KEY
kubectl -n ambit-system create secret generic ambit-llm-openrouter --from-literal=Authorization="$OPENROUTER_KEY"
unset OPENROUTER_KEY
```

Then create the `Model`. `spec.model` is the model's name at OpenRouter. This guide uses `nvidia/nemotron-3-super-120b-a12b:free`. OpenRouter changes its free models often. If this one is gone, take another model whose name ends in `:free` from the [OpenRouter model list](https://openrouter.ai/models?q=free).

```sh
kubectl -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: free-model }
spec:
  displayName: Free model
  provider: Openrouter
  model: nvidia/nemotron-3-super-120b-a12b:free
  credentialsRef: { name: ambit-llm-openrouter }
EOF
kubectl -n ambit-system wait models.ambitproject.dev/free-model --for=condition=Ready --timeout=120s
```


## 5. Get a token and make a call that is refused

Ambit's own Dex issues tokens in this setup. Ask it for a token for the bootstrap admin:

```sh
PW=$(kubectl -n ambit-system get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
TOKEN=$(curl -s http://console.localtest.me:8080/dex/token -d grant_type=password -d client_id=ambit-cli \
  -d scope="openid email profile" --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" \
  | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
```

Call the model:

```sh
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

The answer is `HTTP 403` with the message `Model authorization denied`. The token is valid, but no `AccessRule` allows this person to use the model. Right after you create a `Model`, the answer can be `HTTP 404` for a few seconds, until the gateway has its configuration. Repeat the command.

Clients send the provider's model name (`spec.model`), not the name of the `Model` object.

## 6. Allow the model

An `AccessRule` says who may use what. This rule allows every signed-in person to use the model:

```sh
kubectl -n ambit-system apply -f - <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: everyone-free-model }
spec:
  allSignedIn: true
  models: [free-model]
EOF
```

In a real install you list identity-provider groups instead of `allSignedIn`: `groups: [engineering]`.

## 7. Make the first call

Run the same command again. The gateway needs a few seconds to load the rule, so repeat it if you still see 403.

```sh
curl -s -w '\nHTTP %{http_code}\n' http://llm.localtest.me:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

You get `HTTP 200` and an answer from the model. If you get `HTTP 429`, OpenRouter's free quota for that model is used up. Wait a minute or change `spec.model` to another free model.

## 8. See the call in the console

Open the console at <http://console.localtest.me:8080>. Open **Audit**, search for `admin@ambit.local`, and you see your call with the model name and the numbers of input and output tokens. Ambit records every call that passes the gateway. A call that is refused before Ambit knows who sent it, such as the first call in step 5, is recorded without an email address, so a search by email does not show it.

## 9. Optional: an MCP server that needs no account

A `Server` makes an MCP server available. This one is a small server that tells the time. ToolHive runs it inside your cluster, and it needs no account.

```sh
kubectl -n ambit-system apply -f - <<'EOF'
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
kubectl -n ambit-system wait servers.ambitproject.dev/time --for=condition=Ready --timeout=300s
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

## 10. Clean up

```sh
kind delete cluster --name ambit
```

## Next

- [Concepts](concepts.md): what each object does, and how Ambit checks a request.
- [Install on your cluster](install.md): company sign-in, HTTPS, your own ClickHouse and Valkey.
- [Models and providers](providers.md) and [Personal API keys](api-keys.md): other providers, and keys for Claude Code and Cursor.

The quick start installs Ambit without budgets. To use a `Budget` (daily request and spending limits), install with
`limits.enabled: true`. [Different models for different teams](guides/models-per-team.md) shows the upgrade command.
