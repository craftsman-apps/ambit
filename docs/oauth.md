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
publishes (the gateway reads them inside the cluster and keeps them for 5 minutes; after an install the first token was accepted on every
pod within 31 seconds in the test, and Ambit does not wait for the key set before it serves), the audience is exactly this MCP URL, `exp`, `aud`
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
  person-mode agent, listed with its platform). The hook refuses its refresh within a few seconds (it reads the stop list every 3 seconds, and
  it asks the console whether the agent is active). A token that was already issued is refused by the gateway through the stop list as
  soon as the Gateway has the policy of the stop. The person's other connections are not touched. A stopped agent can be started again only
  after the retention of the stop entry (`agents.stopRetentionMinutes`, 25 minutes by default, longer than a token lives), so that an old token
  cannot work again when the entry goes (the chart refuses a retention shorter than `oauth.accessTokenMinutes` plus 5). Hydra cannot revoke
  one grant, so a stop takes the grants off the agent's record instead: the old refresh token never works again, also after the agent is
  started, and the platform connects again (a new consent). The test
  `test/stepoauth.sh` (check 14) stops an agent through the console's API with the real controller and prints the measured times
  (the token refused on every pod, the refresh refused, the console saying "stopped").
- **Keeping a platform out**: an admin deletes the platform's OAuth client (`GET /api/admin/oauth/clients`, `DELETE
  /api/admin/oauth/clients/{id}`): its codes and refresh tokens go with it and no new sign-in works.

The token hook of `ambit-auth` (Hydra calls it for every token request) refuses a **refresh** when the person is on the offboarding list
(`ambit-offboarded`), the agent or grant is on the stop list (`ambit-agent-stops`), the console says the agent is not active, does not own
the grant, or the scope of the token is no longer within the agent's scope (a scope that was narrowed ends the refresh: the platform connects
again, and a token already issued keeps its scope until it expires, at most 15 minutes), or the grant is older than `oauth.maxGrantDays` (30). Hydra then answers `access_denied` and does not use up the refresh token
(a person who is reactivated after offboarding refreshes again; an agent that was stopped does not: its grants are gone). If the lists cannot be read for 45 seconds, a list has an entry it cannot read, the
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

**In CI, on kind** (`test/stepoauth.sh`, checks 4 to 10, 16 and 17), with PostgreSQL 17 and 15 that this test runs itself, TLS only, one instance, no replication:

| What | Result |
|---|---|
| PostgreSQL 17 and 15, TLS only (`hostnossl` rejects plain connections) | Hydra starts, migrates and serves on both. |
| **P1** TLS | A connection without TLS is refused by the server; a session with TLS uses @@TLSV@@; every connection of Hydra uses TLS. |
| The start check, 16 cases | Every refused `sslmode`, a missing CA or `max_conns`, a wrong CA, a name that is not in the certificate, a superuser, too few connections and a wrong password; no log repeats a password. A login role that differs from the role in effect or from the user in the address is covered by unit tests only. |
| Two migrations of an empty PostgreSQL 15 database | Two that overlap can fail, one or both (Hydra v26.2.0 races on its first table); a run on its own succeeds. This is why Hydra's pods start one at a time. |
| Changing the address from one database to another and back | A new, empty database gets its own signing key; going back finds the first key again. |
| **P3** pool | 60 concurrent clients for 15 s on both Hydra pods: @@P3@@ |
| **P4** logical dump | `pg_dump -Fc` of the live database: @@P4@@ |
| **P5** restore and a moved name | The dump restored into a new PostgreSQL 17 (@@P5RESTORE@@); the Kubernetes Service `ambit-pg17` pointed at the new instance and the old one stopped, while a probe asked Hydra twice a second: @@P5FAIL@@. Then, as a platform and a person see it: a connection that was not used since the dump refreshes (200); a connection that refreshed after the dump is refused with `invalid_grant` and must connect again; a new sign-in works. |
| **P6** the database pod is deleted (its volume stays) | @@P6@@ The data is there: a connection refreshes. |
| **30 second outage** (the database is stopped, then started) | @@OUTAGE@@ |
| Rotation of the database password (a role change, a new Secret, the pods rolled one at a time) | @@ROT@@ |
| A CA file with two certificates | Passes the start check with `sslmode=verify-full` (the middle of a CA rotation). |
| **Upgrade rollback** with `hydra migrate sql down` | **Do not do this on a live database.** @@MIG@@ |
| The janitor (CronJob `ambit-hydra-janitor`, run once by hand) | @@JAN@@ |
| The clean-up of unused clients | A client that was registered dynamically and not used for 31 days is deleted when `ambit-auth` starts; one used 29 days ago and a client that an admin made are kept (also unit: `TestJanitorDeletesUnusedClients`). |

**What this shows, and what it does not.** It shows how Hydra behaves when its database is stopped, deleted, restored from a dump or moved to another
instance on one cluster, and what people see. **It is not a test of any managed PostgreSQL service.** Not tested: Amazon RDS or Aurora, Google Cloud SQL,
Azure Database for PostgreSQL, CloudNativePG or any other service; **their fail-over** (a real replica taking over, a DNS name that moves, the time it
takes), replication lag, point-in-time restore, the loss of the last seconds before a crash (RPO), the certificate (CA) rotation of a real service, the
rotation of a password by the service, and an upgrade of Hydra across schema versions. **Do not read this page as saying that they work.** Use the checklist below
on your own database. What kind showed is how Hydra behaves; your provider's numbers are yours to measure.

### Acceptance checklist for your database

Run this once on your own database before you rely on OAuth. It does not depend on a provider. Write down the numbers, with the date.

| # | Check | You have it when |
|---|---|---|
| 1 | **RPO and RTO.** Ask your provider what a fail-over and a restore lose and how long they take, then measure both once. | You know how many minutes of changes you can lose (RPO) and how long sign-in and refresh may be down (RTO), and both fit what your platforms need. On kind, with a stopped database, sign-in and refresh were down about as long as the database (@@OUTAGEREC@@ after it was back). |
| 2 | **A real refresh across a fail-over.** Connect a platform (or use a test client), keep its refresh token, trigger a fail-over of your database, then refresh with that token. | The refresh succeeds (200, a new refresh token), and sign-in works again within your RTO. Hydra reconnects by itself; if it does not within a few minutes, delete the Hydra pods. A refresh that fails with `invalid_grant` means that the fail-over lost the last refresh: the data of your replica was older than the primary (that is your RPO, measured). |
| 3 | **Restore point.** Restore the newest backup to a new instance and point the Secret `oauth.database.dsnSecret` at it (or move the name). | Connections that refreshed **after** the backup was taken fail with `invalid_grant`: those people connect their platform again. Connections not used since the backup keep working. Connections that were ended (stopped, deleted, offboarded) stay ended: the stop list and the offboarding list are in Kubernetes, and the token hook asks the console whether the connection is still active. |
| 4 | **Password and CA rotation.** Change the role's password, then the Secret, then roll the pods one at a time (see [Rotating the password and the CA](#rotating-the-password-and-the-ca)). Do the same with your CA: put both CAs in the file, roll, then remove the old one. | The pods start with the new password or CA and the start check passes; a refresh works afterwards; the errors during the change are within what you accept. |
| 5 | **Upgrade rollback from the previous Hydra schema.** Before you upgrade Ambit (a new Hydra version), take a backup. On a copy of your database, restore that backup, start the **previous** Hydra version on it, and refresh a connection. | The previous version starts on the restored database and serves. **Do not use `hydra migrate sql down` as the rollback:** on Hydra v26.2.0 one step down deleted every login and consent record and, through the database's cascade rule, every token. The backup is the way back. |
| 6 | **Monitoring.** | You are alerted when Hydra's pods are not Ready, when `POST /oauth2/token` answers 5xx, when the newest backup is old and when connections run out ([Alerts](alerts.md)); and you have seen each alert fire once on a test. |

## Operations

This is the runbook for whoever runs the database and the cluster. Every number from kind is from the tests above: your database is not kind.
"Measured" means measured there. Commands use the namespace `ambit-system`.

### What people see when the database is not there

| Part | Database down | After it is back |
|---|---|---|
| A call with an access token that was already issued | **works** for up to 15 minutes (`oauth.accessTokenMinutes`), also at the gateway: it needs no database | works |
| A new sign-in (connecting a platform), registration | fails (503): Hydra's pods are not Ready | works again by itself |
| A refresh | fails (503). The platform tries again later; a refresh that Hydra answered with an error does not use up the refresh token | works again by itself |
| Stops and offboarding | work: they live in Kubernetes | |
| The database check at start | a pod that starts while the database is down fails in its init container `check-database` and is tried again by Kubernetes: it starts a few minutes after the database is back, at most (the retry back-off); the pods that already run are not affected | |

### Outage

*Measured:* @@OUTAGE@@

1. **Find out** which part is down: `kubectl -n ambit-system get pods -l app.kubernetes.io/name=ambit-hydra`; `kubectl -n ambit-system logs ambit-hydra-0 -c hydra | tail`. Hydra's readiness includes the database, so Hydra not Ready means "the database does not answer", "the password or the CA is wrong" or "the signing key set is not one key" (the log says which).
2. **Fix the database.** Ambit does not restart Hydra for you, and does not need to: its connection pool reconnects. Do not delete the Hydra pods unless a pod stays NotReady for minutes after the database is back.
3. **Check** that sign-in and refresh work again: `curl -s -o /dev/null -w '%{http_code}\n' https://<authHost>/.well-known/jwks.json` is `200`, and a platform's refresh answers 200.

