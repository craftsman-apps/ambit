# The public front door

> **Experimental in 0.6.x.** The front door, its Helm values (`frontDoor.*`), its routes and its audit field may change in 0.7.0, with an upgrade note. They never change silently inside 0.6.x.

Hosted agents such as Grok Bot, Copilot Studio, AWS AgentCore and Cloudflare Agents run on the internet, not on your
company network. They cannot reach the Ambit gateway of your company. The front door is a **second, public gateway**
that lets such an agent reach the MCP servers an admin chose, with a personal API key, and nothing else.

Plain picture: the main gateway is the front door of the building, for your staff. The public gateway is a separate
side door that leads only to the rooms marked "visitors allowed". It has its own address, its own guards and its own
list of visitors. It does not open into the rest of the building.

It is **off by default**. In this release it accepts **API keys only** (personal keys and service account keys with the
MCP scope). Sign-in with OAuth is a separate, experimental feature ([OAuth sign-in](oauth.md)); without it, a request without a valid key gets 403.

## What it is, and what it is not

| | |
|---|---|
| Gateway | `ambit-public`: its own pods, its own Service and load balancer, in the Ambit namespace. The main gateway `ambit` is not changed. |
| Protocol | HTTPS only. There is no HTTP listener: a plain HTTP request gets no answer from MCP. You bring the certificate (`frontDoor.tls.certificateSecret`). |
| Host name | One name, `frontDoor.host`. It must differ from `console.host`, `llm.host` and `mcp.host`. A request for another name gets 404 or no TLS answer. |
| Routes | One: the exact path `/mcp` on `frontDoor.host`. Every other path is 404. The Kubernetes API refuses (a `ValidatingAdmissionPolicy`, `ambit-public-routes`) a route that names `ambit-public` and is not `ambit-mcp-public`, and a change of that route (another host, another path, another backend, another Gateway). |
| Not reachable | The console (`/`, `/api/*`), Dex (`/dex/*`), the LLM host, SCIM and the app sign-in pages. They are not routes of `ambit-public`, so they answer 404 there. |
| Credentials | An Ambit API key with the MCP scope, in `x-api-key` or `Authorization: Bearer ambit_...`. A company token (JWT) is refused on this gateway. A key of an [agent](agents.md) that names source ranges works only from those addresses (as seen by the gateway: see [the client address](#load-balancers-and-the-client-address)). |
| Servers | Only the servers an admin marks for the front door and acknowledges (`Server` `spec.frontDoor`, see [Choose the servers](#choose-the-servers)). |

Everything an agent does through the front door is checked and recorded like any other call: the person's AccessRules,
the guard, the audit record, offboarding. The audit record also says `ambit.ingress = public`. The value is a constant
of the public gateway's access-log policy, so a request cannot change it.

## Requirements

The chart refuses `frontDoor.enabled: true` unless each of these is set, and the message names the value:

| Value | Why |
|---|---|
| `frontDoor.host` | The one public host name. |
| `frontDoor.tls.certificateSecret` | A `kubernetes.io/tls` Secret in the Ambit namespace; its certificate covers `frontDoor.host`. The front door serves HTTPS only. |
| `mcp.enabled: true` | The front door serves MCP only. |
| `apiKeys.enabled: true` | Keys are the only credential. |
| `limits.enabled: true` | The shared rate limits are counted by the ratelimit service and Valkey. |
| `evidence.enabled: true` | Every call through the public gateway must be recorded. |
| `protection.enabled: true` | The public backend has the guard (tool pinning, argument limits, masking), which fails closed. |

## Install

1. Create the certificate Secret for the public host (for example with cert-manager):

   ```
   kubectl -n ambit-system create secret tls ambit-public-tls --cert=fullchain.pem --key=privkey.pem
   ```

2. Add the values and upgrade:

   ```yaml
   frontDoor:
     enabled: true
     host: mcp-public.example.com
     tls:
       certificateSecret: ambit-public-tls
     service:
       type: LoadBalancer
       # your cloud's annotations, for example for a network load balancer
       annotations: {}
       # only these ranges reach the load balancer (the cloud firewall); [] allows every address
       loadBalancerSourceRanges: []
   ```

3. Point the DNS name `mcp-public.example.com` at the load balancer of the Service `ambit-public`. The gateway listens on `frontDoor.port` (default 8443), so callers use `https://mcp-public.example.com:8443/mcp` unless your load balancer or tunnel also serves port 443 and forwards it to the Service:

   ```
   kubectl -n ambit-system get service ambit-public
   ```

4. Check it from outside (a key with the MCP scope, from the console page **Keys**):

   ```
   curl -si https://mcp-public.example.com:8443/                       # 404
   curl -si https://mcp-public.example.com:8443/mcp -X POST -d '{}'     # 403: no key
   ```

The main gateway, the console and Dex are not changed. To turn the front door off, set `frontDoor.enabled: false`: the
Gateway, the load balancer and the policies are deleted. The chart refuses `apiKeys.enabled: false` while the front door is
on, so the emergency stop of all keys is `frontDoor.enabled: false` and `apiKeys.enabled: false` together.

### Before you turn it on

The admission policy checks routes when they are created or changed. A route that names `ambit-public` **before** the
Gateway exists is not checked, so look first:

```
kubectl -n ambit-system get httproutes -o custom-columns=NAME:.metadata.name,GATEWAYS:.spec.parentRefs[*].name
```

No route except `ambit-mcp-public` may list `ambit-public`.

## Load balancers and the client address

The gateway has to know the real address of the caller. The rate limits (below, on the MCP host **and** on the sign-in host) and the
audit record use it. Ambit **never trusts `X-Forwarded-For`**, `X-Real-IP`, `Forwarded` or any other header for this: a caller can write
any value into them. The Gateway takes the address from the connection, or from the PROXY protocol header that your load balancer sends,
and for the sign-in pages it writes that address into its own header toward `ambit-auth`, replacing whatever the caller sent.

**Supported modes.** Set `frontDoor.clientAddress` to match how traffic reaches the gateway pods:

| # | How traffic reaches the gateway | `frontDoor.clientAddress` | What else must be true |
|---|---|---|---|
| 1 | A layer-4 (TCP) load balancer that keeps the caller's source address, or a service of your own that does (MetalLB, `hostPort`, a cloud network load balancer with client address preservation) | `direct` (default) | `frontDoor.service.externalTrafficPolicy: Local` (the default), so that a node does not replace the caller's address with its own. |
| 2 | A layer-4 load balancer that sends the **PROXY protocol** (version 1 or 2) before the TLS handshake (for example AWS NLB with proxy protocol v2 turned on, HAProxy, nginx `proxy_protocol`) | `proxyProtocol` | Turn the PROXY protocol on at the load balancer **first**, then set the value: the gateway then refuses every connection without the header. **Only the load balancer may reach the gateway pods**: the header is not signed. The chart refuses `proxyProtocol` with `frontDoor.service.type: NodePort`; also close every other way in (cloud firewall, `loadBalancerSourceRanges`, a network policy of your own). |
| 3 | `kubectl port-forward`, an SSH tunnel or a local test | `direct` | Every caller counts as the address of the tunnel. For tests only. |

**Not supported:** a layer-7 load balancer, an HTTP reverse proxy, an ingress controller, or a CDN that ends TLS and sends a new request
with `X-Forwarded-For` (AWS ALB, Cloudflare proxy, and similar). The gateway would see the proxy's address for every caller, so all callers
would **share one set of limits**: the first flood would lock everyone out. Use a layer-4 balancer. If you cannot, put the limits at that
proxy, where the real address is known, and raise the limits of this chart so that they do not count every caller as one.

**What happens when the mode is wrong.** With `direct` behind a balancer that hides the address, or behind a node that replaces it (`externalTrafficPolicy: Cluster`),
all callers share one address: the limits count them together. With `proxyProtocol` and a balancer that does not send the header, every
connection is refused. Check once after install.

**Check the address once.** Call the front door from a machine whose public address you know, then read the record in Audit (`client.address`) or in
ClickHouse (`LogAttributes['client.address']`). For the sign-in host, open a sign-in from that machine, and read `ambit-auth`'s log:
`kubectl -n ambit-system logs deploy/ambit-auth | grep "no client address"` must print nothing (the line appears when a request reaches it without
the Gateway's header: then the limits count every caller together).

**What is tested.** Ambit does not test cloud load balancers in CI. On kind, in CI: a connection with no PROXY header (`direct`); a
connection that sends a made-up client address in a PROXY header (`proxyProtocol`), on the MCP host (`test/stepfd.sh`, check 8) and on the
sign-in host (`test/stepoauth.sh`, check 18: five buckets, several made-up addresses, two Gateway pods); and callers that write
`X-Forwarded-For`, `X-Real-IP`, `Forwarded` and `X-Ambit-Client-Address` (check 18: no effect on the count, in both directions).

## Rate limits

Limits protect the gateway and the keys. They are on by default. Values are in `frontDoor.rateLimit`.

| Limit | Default | Counted | Counts |
|---|---|---|---|
| `perSourcePerMinute` (bucket **mcp**) | 120 | by the ratelimit service and Valkey, **shared by all gateway pods**, **before** the route is matched and before the key is checked | **every** request from one source address that is not in a bucket below: a valid key, a wrong key, an unknown path, a call with a token |
| `perCredentialPerMinute` | 600 | by the same service, shared, after authentication | requests with one valid key (by key ID), or with one token (by its agent: one connection of one person to one platform), so that tokens do not share one counter |
| `perPodPerSecond` | 200 | in each gateway pod, after authentication | every request that reaches the limit check. A second line: it still works when the ratelimit service is down. |

With `oauth.enabled` there are four more **buckets of the source limit**, on the same address, counted the same way (before the route and
the credential, refused requests included, shared by all gateway pods). A flood of one kind never uses up another kind:

| Bucket | Default | Which requests |
|---|---|---|
| **discovery** (`oauth.rateLimit.discoveryPerMinute`) | 600 a minute | `GET /.well-known/*` on both hosts (the metadata documents, the key list), and `POST /mcp` **without any credential** (no `x-api-key`, no `Authorization`): the 401 that starts a sign-in |
| **sign-in** (`oauth.rateLimit.signInPerMinute`) | 300 a minute | `/oauth2/auth`, `/login`, `/callback`, `/consent`, `/error` of the sign-in host |
| **token** (`oauth.rateLimit.tokenPerMinute`) | 600 a minute | `/oauth2/token`, `/oauth2/revoke` |
| **registration** (`oauth.registration.dynamic.perSourcePerHour`) | 10 an hour | `/register` |

The bucket is chosen by the path and by whether a credential header is present, nothing else. A path that no route serves is in **mcp**.
`ambit-auth` counts the first two buckets and the registration again with the same numbers, as a second line when the ratelimit service is down.

A request over a limit gets **429** (or, for an MCP request, `200` with a JSON-RPC error: agentgateway reports some errors of
an MCP request that way). The first two limits are per minute (a fixed window that starts at each full minute).

- **How the source limit works before authentication.** agentgateway v1.7.0 runs its rate limits after it matches the route
  and checks the key, and it allows no rate limit earlier. So a flood of wrong keys (403) or unknown paths (404) would
  count against nothing ([issue #390](https://github.com/craftsman-apps/ambit/issues/390)). The chart therefore adds the
  policy `ambit-public-source-limit`: before the route is matched, the Gateway asks the ratelimit service (its `/json`
  endpoint, over the cluster network) to count one request for the source address. Over the limit, the service answers 429 and the
  Gateway sends that answer to the caller. The Gateway sends only the address, never the key.
- **A flood from one address also blocks that address's good requests.** The counter is per address, so a caller that floods
  with wrong keys uses up the limit of every key that comes from the same address, for the rest of the minute. That is the
  purpose (the flood stops being cheap), and the price is that callers behind one shared address (a NAT, a hosted-agent
  platform with few addresses) share one budget. A valid key from another address is not affected.
- **What this does not cover.** The check runs when the request headers have arrived, before the body is read and before the Host header is compared with the route (a request with the right TLS name and another Host header is counted). Connections that never send a
  request, TLS handshakes, requests for another host name (the listener is bound to one host name), and many source addresses
  (an IPv6 caller can use a whole /64 range; the counter is per address) are not limited by Ambit. Limit them at the edge:
  a cloud firewall or WAF, and the load balancer's own connection limits. For one more cap in the gateway itself, you can add an
  `AgentgatewayPolicy` of your own with `frontend.tcp.maxConnections` (connections per Gateway port) or
  `frontend.http.maxConcurrentRequests` (requests in flight). Both count all callers together, so a flood can use them up and block good
  callers too: they protect the pods, not the callers. Ambit does not set them.
- **A failed check on the way in.** `frontDoor.rateLimit.failureMode` also applies to the source check: with `FailOpen`
  (default) requests pass when the ratelimit service cannot be reached, and the per-pod limit still applies; with `FailClosed`
  they are refused with 403. If the ratelimit service answers but **Valkey is down**, the service reports its error as HTTP
  400, and the front door refuses every request with 400 until Valkey is back (and for a few seconds after, while the service reconnects), whatever `failureMode` says. If Valkey is slow
  instead of failed, the check waits 2 seconds and then follows `failureMode`.
- **A flood is still read by the limiter.** Every request, also one that is already over its limit, costs one call to the
  ratelimit service and one operation in Valkey. The limit makes a flood cheap for the gateway pods and for the MCP servers,
  not free for the ratelimit service. The edge limits above are the place to stop the volume itself.
- **The ratelimit port.** The check uses port 8080 of the ratelimit service (`/json`). That endpoint counts a hit for any
  domain and descriptor and has no password, so the network policy `ambit-ratelimit` admits **only the public gateway pods**
  to it (and the gateway pods to port 8081, as before). No other pod in the namespace, such as an MCP server, can raise or
  fill a counter through `/json`. The kubelet's readiness probe comes from the node, which a network policy does not limit. The rule holds on
  a cluster network that enforces network policies, and only while nobody who can create pods in the namespace is untrusted
  (the policy admits pods by label). A pod of the private gateway can still use port 8081, which the budgets need, and another network policy in the
  namespace that admits more pods widens the access.
- **Many people behind one company address.** Callers behind one NAT address share the buckets of that address. One sign-in costs 7
  requests in the sign-in bucket (`/oauth2/auth` three times, `/login`, `/callback`, `/consent` twice), and one request in the token bucket.
  Measured on kind with the default limits: one shared address made @@NAT@@ whole sign-ins in one minute before the sign-in bucket answered 429, and
  another address was not affected (docs/oauth.md, "Limits"). That is the number to compare with your largest group that connects at the same time
  (an announcement, a training); raise `oauth.rateLimit.signInPerMinute` for it. 120 MCP calls a minute (bucket **mcp**) are a starting value for a small number of hosted agents, not for
  a company office or a platform that serves many people from a few addresses: raise `perSourcePerMinute`, or put your own limit at the edge, where you know the traffic.
- **When the ratelimit service is down.** `frontDoor.rateLimit.failureMode: FailOpen` (default) lets requests through
  uncounted, and the per-pod limit still applies. `FailClosed` refuses requests until the service is back. When you first
  turn the front door on, an old ratelimit pod can still lack the new limits for a moment (it answers "allowed" even with
  `FailClosed`); wait until the pods are ready before you rely on it.

Edge limits (a cloud firewall or WAF in front of the load balancer) are the operator's: Ambit does not manage them.

## Choose the servers

Nothing is on the front door until an admin puts it there. In the `Server` object:

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata:
  name: wiki
spec:
  url: https://wiki.example.com/mcp
  frontDoor:
    enabled: true
    acceptUnmaskedServerMessages: true
```

`enabled` puts the server on the front door. `acceptUnmaskedServerMessages: true` is the admin's written
acknowledgment of the limit in [What the guard does not mask](#what-the-guard-does-not-mask): there is no safe default for
it, so it is a separate field that you have to set on purpose. **With `enabled` and without the acknowledgment the server
is not served at all, on either gateway**: `Ready` is `False`, reason `FrontDoorNotAcknowledged`, and the message says what
to set. A half-written decision must not leave a server open anywhere.

A server with a `connection` (a person's own app account) cannot be on the front door yet: `Ready` is `False`, reason
`FrontDoorConnection` (a server whose connection is bound to another address says `ConnectionBound` first). A hosted agent would otherwise receive the person's own app token. The front door will carry it only
when the person has agreed for that agent and that app, in a later change of this release series.

The `frontDoor` setting of a server request is never used: an approved request creates a server that is not on the front
door. Only an admin, with `kubectl`, in GitOps, or in the console later, puts a running server there.

**Taking a server off the front door** (remove `frontDoor`, or remove the acknowledgment) removes it from the public backend at
once and the public access policy refuses its tools from then on. A public session that is already open can still send the
server the messages that open a session (the server is contacted when the session was made), until the session ends; no tool call
passes. The server's proxy pod loses the label that admits the public gateway.

### How the separation works

The controller writes **a second backend**, `ambit-mcp-public`, that contains only the servers on the front door, and its own
access policy `ambit-mcp-public-access`. The public route names that backend. The private route names `ambit-mcp`
(every server). No header says which door a request came through, so no header can be forged.

| What | On the public route |
|---|---|
| The server's tools in `tools/list` | only servers on the front door |
| `tools/call`, `resources/read`, `prompts/get` or `completion/complete` for a private server (with or without its name prefix) | refused: the server is not a target |
| Access rules | the same rules as on the private route, restricted to the servers on the front door. Audit-only mode relaxes the private route only, never the public one. |
| The guard (tool pinning, argument limits, masking) | the same guard, `FailClosed` |
| Network | of the MCP server pods, the public gateway pods reach only the pods of front-door servers (network policy `ambit-mcp-servers-front-door`, pod label `ambitproject.dev/front-door`) |
| The console's token broker (port 8082) | closed to the public gateway pods. Other ports of the console and Dex keep the network policies they had; they are not routes of `ambit-public`, but they are not closed to a pod in the cluster. Add a network policy of your own if you need that. |
| A header such as `x-ambit-ingress`, `X-Ambit-User` or `x-ambit-agent` sent by a client | no effect; the gateway removes `X-Ambit-User`, and nothing reads the others |
| An MCP session id from the private gateway | unknown on the public gateway |

The e2e test `test/stepfd.sh` runs every MCP method against a front-door server and a private server that the same person
may use, on both routes, and checks the table above.

### What the guard does not mask

`ambit-guard` masks personal data in the **result** of a tool call. The gateway does not send it other messages, so a
server's **error messages and notifications** (log, progress, sampling and elicitation requests) reach the hosted agent
as the server wrote them (issue #333). Hosted agents are outside your network, so this matters more here. Put a server on
the front door only if it never writes personal data into an error or a notification; that is what
`acceptUnmaskedServerMessages` says. Ambit cannot remove the limit until agentgateway can pass these messages to the guard:
see [agentgateway issue 4245](https://github.com/agentgateway/agentgateway/issues/4245) and
[pull request 4345](https://github.com/agentgateway/agentgateway/pull/4345). The audit log never stores the text of an
error. A tool result that is not a JSON object, and a tool list that is not an object with a list of tools, are withheld
(the guard fails closed); see [Masking](concepts.md#masking).

## Connect a platform

Each guide gives the request shape of one platform, tested against a test cluster (**request shape tested; not yet tested with a live
account**):

| Platform | Credential | Guide |
|---|---|---|
| Grok Bot | a person's own agent key | [Grok Bot](guides/agents/grok-bot.md) |
| Copilot Studio | each user's own agent key, in a header | [Copilot Studio](guides/agents/copilot-studio.md) |
| AWS AgentCore Gateway | an itself-mode agent key | [AWS AgentCore](guides/agents/agentcore.md) |
| Cloudflare Agents SDK | an itself-mode or person-mode agent key | [Cloudflare Agents](guides/agents/cloudflare.md) |
| Claude.ai, one key for a whole team | an itself-mode agent key. **Lower assurance**: Audit shows a shared identity, never a person | [Claude.ai shared team setup](guides/agents/claude-ai-shared-team.md) |

## Public API (experimental)

What this feature adds, all of it experimental in 0.6.x:

- Helm values `frontDoor.*` (all off or empty by default).
- The field `Server.spec.frontDoor` (`enabled`, `acceptUnmaskedServerMessages`) and the `Ready` reasons
  `FrontDoorNotAcknowledged` and `FrontDoorConnection`.
- The objects `Gateway ambit-public`, `HTTPRoute ambit-mcp-public`, `AgentgatewayParameters ambit-public`,
  `ConfigMap ambit-ratelimit-front-door`, `ValidatingAdmissionPolicy ambit-public-routes`, the generated
  `AgentgatewayBackend ambit-mcp-public` with its policy `ambit-mcp-public-access`, the network policy
  `ambit-mcp-servers-front-door` and the pod label `ambitproject.dev/front-door`, and the policies `ambit-mcp-public-auth`,
  `ambit-public-limits` and `ambit-public-access-log` (written by the controller).
- The public route: exact path `/mcp` on `frontDoor.host`, with a key in `x-api-key` or `Authorization: Bearer ambit_...`.
- The audit attribute `ambit.ingress` (`public` or `private`; written only when the front door is on).

## Measured

Measured on kind (see `test/stepfd.sh`).

| What | Measured |
|---|---|
| Private paths and hosts on the public port | see the table in the pull request |
