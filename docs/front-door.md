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
| Routes | One: the exact path `/mcp` on `frontDoor.host`. Every other path is 404. A route that names `ambit-public` and is not `ambit-mcp-public` is refused by the Kubernetes API (a `ValidatingAdmissionPolicy`, `ambit-public-routes`). |
| Not reachable | The console (`/`, `/api/*`), Dex (`/dex/*`), the LLM host, SCIM and the app sign-in pages. They are not routes of `ambit-public`, so they answer 404 there. |
| Credentials | An Ambit API key with the MCP scope, in `x-api-key` or `Authorization: Bearer ambit_...`. A company token (JWT) is refused on this gateway. |
| Servers | Only the servers an admin marks for the front door (`Server` `spec.frontDoor`, see [Choose the servers](#choose-the-servers)). |

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

3. Point the DNS name `mcp-public.example.com` at the load balancer of the Service `ambit-public`:

   ```
   kubectl -n ambit-system get service ambit-public
   ```

4. Check it from outside (a key with the MCP scope, from the console page **Keys**):

   ```
   curl -si https://mcp-public.example.com/                       # 404
   curl -si https://mcp-public.example.com/mcp -X POST -d '{}'     # 403: no key
   ```

The main gateway, the console and Dex are not changed. To turn the front door off, set `frontDoor.enabled: false`: the
Gateway, the load balancer and the policies are deleted. The emergency stop for every key is `apiKeys.enabled: false`.

## Load balancers and the client address

The gateway has to know the real address of the caller. The per-source rate limit and the audit record use it. Ambit
**never trusts `X-Forwarded-For`**: a caller can write any value into that header.

| `frontDoor.clientAddress` | Use it when | Notes |
|---|---|---|
| `direct` (default) | The load balancer passes the caller's address through (a layer-4 balancer that keeps the source address). | The Service has `externalTrafficPolicy: Local` (default), so a node does not hide the caller behind its own address. |
| `proxyProtocol` | The load balancer sends the PROXY protocol (version 1 or 2) before the TLS handshake. | The gateway then **refuses every connection without the PROXY header**, so a caller cannot choose its own address. Turn the PROXY protocol on at the load balancer first, then set this value. |

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
| `perCredentialPerMinute` | 600 | by the ratelimit service and Valkey, **shared by all gateway pods** | requests with one valid key (by key ID). Requests without a valid key share one counter with this limit. |
| `perSourcePerMinute` | 120 | by the same service, shared | every request from one source address, including requests that are refused |
| `perPodPerSecond` | 200 | in each gateway pod | every request. A second line: it still works when the ratelimit service is down. |

A request over a limit gets **429**. The first two limits are per minute (a fixed window that starts at each full minute).

- **Cheap and expensive calls.** In this release there is only one kind of public call (`/mcp`), so there is one source
  limit. When sign-in endpoints are added later, they get their own, lower limit, so that a flood of sign-in attempts
  cannot use up the limit of MCP calls from the same address.
- **Many people behind one company address.** Callers behind one NAT address share the source limit. 120 per minute is a
  starting value for a small number of hosted agents, not for a company office. If agents of one platform call from a few
  addresses, raise `perSourcePerMinute` or put your own limit at the edge (cloud firewall, WAF), where you know the traffic.
- **When the ratelimit service is down.** `frontDoor.rateLimit.failureMode: FailOpen` (default) lets requests through
  uncounted, and the per-pod limit still applies. `FailClosed` refuses requests until the service is back.

Edge limits (a cloud firewall or WAF in front of the load balancer) are the operator's: Ambit does not manage them.

## Choose the servers

> The server list is added in the next change of this release series (`Server` `spec.frontDoor`). Until then the route
> `ambit-mcp-public` has no backend, so a request with a valid key passes the checks above and then gets an error from the
> gateway.

## Public API (experimental)

What this feature adds, all of it experimental in 0.6.x:

- Helm values `frontDoor.*` (all off or empty by default).
- The objects `Gateway ambit-public`, `HTTPRoute ambit-mcp-public`, `AgentgatewayParameters ambit-public`,
  `ConfigMap ambit-ratelimit-front-door`, `ValidatingAdmissionPolicy ambit-public-routes`, and the policies
  `ambit-mcp-public-auth`, `ambit-public-limits` and `ambit-public-access-log` (written by the controller).
- The public route: exact path `/mcp` on `frontDoor.host`, with a key in `x-api-key` or `Authorization: Bearer ambit_...`.
- The audit attribute `ambit.ingress` (`public` or `private`; written only when the front door is on).

## Measured

Measured on kind (see `test/stepfd.sh`).

| What | Measured |
|---|---|
| Private paths and hosts on the public port | see the table in the pull request |
