# OAuth sign-in for hosted MCP clients

> **Experimental in 0.6.x.** The values (`oauth.*`, `frontDoor.authHost`), the public endpoints on the sign-in host, the
> objects named here and the token claims may change in 0.7.0, with an upgrade note. They never change silently inside
> 0.6.x. Ambit claims **standards-based OAuth for MCP clients**. It does not claim support for a named platform: no
> platform has been tested with a live account.

Some hosted AI platforms cannot send a fixed API key. They ask the person to sign in, then call Ambit with a short-lived
token. This page is about the part of Ambit that makes that possible: **Ory Hydra**, which issues the tokens, and the
PostgreSQL database it needs. You run the database.

> **What is in this release series so far:** Hydra and its database; **`ambit-auth`**, the sign-in host; and the **check of
> tokens at the public MCP route**, so that a token opens exactly the tools its person ticked. Still to come in this series: the
> stop of one connection (together with the stop state machine of [Agents](agents.md)), key rotation, the database
> operations (backup, clean-up, alerts) and the guide for a platform.

It is **off by default** (`oauth.enabled: false`).

## Before you turn it on

The chart refuses `oauth.enabled: true` unless each of these is set, and the message names the value.

| Needed | Why |
|---|---|
| `frontDoor.enabled: true` and its requirements ([The public front door](front-door.md)) | OAuth is served on the public Gateway. |
| `frontDoor.authHost`, for example `auth.example.com` | The **sign-in host**: a second name on the same public Gateway. It must differ from `frontDoor.host`, `console.host`, `llm.host` and `mcp.host`. Its DNS name points at the same load balancer. |
| A certificate for **both** names in `frontDoor.tls.certificateSecret` | One Secret serves both listeners. |
| A PostgreSQL database that you run (below) | Hydra keeps clients, consents and refresh tokens in it. |
| An OIDC client at your identity provider (below), and `oauth.signIn.*` | People sign in with the company account. |
| `limits.enabled: true` | Sign-in and registration limits are counted in Valkey, shared by all replicas. |
| `oauth.database.dsnSecret`, `oauth.database.caSecret` | The address of the database and the CA that signed its certificate. |
| `oauth.hydra.systemSecret` | A random string that encrypts Hydra's keys and tokens in the database. |
| `oauth.cookieSecret` | A random string of 32 or more characters that protects pending sign-ins in the person's browser. |