### Backup

Back up: (1) **the whole database**, with your database's backup (managed backups, or `pg_dump -Fc`), at least daily and before every Ambit upgrade;
(2) **the Secret `oauth.hydra.systemSecret`** (`system`): it encrypts Hydra's keys and tokens, and without it a restored database cannot be read. Keep it where
you keep the backups, not in the same place as the database. The other Secrets (address, CA, cookie secret, identity provider's client secret) are yours already.

```
kubectl -n <database namespace> exec <database pod> -- pg_dump -Fc -U <owner role> -d hydra > hydra.dump
```

The dump goes to a file **outside** the database pod, and then to your backup storage: a dump that lives only in the pod that holds the database is lost with it.
Keep the matching `system` Secret with it (the restore needs both).

*Measured:* @@P4@@ Check a backup by restoring it somewhere once (below): a backup that was never restored is a guess.

### Restore

Restore into a **new** instance; never over the live one.

0. **Keep the matching Secret** `oauth.hydra.systemSecret` in the cluster (it is a Secret of the install; a restored database is read with it). A row count can be right and the keys
   unreadable if the `system` value is another one.
1. Create the new instance with the same TLS rules (a certificate for the name Hydra uses, the same CA), the role and the empty database `hydra`, then restore:
   `pg_restore -U <superuser> -d hydra --exit-on-error /tmp/hydra.dump` (a managed service: its "restore to a new instance").
2. Check it: the counts of `hydra_client` and `hydra_oauth2_refresh` are what you expect.
3. **Stop the old database first**, or take the login away from its role (`ALTER ROLE ... NOLOGIN`), so that no Hydra pod can still write to it while another pod already uses the new one. A rolling
   change of the address leaves for a short time pods on both: a refresh that succeeds on the old side (and rotates the token there) can fail on the next call on the new side.
   Then point Hydra at the new instance: change the Secret `oauth.database.dsnSecret` (or move the DNS name or Service), then roll Hydra's pods one at a time:
   `kubectl -n ambit-system rollout restart statefulset/ambit-hydra`. Hydra does not notice a changed Secret by itself. If the name moved and the old instance
   is stopped, the pods reconnect by themselves.
4. **What people see** (measured, P5): connections that were **not used since the backup** keep working. Connections that **refreshed after the backup** are refused with
   `invalid_grant`: the new refresh token that the platform holds is not in the backup. Those people connect the platform again (a new sign-in and consent). Clients that
   registered after the backup are missing: that platform gets `invalid_client` and registers again. A person who ended a connection after the backup does not get it back:
   the stop and the offboarding live in Kubernetes. Tell the people: "connections you made or used in the last <time since the backup> may ask you to connect again".
5. **Re-apply what the backup does not know.** Changes in Hydra after the backup are undone: an OAuth client that an admin deleted is back, and a refresh token that a platform revoked
   (`/oauth2/revoke`) works again until its agent ends. Read the History of the console for deletions of OAuth clients since the backup, and delete them again; ended connections (agents) stay ended.
6. Take a new backup.

### Fail-over

A managed service's fail-over usually keeps the DNS name and moves it to a replica. Hydra reconnects by itself: no action. *Measured on kind (a Service moved to another
instance, the old one stopped, so no pod wrote to two databases):* @@P5FAIL@@ What kind cannot show: how long your provider's fail-over takes, and how much of the last seconds the replica has (your
RPO). Measure both with checklist items 1 and 2. If the name does not move (a new address), change the Secret and roll the pods as in Restore.

