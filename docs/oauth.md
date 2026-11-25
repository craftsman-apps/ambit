# OAuth sign-in for hosted MCP clients

> **Experimental in 0.6.x.** The values (`oauth.*`, `frontDoor.authHost`), the public endpoints on the sign-in host, the
> objects named here and the token claims may change in 0.7.0, with an upgrade note. They never change silently inside
> 0.6.x. Ambit claims **standards-based OAuth for MCP clients**. It does not claim support for a named platform: no
> platform has been tested with a live account.

Some hosted AI platforms cannot send a fixed API key. They ask the person to sign in, then call Ambit with a short-lived
token. This page is about the part of Ambit that makes that possible: **Ory Hydra**, which issues the tokens, and the
PostgreSQL database it needs. You run the database.

> **This change contains Hydra and its database prerequisite only.** The sign-in pages (`ambit-auth`), the check of
> tokens on the public MCP route and the list of connected platforms follow in the next changes of this release series.
> Until then `oauth.enabled: true` starts Hydra and serves its token, revoke and key endpoints, and nobody can sign in.

It is **off by default** (`oauth.enabled: false`).

## Before you turn it on

The chart refuses `oauth.enabled: true` unless each of these is set, and the message names the value.

| Needed | Why |
|---|---|
| `frontDoor.enabled: true` and its requirements ([The public front door](front-door.md)) | OAuth is served on the public Gateway. |
| `frontDoor.authHost`, for example `auth.example.com` | The **sign-in host**: a second name on the same public Gateway. It must differ from `frontDoor.host`, `console.host`, `llm.host` and `mcp.host`. Its DNS name points at the same load balancer. |
| A certificate for **both** names in `frontDoor.tls.certificateSecret` | One Secret serves both listeners. |
| A PostgreSQL database that you run (below) | Hydra keeps clients, consents and refresh tokens in it. |
| `oauth.database.dsnSecret`, `oauth.database.caSecret` | The address of the database and the CA that signed its certificate. |
| `oauth.hydra.systemSecret` | A random string that encrypts Hydra's keys and tokens in the database. |

Look at the routes first, as for the [front door](front-door.md#before-you-turn-it-on): the admission policy checks routes when
they are created or changed, so a route that names `ambit-public` **before** OAuth is turned on is not checked. No route
except the ones this chart writes may name it:

```
kubectl -n ambit-system get httproutes -o custom-columns=NAME:.metadata.name,GATEWAYS:.spec.parentRefs[*].name
```

Create the three Secrets first. Nothing secret is ever written into the chart's values or objects.

```
kubectl -n ambit-system create secret generic ambit-oauth-db \
  --from-literal=dsn='postgres://hydra:PASSWORD@db.example.com:5432/hydra?sslmode=verify-full&sslrootcert=/ca/ca.crt&max_conns=8&max_idle_conns=4'
kubectl -n ambit-system create secret generic ambit-oauth-db-ca --from-file=ca.crt=./database-ca.pem
kubectl -n ambit-system create secret generic ambit-oauth-hydra --from-literal=system="$(openssl rand -hex 32)"
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
```

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

The address may carry only these parameters: `sslmode`, `sslrootcert`, `max_conns`, `max_idle_conns`, `max_conn_lifetime`, `max_idle_conn_time`, `connect_timeout` and `application_name`. Any other (a `host`, `options`, a startup setting) is refused, and its name is not printed. When the connection fails, the message holds a fixed text and, for a refusal by the server, the SQLSTATE code; it never holds the server's own words or the address.

The check is the init container `check-database` of each Hydra pod (`ambit-auth check-database`). It changes nothing in
the database and never prints the address or the password. If it fails, the pod does not start and its log names the fix:

```
kubectl -n ambit-system logs ambit-hydra-1 -c check-database
```

### What was tested, exactly

| Tested on kind in CI | What it shows |
|---|---|
| PostgreSQL 17 and 15, TLS only (a `hostnossl` line that rejects plain connections) | Hydra starts, migrates and serves on both. |
| The start check, 16 cases | The refusals above, a wrong CA, a name that is not in the certificate, too few connections and a wrong password; no log repeats a password. |
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
the name, the exact paths, the backend, the listener, and the exact header filter of each route. A route may not rewrite a path, mirror a
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

## Public API (experimental)

- Helm values `oauth.*` and `frontDoor.authHost` (all off or empty by default).
- The objects `StatefulSet`, `Services`, `ConfigMap`, `ServiceAccount`, `PodDisruptionBudget` and `NetworkPolicy`
  `ambit-hydra*`, `HTTPRoute ambit-auth-hydra`, and the Gateway listener `https-auth`.
- The public endpoints on `frontDoor.authHost`: `POST /oauth2/token`, `POST /oauth2/revoke`, `GET /.well-known/jwks.json`.
- The command `ambit-auth` (`check-database`, `hydra-keys`) in the Ambit image.