Look at the routes first, as for the [front door](front-door.md#before-you-turn-it-on): the admission policy checks routes when
they are created or changed, so a route that names `ambit-public` **before** OAuth is turned on is not checked. No route
except the ones this chart writes may name it:

```
kubectl -n ambit-system get httproutes -o custom-columns=NAME:.metadata.name,GATEWAYS:.spec.parentRefs[*].name
```

Create the Secrets first. Nothing secret is ever written into the chart's values or objects.

```
kubectl -n ambit-system create secret generic ambit-oauth-db \
  --from-literal=dsn='postgres://hydra:PASSWORD@db.example.com:5432/hydra?sslmode=verify-full&sslrootcert=/ca/ca.crt&max_conns=8&max_idle_conns=4'
kubectl -n ambit-system create secret generic ambit-oauth-db-ca --from-file=ca.crt=./database-ca.pem
kubectl -n ambit-system create secret generic ambit-oauth-hydra --from-literal=system="$(openssl rand -hex 32)"
kubectl -n ambit-system create secret generic ambit-oauth-cookie --from-literal=cookie="$(openssl rand -hex 32)"
kubectl -n ambit-system create secret generic ambit-oauth-idp --from-literal=client-secret='THE-CLIENT-SECRET-OF-YOUR-IDENTITY-PROVIDER'
```

Use a PASSWORD of letters and digits (it is part of a URL). **Keep the `system` secret with your database backups:**
without it the database cannot be read, and every connected platform has to connect again.

```yaml
frontDoor:
  enabled: true
  host: mcp.example.com
  authHost: auth.example.com
  tls: { certificateSecret: ambit-public-tls }   # covers both names
oauth:
  enabled: true
  database:
    dsnSecret: { name: ambit-oauth-db, key: dsn }
    caSecret: { name: ambit-oauth-db-ca, key: ca.crt }
  hydra:
    systemSecret: { name: ambit-oauth-hydra, key: system }
  cookieSecret: { name: ambit-oauth-cookie, key: cookie }
  signIn:
    issuer: https://idp.example.com          # as your provider writes it, no final slash
    clientID: ambit-auth
    clientSecret: { name: ambit-oauth-idp, key: client-secret }
    groupsClaim: groups
  registration:
    dynamic: { enabled: true, redirectHosts: [chatgpt.com, claude.ai] }
```

### Register Ambit at your identity provider

Once, at Okta, Entra ID, Google, Keycloak or another OIDC provider: one **confidential web client**.

| Setting | Value |
|---|---|
| Flow | Authorization code only. PKCE with S256 required. |
| Redirect URI | Exactly `https://auth.example.com/callback` (your `frontDoor.authHost`). No wildcard, no other address. |
| Scopes | `openid email profile` |
| Token claims | A `groups` claim in the ID token (the name is `oauth.signIn.groupsClaim`), and `email` with `email_verified: true`. |
| Client authentication | `client_secret_basic` |

Ambit accepts ID tokens only from `oauth.signIn.issuer` (an exact match), for this client. One provider per install. A
provider whose certificate your cluster does not trust gets its CA in `oauth.signIn.caSecret` (key `ca.crt`).

## How a person connects a platform

1. The platform calls the MCP URL without a token and gets `401` with a pointer to `/.well-known/oauth-protected-resource`.
   The answer carries `WWW-Authenticate: Bearer resource_metadata="https://<host>/.well-known/oauth-protected-resource/mcp"`,
   and the gateway serves that document itself: this MCP URL, and `authorization_servers: [https://<authHost>]`.
2. It reads Ambit's authorization server document at `https://<authHost>/.well-known/oauth-authorization-server`, and, if it
   has no registered client, registers itself at `POST /register` (below).
3. It sends the person's browser to `/oauth2/auth`. `ambit-auth` checks the request, Hydra checks the client and the exact
   redirect address, and the browser goes on to `/login`.
4. `/login` sends the browser to **your identity provider** (code flow, PKCE, `state`, `nonce`). The person signs in there,
   with your MFA and your rules. Ambit never sees a password.
5. `/callback`: Ambit checks `state`, the provider's `iss` (required when the provider says it sends it), the ID token's
   signature, issuer, audience (several audiences need `azp` equal to the client), expiry and nonce; that every address in
   the provider's discovery document is https; that the email is verified, is not a service account
   (`@svc.ambit.internal`) and is not offboarded. The person is identified by the **email address** (as everywhere in
   Ambit); an identity provider that gives an old address to a new person gives that person the old person's
   permissions, as it does for keys: change the person's address in the provider before you reuse it, and offboard the old one.
6. `/consent`: a page that names the platform **as it declared itself** (Ambit has not checked the name), the address it
   returns to, and the person. It lists the MCP servers that the person's AccessRules allow **and** that the front door
   serves. **None is ticked.** The person ticks what the platform may use, and, for servers that need the person's own
   sign-in to an app, whether the platform may use it. The platform gets nothing the person did not tick.
7. The browser returns to the platform's address with `code`, `state` and `iss` (RFC 9207: the platform can check which
   authorization server answered). The platform exchanges the code at `POST /oauth2/token` and gets a JWT access token
   (15 minutes) and a refresh token.

