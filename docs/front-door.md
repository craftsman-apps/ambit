# The public front door

> **Experimental in 0.6.x.** The front door, its Helm values (`frontDoor.*`), its routes and its audit field may change in 0.7.0, with an upgrade note. They never change silently inside 0.6.x.

Hosted agents such as Grok Bot, Copilot Studio, AWS AgentCore and Cloudflare Agents run on the internet, not on your
company network. They cannot reach the Ambit gateway of your company. The front door is a **second, public gateway**
that lets such an agent reach the MCP servers an admin chose, with a personal API key, and nothing else.

Plain picture: the main gateway is the front door of the building, for your staff. The public gateway is a separate
side door that leads only to the rooms marked "visitors allowed". It has its own address, its own guards and its own
list of visitors. It does not open into the rest of the building.

It is **off by default**. In this release it accepts **API keys only** (personal keys and service account keys with the
MCP scope). Sign-in with OAuth is a later step; until then, a request without a valid key gets 403.

## What it is, and what it is not

| | |
|---|---|
| Gateway | `ambit-public`: its own pods, its own Service and load balancer, in the Ambit namespace. The main gateway `ambit` is not changed. |
| Protocol | HTTPS only. There is no HTTP listener: a plain HTTP request gets no answer from MCP. You bring the certificate (`frontDoor.tls.certificateSecret`). |
| Host name | One name, `frontDoor.host`. It must differ from `console.host`, `llm.host` and `mcp.host`. A request for another name gets 404 or no TLS answer. |
| Routes | One: the exact path `/mcp` on `frontDoor.host`. Every other path is 404. The Kubernetes API refuses (a `ValidatingAdmissionPolicy`, `ambit-public-routes`) a route that names `ambit-public` and is not `ambit-mcp-public`, and a change of that route (another host, another path, another backend, another Gateway). |
| Not reachable | The console (`/`, `/api/*`), Dex (`/dex/*`), the LLM host, SCIM and the app sign-in pages. They are not routes of `ambit-public`, so they answer 404 there. |
| Credentials | An Ambit API key with the MCP scope, in `x-api-key` or `Authorization: Bearer ambit_...`. A company token (JWT) is refused on this gateway. |
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

The gateway has to know the real address of the caller. The per-source rate limit and the audit record use it. Ambit
**never trusts `X-Forwarded-For`**: a caller can write any value into that header.

| `frontDoor.clientAddress` | Use it when | Notes |
|---|---|---|
| `direct` (default) | The load balancer passes the caller's address through (a layer-4 balancer that keeps the source address). | The Service has `externalTrafficPolicy: Local` (default), so a node does not hide the caller behind its own address. |
| `proxyProtocol` | The load balancer sends the PROXY protocol (version 1 or 2) before the TLS handshake. | The gateway then **refuses every connection without the PROXY header**. Turn the PROXY protocol on at the load balancer first, then set this value. **Only the load balancer may reach the gateway pods.** The header is not signed: anyone who can connect to the pods directly can write any address into it. So do not use `frontDoor.service.type: NodePort` (the chart refuses it), and close every other way in: a cloud firewall, `loadBalancerSourceRanges`, a network policy of your own. |

Not supported in this release: a layer-7 load balancer or a CDN that ends TLS and sends a new request with
`X-Forwarded-For`. Every request would count as coming from the load balancer. Use a layer-4 load balancer.

Ambit does not test cloud load balancers in CI. What is tested on kind: a connection with no PROXY header (`direct`), and
a connection that sends a made-up client address in a PROXY header (`proxyProtocol`). On your cloud, check the address
once after install: call the front door from a machine whose address you know, then read the record in Audit or in
ClickHouse (`LogAttributes['src.addr']`).

## Rate limits

Limits protect the gateway and the keys. They are on by default. Values are in `frontDoor.rateLimit`.

| Limit | Default | Counted | Counts |
|---|---|---|---|
| `perCredentialPerMinute` | 600 | by the ratelimit service and Valkey, **shared by all gateway pods** | requests with one valid key (by key ID) |
| `perSourcePerMinute` | 120 | by the same service, shared | requests from one source address that pass authentication, whatever key they carry |
| `perPodPerSecond` | 200 | in each gateway pod | every request that reaches the limit check. A second line: it still works when the ratelimit service is down. |

A request over a limit gets **429** (or, for an MCP request, `200` with a JSON-RPC error: agentgateway reports some errors of
an MCP request that way). The first two limits are per minute (a fixed window that starts at each full minute).

- **Refused requests are not counted.** agentgateway v1.7.0 checks the rate limits **after** it matches the route and checks
  the credential. A request for another path (404), or with no key, a wrong key, an expired key or an offboarded owner (403),
  is answered first and counts against no limit. These answers are cheap, but a flood of them is not slowed by Ambit.
  Limit that traffic at the edge (cloud firewall, WAF, load balancer), where you can see the source.
- **Cheap and expensive calls.** In this release there is only one kind of public call (`/mcp`), so there is one source
  limit. When sign-in endpoints are added later, they get their own, lower limit, so that a flood of sign-in attempts
  cannot use up the limit of MCP calls from the same address.
- **Many people behind one company address.** Callers behind one NAT address share the source limit. 120 per minute is a
  starting value for a small number of hosted agents, not for a company office. If agents of one platform call from a few
  addresses, raise `perSourcePerMinute` or put your own limit at the edge, where you know the traffic.
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
`FrontDoorConnection`. A hosted agent would otherwise receive the person's own app token. The front door will carry it only
when the person has agreed for that agent and that app, in a later change of this release series.

The `frontDoor` setting of a server request is never used: an approved request creates a server that is not on the front
door. Only an admin, with `kubectl`, in GitOps, or in the console later, puts a running server there.

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
| Network | the public gateway pods reach only the pods of front-door servers (network policy `ambit-mcp-servers-front-door`, pod label `ambitproject.dev/front-door`), and not the console, Dex or any other server |
| The console's token broker, and Dex | not reachable from the public gateway pods |
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