### Pool sizing

Each Hydra pod keeps at most `max_conns` connections (in the address; the chart's example is 8). The database role needs room for `pods × max_conns` **plus one more run of
a pod that uses the same address**: the janitor (a Job) and a migration use `max_conns` too. For 2 pods and `max_conns=8` that is `(2 + 1) × 8 = 24`; the start check
counts `pods × max_conns + 1`, so set the role's `CONNECTION LIMIT` to `(pods + 1) × max_conns` or more, and the server's `max_connections` above the sum of all clients.
*Measured (P3):* @@P3@@ Raise `max_conns` only when Hydra's answers are slow while connections are all in use; more connections do not make a small database faster.

### Daily clean-up (the janitor)

Hydra never deletes finished rows. The CronJob **`ambit-hydra-janitor`** (`oauth.janitor.*`, 03:17 every day) runs `hydra janitor` with Hydra's own lifetimes
(the same ConfigMap), so **a token that is still valid is never deleted**. It deletes **expired access and refresh tokens**, and keeps everything younger than an
hour. One run at a time (`concurrencyPolicy: Forbid`), stopped after 30 minutes, at most 10,000 rows of each kind per run (the next day goes on). It does not touch clients.

**If you shorten `oauth.refreshTokenIdleDays`**, the janitor uses the new, shorter lifetime: a refresh token that is older than it is deleted at the next run, although the token itself still said
that it was valid for longer. Lower the value only when you mean to end those connections.

**What it does not delete** (measured on Hydra v26.2.0 with a database in which the rows were made old by hand): authorization codes, and the records of sign-ins that
were completed, **also when they are old**. A refresh token is tied to the record of its sign-in by a cascading rule of the database, so a record that is deleted ends the
connection: the janitor leaves the records of completed sign-ins alone (tested: a live connection whose record is two days old still refreshes after a run). A sign-in that is
abandoned or denied writes no row in this version. The rows that stay are small (about 1.3 KB for a code and 0.6 KB for a record, plus what Ambit's claims add) and they grow
with the number of sign-ins, not with the number of connections: at 1,000 sign-ins a day that is a few MB a day. Look at the size of `hydra_oauth2_code` and
`hydra_oauth2_flow` once a quarter. Ambit does not delete them: it does not edit Hydra's tables.

Run it by hand, and read the log:

```
kubectl -n ambit-system create job --from=cronjob/ambit-hydra-janitor janitor-now
kubectl -n ambit-system logs job/janitor-now
kubectl -n ambit-system delete job janitor-now
```

The log says `Successfully completed Janitor run on access tokens`, `... refresh tokens`, `... login-consent requests` and `... grants`. Alerts: `AmbitHydraJanitorFailed`
and `AmbitHydraJanitorStale` ([Alerts](alerts.md)). *Measured (kind):* @@JAN@@ To pause it: `oauth.janitor.enabled: false`.

### Unused clients (registered dynamically)

`ambit-auth` deletes a client that registered itself and was not used for `oauth.registration.dynamic.unusedClientDays` (30). It runs when `ambit-auth` starts and every 10
minutes; a consent and a refresh count as use. A client that an admin created in Hydra is never deleted. A platform whose client was deleted gets `invalid_client` and registers
again ([the guide](guides/oauth-mcp-client.md#4-register-a-client-rfc-7591)). Tested in CI (check 17 and the unit tests).

### Rotating the password and the CA

**The database password:** (1) change the role's password in the database; (2) change the Secret `oauth.database.dsnSecret` (a URL-safe password); (3) roll the pods one at a time:
`kubectl -n ambit-system rollout restart statefulset/ambit-hydra`. A connection that exists keeps working (a password is checked when a connection starts), so only new
connections fail between step 1 and the end of step 3. The janitor Job reads the Secret each time it runs, and starts with the same database check as Hydra's pods (a wrong address is refused with a fixed message). *Measured:* @@ROT@@
**The CA:** put the old and the new CA, one after the other, in the file of `oauth.database.caSecret` (a file with several certificates is accepted: tested), change the
server's certificate, roll the pods, then remove the old CA from the file and roll again. A rotation of the server's own certificate by a real service is not tested.

### Upgrading Hydra, and rolling back

1. Back up (above). 2. Read Hydra's release notes for schema changes. 3. Upgrade Ambit: the first pod that restarts runs `hydra migrate sql up` in its init container.
**Rolling back:** deploy the previous Ambit version **and restore the backup you took in step 1** (Restore, above). **Do not run `hydra migrate sql down` on a live database.**
Measured on v26.2.0: one step down, then up again, left the clients, and removed every login and consent record and, with them (a cascading rule of the database), all
@@MIGN@@ refresh tokens and every access token: every connection had to connect again. A rollback that way is a reconnect of everybody.

### Alerts

See [Alerts](alerts.md): `AmbitHydraNotReady`, `AmbitOAuthTokenErrors`, `AmbitHydraJanitorFailed`, `AmbitHydraJanitorStale`, and, with your own expressions,
`AmbitOAuthDatabaseBackupOld` and `AmbitOAuthDatabaseConnectionsHigh`.

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