Consent creates a **person-mode agent** for the person (see [Agents](agents.md)) named after the platform, and a grant
(`g-...`). The token carries `ambit_agent`, `ambit_grant`, `ambit_scope` (the ticked servers), `ambit_agent_mode`,
`ambit_agent_owner_at_issue` and `ambit_platform_declared`. The platform is declared by the platform's redirect host
(`chatgpt.com` is `chatgpt`); an unknown host is `other`. It is a label, never a proof.

**What a request must look like.** Only the code flow, PKCE with `S256`, the query response mode, and **exactly one**
`resource` parameter equal to the MCP URL (`https://<host>/mcp`); anything else is `400` before Hydra sees it. No
parameter may appear twice. Scopes are `mcp` and `offline_access` (a refresh token needs the second; if a client sends no
scope it gets both). The `resource` is checked again at login and at consent, from the request Hydra stored.

**Two sign-ins in one browser** (two platform tabs) work: each has its own short-lived cookie (`__Host-ambit_tx_<handle>`,
encrypted and authenticated, bound to its own name, deleted when used), and each consent form its own cookie. At most six
sign-ins are kept per browser. One limit comes from Hydra: it keeps one CSRF cookie per client, so a second sign-in of
the **same client** in the same browser makes the first one fail (the platform shows an error, and the person connects
again). Different platforms do not disturb each other.

**The consent page opens only in the browser that signed in.** The callback gives that browser a sealed cookie, and the consent
page and its form ask for it. A consent link that is copied out of the browser (a message, a log) shows nothing about the
person and creates no agent.

**Each Hydra challenge is decided once.** Hydra v26.2.0 lets a rejected consent request be read and accepted again. Ambit
marks a login or consent challenge as decided in Valkey, so a second decision (a replay, or two clicks at once) is refused
and creates no agent. The marks are kept in Valkey: the bundled Valkey has no persistence, so after a restart of Valkey a
challenge that was decided less than 20 minutes ago could be decided again, but only in the browser that holds the
cookies of that sign-in.

## Registering clients

Platforms register themselves (`POST /register`, RFC 7591) when `oauth.registration.dynamic.enabled` is true. Only these
are accepted:

- redirect addresses of 1 to 5 entries: `https` on a host in `redirectHosts` (exact name, port 443, no user name, no
  fragment), or for a native client `http` on `localhost`, `127.0.0.1` or `[::1]` with any port;
- public clients only (`token_endpoint_auth_method: none`, PKCE), grants `authorization_code` and `refresh_token`,
  response type `code`, scopes `mcp` and `offline_access`;
- `client_name` of at most 100 characters (control characters and angle brackets are removed);
- no `iss`, `code`, `state`, `error` or `error_description` in the query of a redirect address (Ambit adds them).

A native client that needs a changing port registers `http://127.0.0.1` or `http://[::1]` (Hydra matches the port of
loopback IP addresses only; for `http://localhost` the port must be exactly the registered one).

The cap of `maxClients` is counted in Valkey and is approximate for up to ten minutes (the clean-up compares it with Hydra every
ten minutes). Each registration is limited per caller address (`perSourcePerHour`, 10 by default), and at most `maxClients` clients exist (500). A client
that nobody used for `unusedClientDays` (30) is deleted; every consent and every refresh counts as use, written at most
once a day. Clients that an admin creates in Hydra by hand are never touched.

