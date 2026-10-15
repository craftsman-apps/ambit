---
title: Give an agent tools and apps
description: Make an MCP server available, allow chosen tools to chosen groups, and see how work apps that need a personal sign-in are added.
---

# Give an agent tools and apps

**What you get:** agents reach tools only through the Ambit gateway. A person sees and can call only the tools that an access rule gives their group. Every call is recorded.

**Which server this guide uses, and why.** The steps you run use the `time` MCP server. It runs inside your cluster and needs no vendor account and no internet access at run time. It is the same server as in the quick start, so you can follow the guide on a laptop. Work apps such as GitHub or Slack need an account at the vendor. They are described in a separate section and are not part of the check.

## Before you start

You need the cluster from the [Quick start](../quick-start.md), the groups and people of [Different models for different teams](models-per-team.md) (Alice in `engineering`, Bob in `sales`), and a personal API key with the scope **MCP** or **LLM and MCP** for each of them. The key is made in the console, as described in [Connect Claude Code and Cursor](coding-tools.md). Save Alice's key in `ALICE_KEY` and Bob's key in `BOB_KEY`.

## 1. Add the server and allow one tool

A `Server` is an MCP server in Ambit's catalog. An `AccessRule` lists the servers a group may use and, for each, the tools. If you leave `tools` out, the group may use every tool of the server, including tools that the server adds later. Here, engineering may use one tool.

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
metadata: { name: engineering-time }
spec:
  groups: [engineering]
  servers:
    - name: time
      tools: [get_current_time]
EOF
kubectl -n ambit-system wait servers.ambitproject.dev/time --for=condition=Ready --timeout=300s
```

The image is pinned by its digest. A tag could later point to different code. A digest cannot.

If you did not delete the quick-start rule `everyone-time`, delete it now. A rule only adds access, so that rule would give every signed-in person the tool.

```sh
kubectl -n ambit-system delete accessrule everyone-time --ignore-not-found
```

## 2. Connect a client

A client needs the MCP address and a key. With Claude Code:

```sh
claude mcp add --transport http ambit http://mcp.localtest.me:8080/mcp --header "Authorization: Bearer $ALICE_KEY"
```

The gateway shows each tool with the name of its server in front. This tool is `time_get_current_time`.

## Check it works

The gateway needs a few seconds to load a new rule. This function opens an MCP session with a key and sends one request.

```sh
mcp() { # mcp KEY JSON-RPC-REQUEST
  MCP=http://mcp.localtest.me:8080/mcp
  H='Accept: application/json, text/event-stream'
  SID=$(curl -s -D - -o /dev/null $MCP -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -H "$H" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"guide","version":"1"}}}' \
    | awk 'tolower($1)=="mcp-session-id:"{print $2}' | tr -d '\r')
  curl -s -o /dev/null $MCP -H "Authorization: Bearer $1" -H "Mcp-Session-Id: $SID" -H 'Content-Type: application/json' -H "$H" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  curl -s -w '\nHTTP %{http_code}\n' $MCP -H "Authorization: Bearer $1" -H "Mcp-Session-Id: $SID" -H 'Content-Type: application/json' -H "$H" -d "$2"
}
```

Alice lists her tools. She sees only the allowed tool:

```sh
mcp "$ALICE_KEY" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

The answer has one tool, `time_get_current_time`, and ends with `HTTP 200`.

Alice calls the allowed tool:

```sh
mcp "$ALICE_KEY" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"time_get_current_time","arguments":{"timezone":"Europe/London"}}}'
```

The answer holds the time in London and ends with `HTTP 200`.

Alice calls another tool of the same server. The rule does not list it:

```sh
mcp "$ALICE_KEY" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"time_convert_time","arguments":{"source_timezone":"Europe/London","time":"10:00","target_timezone":"Asia/Seoul"}}}'
```

```text
{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"Unknown tool: time_convert_time"}}
HTTP 400
```

Bob is in `sales`. No rule gives that group the server, so his list is empty:

```sh
mcp "$BOB_KEY" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

```text
data: {"jsonrpc":"2.0","id":2,"result":{"tools":[]}}
```

In the console, open **Audit** and search for `alice@acme.test`. Each call shows the server and the tool. The refused call to `convert_time` is there too.

## Work apps that need a personal sign-in

This part is not part of the check above. It needs a vendor account, so it was not run for this guide. It follows [App presets](../presets.md).

Some vendors run their own MCP server and ask each person to sign in at the vendor. Ambit keeps one token for each person and app, and adds it to that person's calls. A shared service account is not used. A preset holds the vendor's addresses, scopes and setup steps. It does not hold secrets. Your company registers its own OAuth app at the vendor and gives Ambit the app's client ID and secret.

An admin adds an app in the console:

1. Open **Apps** and choose **Add an app**, then choose a preset. Each card can show `tested <date>`, `preview` or `needs: ...`.
2. On the **Set up** screen, follow the numbered steps to register an OAuth app at the vendor. The steps contain the callback address of your Ambit install. Then enter the client ID and the client secret.
3. Choose **Add**. Ambit creates the client secret, a `Connection` and a `Server` together.
4. Connect your own account. Then choose **Use my account to read tools** on the **Servers** page, tick the tools to allow, choose groups, and create the access rule. At first no tool is allowed.

Each person who uses the app then connects their own account. A call without a connection gets a link to connect. The access rule works as in this guide: groups and tools.

## Related

- [Ask for an MCP app and approve it](request-an-app.md): how a person asks for an MCP app and how an approver decides.
- [App presets](../presets.md): the screens, limits and the tool reader.
- [Offboarding](../offboarding.md): which vendors truly revoke a person's access.
- [Concepts](../concepts.md): how Ambit checks an MCP request.
- [Someone leaves](someone-leaves.md): what happens to a person's connected apps.