**Which address is "the caller".** The public Gateway writes the caller's address from the connection (or the PROXY
protocol header, `frontDoor.clientAddress`) into the header `X-Ambit-Client-Address`, replacing anything the caller sent,
and removes `X-Forwarded-For`, `X-Real-IP` and `Forwarded`. `ambit-auth` reads only that header, and only the Gateway's
pods may reach its port 8080. An IPv6 caller counts by its /64. Behind a layer-7 proxy that hides the address, every
caller looks like one address: see [The public front door](front-door.md#load-balancers-and-the-client-address).

## Limits

Separate buckets per caller address, so that a flood of one kind never uses up another, counted in Valkey and therefore
shared by every replica. If Valkey does not answer, each replica counts for itself: a limit never disappears.

| Bucket | Paths | Default |
|---|---|---|
| discovery | `/.well-known/oauth-authorization-server` | 600 per minute |
| sign-in | `/oauth2/auth`, `/login`, `/callback`, `/consent`, `/error` | 300 per minute |
| registration | `/register` | 10 per hour |

These first values are not measured yet (the load test is part of a later change). Many people behind one company address
share a bucket; the sign-in bucket is large for that reason.

## Tokens at the public MCP route

The public route `ambit-mcp-public` has two rules. A request that carries an Ambit **key** (`x-api-key`, or `Authorization: Bearer ambit_...`)
takes the key rule, exactly as without OAuth: a request with a wrong key gets **403**. Every other request to `/mcp` takes the **token
rule**: without a valid token it gets **401** with the `resource_metadata` challenge above, so that a platform can start a sign-in.

The token rule accepts a JWT only if: the issuer is Ambit's sign-in host (`https://<authHost>`), the signature fits a key that Hydra
publishes (the gateway reads them inside the cluster and keeps them for 5 minutes), the audience is exactly this MCP URL, `exp`, `aud`
and `sub` are there, the person has an email that is not a service account, and the token carries an agent, a grant and a scope list
(the consent page writes all three into every token). The same checks of every call as for a key then follow: the offboarding list, the
stop list, the person's AccessRules **and** the scope of the token (`ambit_scope`): a tool outside the scope is not listed and not callable,
on every call. Only servers that are on the front door are there at all.

Tokens do not work on the private route (it takes tokens of your company's provider), and keys do not work on the sign-in host.

## Stopping a connection, and offboarding

- **Offboarding** (SCIM deactivation): the person's email goes to the offboarding list; the gateway refuses their tokens on both Gateways
  (the same policy as for keys), a new sign-in is refused, and a refresh is refused by the token hook. Their agents are ended with the
  stop state machine of [Agents](agents.md#stopping-an-agent) (an agent that is not active makes the hook refuse its refresh for good, also after
  a reactivation).
- **Ending one connection**: the person (or an admin) deletes or stops the agent of the platform ([Agents](agents.md): a connection is a
  person-mode agent, listed with its platform). The hook refuses its refresh at once. A token that was already issued is refused by the
  gateway through the stop list as soon as the agent is stopped; without the stop list it expires within 15 minutes.
- **Keeping a platform out**: an admin deletes the platform's OAuth client (`GET /api/admin/oauth/clients`, `DELETE
  /api/admin/oauth/clients/{id}`): its codes and refresh tokens go with it and no new sign-in works.

The token hook of `ambit-auth` (Hydra calls it for every token request) refuses a **refresh** when the person is on the offboarding list
(`ambit-offboarded`), the agent or grant is on the stop list (`ambit-agent-stops`), the console says the agent is not active or does not own
the grant, or the grant is older than `oauth.maxGrantDays` (30). Hydra then answers `access_denied` and does not use up the refresh token,
so a restart of the agent works without a new consent. If the lists cannot be read for 45 seconds, a list has an entry it cannot read, the
request is incomplete (no agent, grant, email or grant type), or the console does not answer, the hook refuses (a refresh waits; tokens
already issued are not affected). **The groups in a token are those of the sign-in** (the console does not know a person's groups between
sign-ins): a group that the person loses at your identity provider counts until the grant ends, at most `oauth.maxGrantDays`; lower it if that
is too long.

## If a signing key may be stolen

**First, block every token of the sign-in host at once.** This edit of one policy needs no Helm run, no Hydra, no keys and none of Ambit's
own components; the gateway applies it within a second. API keys and service account keys are not touched (they take the other rule).

```
kubectl -n ambit-system patch agentgatewaypolicy ambit-mcp-public-oauth --type json \
  -p '[{"op":"add","path":"/spec/traffic/authorization/policy/matchExpressions/-","value":"false"}]'
```

Every request with a token now gets 403 on every public Gateway pod, also with a token that the stolen key signed (the gateway checks this
clause after it has checked the signature, and `false` is never true). The next `helm upgrade` removes the edit; to keep the block, set
`oauth.emergencyBlock: true`. To lift it, remove the clause (`"op":"remove"` with its index, the last one) or set the value to `false` and
upgrade. Platforms then get 403 on `/mcp`: they stay connected, and work again when you lift the block.

The block does **not** replace the new key: a token signed with the stolen key is valid again as soon as the block is lifted, until the
gateway's copy of the old key is gone. The removal and replacement of the key (every connection then connects again) comes with the next change;
keep the block until it is done. Measured in the end-to-end test, with Hydra down and the old key cached on the gateways, the block takes
a fraction of a second (the numbers are in the pull request).

## PostgreSQL

**Plain statement.** Turning on OAuth means you run a PostgreSQL database for Ambit, with backups. If the database is
down, people cannot connect a platform or refresh a connection. Once the gateway checks tokens (the next change of this release series), a call that already carries a valid access
token keeps working for up to 15 minutes. Ambit does not install, back up, upgrade or fail over this database. A front door with API
keys only needs no database.

| Requirement | How Ambit checks it |
|---|---|
| PostgreSQL **15, 16 or 17** | The start check reads `server_version_num`. CI runs 17 and 15. |
| TLS, and the server's name proven: `sslmode=verify-full` and a CA | The start check refuses every other `sslmode` (`disable`, `allow`, `prefer`, `require`, `verify-ca`, none) before it connects, and then checks that its own session uses TLS. |
| One database and one owner role, **not a superuser** | The start check refuses a superuser role, and a session that runs as another role than the one that logged in. The role needs no other right. |
| Connections: `max_conns` in the address, and `pods × max_conns + 1` within the role's `CONNECTION LIMIT` | The address must name `max_conns` (1 to 64; the chart's example uses 8); the start check compares the role's limit. |

The address may carry only these parameters: `sslmode`, `sslrootcert`, `max_conns`, `max_idle_conns`, `max_conn_lifetime`, `max_idle_conn_time` and `connect_timeout`. Any other (a `host`, `options`, a startup setting) is refused, and its name is not printed. When the connection fails, the message holds a fixed text and, for a refusal by the server, the SQLSTATE code; it never holds the server's own words or the address.

The check is the init container `check-database` of each Hydra pod (`ambit-auth check-database`). It changes nothing in
the database and never prints the address or the password. If it fails, the pod does not start and its log names the fix:

```
kubectl -n ambit-system logs ambit-hydra-1 -c check-database
```

### What was tested, exactly

| Tested on kind in CI | What it shows |
|---|---|
| PostgreSQL 17 and 15, TLS only (a `hostnossl` line that rejects plain connections) | Hydra starts, migrates and serves on both. |
| The start check, 16 cases | Every refused `sslmode`, a missing CA or `max_conns`, a wrong CA, a name that is not in the certificate, a superuser, too few connections and a wrong password; no log repeats a password. A login role that differs from the role in effect or from the user in the address is covered by unit tests only. |
| Two migrations of an empty PostgreSQL 15 database started one after the other, and a repeat | Two that overlap can fail, one or both (Hydra v26.2.0 races on its first table; the test starts them at once but does not prove that they overlap); a run on its own succeeds. This is why Hydra's pods start one at a time. |
| Changing the address from one database to another and back (a Secret change) | A new, empty database gets its own signing key; going back finds the first key again. |

Measured separately on kind during the design (a prototype, not in CI): a logical dump and restore (`pg_dump -Fc`,
`pg_restore`), reconnection of Hydra after the database was replaced behind its Service name (switching a Kubernetes
Service to another instance: no errors in 90 s), a 30 second outage (sign-in and refresh answered 503 for 27 to 33
seconds, then recovered), and one migration step down and up again.

**Not tested:** any managed PostgreSQL service (Amazon RDS or Aurora, Google Cloud SQL, Azure Database for PostgreSQL,
CloudNativePG), its fail-over, replication, point-in-time restore, certificate or password rotation, and an upgrade of
Hydra across schema versions. Do not read this page as saying they work. Use the checklist below.

### Acceptance checklist for your database

Run this once on your own database before you rely on OAuth. It does not depend on a provider. Write down the numbers.

| # | Check | You have it when |
|---|---|---|
| 1 | **RPO and RTO.** Ask your provider what a fail-over and a restore lose and how long they take. | You know how many minutes of changes you can lose (RPO) and how long sign-in and refresh may be down (RTO), and both fit what your platforms need. |
| 2 | **A real refresh across a fail-over.** Connect a platform (or use a test client), keep its refresh token, trigger a fail-over of your database, then refresh. | The refresh succeeds, and sign-in works again within your RTO. Hydra reconnects by itself; if it does not, the pods are restarted. |
| 3 | **Restore point.** Restore a backup to a new instance and point the Secret `oauth.database.dsnSecret` at it. | Connections that were refreshed **after** the backup was taken fail with `invalid_grant`: those people connect their platform again. Connections not used since the backup keep working. Stops and offboarding are not lost: they live in Kubernetes, not in PostgreSQL. |
| 4 | **Password and CA rotation.** Change the Secret, then delete the Hydra pods one at a time. | The pods start with the new password or CA and the start check passes. Rotate the CA with both CAs in the file during the change. |
| 5 | **Upgrade rollback.** Before you upgrade Ambit (a new Hydra version), take a backup. | You have restored that backup once on a test database and started the previous Hydra version on it. A Hydra migration is not always reversible; the backup is the way back. |
| 6 | **Monitoring.** | You are alerted when Hydra's pods are not Ready, and when `POST /oauth2/token` answers 5xx. |

## What runs

| Object | What it is |
|---|---|
| `StatefulSet ambit-hydra` (2 pods) | Hydra v26.2.0 (image pinned by digest). Init containers: `check-database`, `migrate` (`hydra migrate sql up`). Containers: `hydra`, `hydra-keys` (below). |
| `Service ambit-hydra-public` (4444), `Service ambit-hydra-admin` (4445) | ClusterIP only. |
| `NetworkPolicy ambit-hydra` | Port 4444: the public Gateway's pods, agentgateway's controller (it fetches the keys) and `ambit-auth` (its authorize front). Port 4445: `ambit-auth` and the console only. Every other pod is refused. |
| `Deployment ambit-auth` (2 pods), `Service ambit-auth`, `NetworkPolicy ambit-auth` | The sign-in pages, registration and the token hook. Stateless. Port 8080 (pages) is open to the public Gateway's pods only, 8081 (token hook) to Hydra's pods only, 8082 (health, metrics) to the cluster. It reads two ConfigMaps by name (`ambit-offboarded`, `ambit-agent-stops`) and writes nothing in Kubernetes. |
| `HTTPRoute ambit-auth-app` | The pages: `GET /.well-known/oauth-authorization-server`, `/oauth2/auth`, `/login`, `/callback`, `/consent`, `/error`, `POST /consent`, `POST /register`. |
| `HTTPRoute ambit-auth-hydra` | On the sign-in host, three exact paths: `POST /oauth2/token`, `POST /oauth2/revoke`, `GET /.well-known/jwks.json`. Everything else of Hydra (registration, discovery, user info, device flow, sessions, health, the admin API) has no route and answers 404. |
| `PodDisruptionBudget ambit-hydra` | One pod stays up during a drain. |

The **admin API can read Hydra's private keys, and it has no password of its own.** It is never routed, and the network policy
admits only the two callers above, **by pod label**. So the rule holds only while nobody who can create pods in the Ambit
namespace is untrusted (a pod with the label `app.kubernetes.io/name: ambit-auth` would be let in), and only on a cluster
network that enforces NetworkPolicy (kindnet, Calico, Cilium and the major clouds' policy engines do; check yours, the
test `test/stepoauth.sh` shows how). Treat the right to create pods in this namespace as the right to read the signing key.

Two more things widen that rule, and neither is a bug of the chart. **First**, a NetworkPolicy does not limit traffic that a pod receives
from the node it runs on: a process on the node that runs a Hydra pod can reach port 4445 of that pod. **Second**, NetworkPolicies add up:
another policy in this namespace that admits more pods to Hydra's pods widens the access, whatever this chart says. Whoever can change
NetworkPolicies, or run a process on the nodes of Hydra's pods, can read the signing key.

The Kubernetes API refuses any route on the public Gateway that the chart did not write (`ValidatingAdmissionPolicy ambit-public-routes`):
the name, the exact paths, the backend, the listener, and the exact header filter of each route. The policy checks a route when it is created or changed: a route that already named `ambit-public` before the policy existed is not checked (see "Before you turn it on" in [front-door.md](front-door.md)). A route may not rewrite a path, mirror a
request or edit other headers. The policy does not pin the sign-in host of the Hydra route (the host comes from `frontDoor.authHost`, which
the policy cannot know when you enable OAuth in the same upgrade); it refuses a wildcard host, and the listener `https-auth` admits only the
host `frontDoor.authHost` whatever the route says.

Hydra's own dynamic client registration is turned off. Access tokens are signed JWTs that live `oauth.accessTokenMinutes`
(default 15); a refresh token not used for `oauth.refreshTokenIdleDays` (default 30) stops working; PKCE is required for
every client.

## Signing keys

Hydra signs access tokens with one RSA key. Two things matter:

- **Exactly one key** in the signing set `hydra.jwt.access-token` (and in `hydra.openid.id-token`). Hydra v26.2.0 stores
  the key ID of a grant when it is created, and signs a refreshed token with the current key. With two keys, half of the
  tokens name a key that did not sign them, and the gateway refuses them.
- **The key must exist before Hydra serves.** Hydra creates a missing key set by itself at the first request for it,
  including the first fetch of its public key list. Two pods that start together on an empty database each create a key.

So Hydra's pods start **one at a time** (`StatefulSet`, `OrderedReady`). The first pod finds an empty database and its
container `hydra-keys` creates the one key of each set; only then is the pod Ready, and the second pod starts. Nothing
needs a Helm hook or a Job, so Argo CD and `helm template` give the same result. `hydra-keys` keeps checking: **a pod
whose signing set is missing or holds two keys is not Ready**, so it gets no request (the check runs every few seconds: a
second key that appears is noticed within that time). If two pods run when a database is replaced by an empty one, both may
create a key; both then report two keys, stay out of service, and the log says so: remove one key by hand. It never deletes a key; which key
to keep is your decision (the pod's log says `key set ... holds 2 keys`).

There is no scheduled key rotation in 0.6.0. After a rotation, refreshed tokens of existing connections carry the old key
ID and are refused until the person connects again (a known defect of Hydra v26.2.0). A key
rotation is therefore a planned reconnect of every OAuth connection. The procedure comes with the next changes of this
series.

## Operating

- **One Hydra pod.** With `oauth.hydra.replicas: 1` an update replaces the only pod: sign-in and refresh are unavailable
  until the new pod is Ready, and a bad change (a wrong Secret) leaves them unavailable until you fix it. Keep two pods for
  a roll-out that never takes the service down.
- **A bad change stops one pod, not the service (two pods).** If a change makes the start check fail (for example a Secret with
  `sslmode=require`), the StatefulSet stops at the first pod it updates; the other pod keeps serving. A StatefulSet does
  not replace a pod that failed its start when you fix the template: after you fix the Secret or the values, **delete the
  stuck pod** (`kubectl -n ambit-system delete pod ambit-hydra-1`) and the roll-out goes on.
- **Moving to another database** (a changed address): the pods roll one at a time, so for a short time they use different
  databases. Set `oauth.hydra.replicas: 1` for the move if that matters. A new, empty database gets a new signing key.
- **Upgrading Hydra.** A new Hydra version is a reviewed change of Ambit (the image digest changes). Back up the database
  first. The `migrate` init container applies the new schema when the first pod updates, while the other pod still runs the
  old version: read the Hydra release notes for schema changes before you upgrade.
- **Turning it off.** `oauth.enabled: false` removes Hydra, its Services, policy, route and the sign-in listener. The
  database and its content stay where they are.

## Internal API of the console (experimental)

`ambit-auth` never writes agent records: only the console may. It calls the console on port 8085 with the pod's own service account
token (a projected token whose audience is `ambit-console`, read again for every call). The console asks the Kubernetes API whether the token is
valid (TokenReview) and refuses every service account but `ambit-auth`'s; a network policy admits only `ambit-auth`'s pods to the port.

| Call | Answer |
|---|---|
| `POST /internal/oauth/v1/servers` `{email, groups}` | `{"servers":[{"name","display"}]}`: the servers that the person's AccessRules allow (at least one tool) **and** the front door serves. A service account, an offboarded person and a made-up address get none. |
| `POST /internal/oauth/v1/agents` `{owner, platform, displayName, scope, connections, grantID}` | `{"name": agent}`: a person-mode agent for the person, with the limits, offboarding check and names of an agent made by hand. `409` `{"error"}` when the console refuses (too many agents, offboarded). |
| `POST /internal/oauth/v1/active` `{agent, grant}` | `{"active": bool}`: the agent exists, is active, belongs to a person who is not offboarded, and owns this grant. |

## Platforms that are connected (console)

A connection is a person-mode agent with a grant: it is listed by `GET /api/agents` (`platform`, `scope`, `state`) and ended with `DELETE /api/agents/{name}`
([Agents](agents.md)). An admin lists the OAuth clients with `GET /api/admin/oauth/clients` (name as the platform declared it, redirect hosts, how it
registered, last use) and deletes one with `DELETE /api/admin/oauth/clients/{id}`.

## Public API (experimental)

- Helm values `oauth.*` and `frontDoor.authHost` (all off or empty by default).
- The objects `StatefulSet`, `Services`, `ConfigMap`, `ServiceAccount`, `PodDisruptionBudget` and `NetworkPolicy`
  `ambit-hydra*`, `HTTPRoute ambit-auth-hydra`, and the Gateway listener `https-auth`.
- The public endpoints on `frontDoor.authHost`: `POST /oauth2/token`, `POST /oauth2/revoke`, `GET /.well-known/jwks.json`,
  `GET /.well-known/oauth-authorization-server`, `GET /oauth2/auth`, `GET /login`, `GET /callback`, `GET` and `POST
  /consent`, `GET /error`, `POST /register`.
- The console endpoints `GET /api/admin/oauth/clients`, `DELETE /api/admin/oauth/clients/{id}`, and the internal API above.
- The routes rule names `keys` and `oauth` of `HTTPRoute ambit-mcp-public`, the policy `ambit-mcp-public-oauth`, and `oauth.emergencyBlock`, `oauth.maxGrantDays`.
- The access token claims `email`, `groups`, `ambit_agent`, `ambit_agent_mode`, `ambit_agent_owner_at_issue`,
  `ambit_platform_declared`, `ambit_grant`, `ambit_scope`.
- Helm values `oauth.signIn.*`, `oauth.cookieSecret`, `oauth.registration.*`, `oauth.auth.*`.
- The internal console API above, and the command `ambit-auth` (`serve`, `check-database`, `hydra-keys`) in the Ambit image.
