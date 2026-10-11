# Personal API keys

A personal API key lets one person use Ambit from a tool that cannot refresh a sign-in token: Claude Code,
Cursor, an OpenAI or Anthropic SDK, or an MCP client. A request with a key is treated like a request with
that person's token: the same access rules, budgets, audit records and offboarding.

## What a key is, and is not

- A key is a **personal credential**. It carries the access of its owner, never more. It carries the groups
  the owner had at their last console sign-in.
- A key is **not a service account**. It belongs to a person and stops when that person is offboarded or has
  not signed in to the console for a while.
- The key is **shown once**, when you create it. Ambit stores only a SHA-256 hash and a short display prefix
  (`ambit_` and 4 characters). If you lose a key, revoke it and create a new one.
- A key starts with `ambit_`, so secret scanners can recognise a leaked key.

## Create and use a key

1. Sign in to the console and open **Keys**.
2. Give the key a name, choose the scope and the end date, and create it.
3. Copy the key and the snippet for your tool. The console fills in your company's addresses.

Clients can send the key as `Authorization: Bearer <key>` or as `x-api-key: <key>`. Both work on both
gateways. Clients send the **provider's model ID** (for example `google/gemma-4-31b-it:free`), not the name of
the Model object.

The console writes these snippets for you. `<llm>` is the LLM address (for example `https://llm.example.com`)
and `<mcp>` is the MCP address (for example `https://mcp.example.com/mcp`).

**Claude Code**, in `settings.json`:

```json
{"env": {"ANTHROPIC_BASE_URL": "<llm>", "ANTHROPIC_AUTH_TOKEN": "ambit_..."}}
```

**Cursor**, in the model settings:

```text
OpenAI API Key: ambit_...
Override OpenAI Base URL: <llm>/v1
```

Cursor sends requests from its own servers, not from your computer. Ambit's LLM address must therefore be
reachable from the internet, over HTTPS.

**OpenAI SDK and other OpenAI-compatible tools**:

```bash
export OPENAI_BASE_URL=<llm>/v1
export OPENAI_API_KEY=ambit_...
```

**Anthropic SDK**:

```bash
export ANTHROPIC_BASE_URL=<llm>
export ANTHROPIC_API_KEY=ambit_...
```

**MCP, with Claude Code**:

```bash
claude mcp add --transport http ambit <mcp> --header "Authorization: Bearer ambit_..."
```

A key with scope `LLM` gets the first four snippets. A key with scope `MCP` gets the last one.

## Scope, end date and limits

| Item | Rule |
|---|---|
| Scope | `LLM`, `MCP`, or both. A key is refused on a gateway outside its scope. |
| End date | 30, 90 or 365 days, chosen at creation. Default 90. Not longer than `apiKeys.maxDays`. |
| Inactivity | A key also stops when its owner has not signed in to the console for `apiKeys.inactivityDays` (default 30). Each console sign-in restarts this count and copies the owner's current groups onto all their keys. |
| Keys per person | At most 20 active keys. |
| Creations per person | At most 10 per hour. A console restart resets the count. |
| Name | 1 to 100 characters. |

A key that has stopped is deleted and is never revived. The person creates a new one. Lowering a setting
shortens the life of existing keys; raising it never extends them.

Seven days before a key stops, the console shows a banner on every page to its owner. Ambit sends no email.

## What admins see

Under **Settings → Keys**, admins see:

- every key with its owner, scope, end date and last use (last use needs the audit trail, otherwise it shows
  as unknown);
- a button to **revoke** any key;
- the **History** of key changes: who created or revoked which key, and when. It never shows a key or its hash;
- the three settings, **read-only**. They are Helm values.

Everyone who can sign in can create keys. A person sees and revokes only their own keys.

## Settings

Set these in the Helm values. A change restarts the console, which applies the new values to all existing keys
before it serves requests.

| Value | Default | Meaning |
|---|---|---|
| `apiKeys.enabled` | `true` | `false`: the gateways accept tokens only, and every key is refused at once, even if the console is down. Keys are kept. Creating a key answers 409. Revoking still works. Switching back on restores keys that have not stopped. |
| `apiKeys.inactivityDays` | `30` | Days without a console sign-in before a key stops (1 to 365). |
| `apiKeys.maxDays` | `365` | Longest end date a person may choose: 30, 90 or 365. |
| `llm.jwt.requireEmail` | `true` | Refuse tokens without a usable `email` claim at the LLM gateway. |
| `mcp.jwt.requireEmail` | `true` | The same, at the MCP gateway. |

With `requireEmail: false`, a token without an email is identified by its subject (`sub`). Offboarding cannot
block such a token before it expires. See [upgrading.md](upgrading.md).

## Offboarding

When the identity provider deactivates or deletes a person (SCIM), Ambit:

1. adds their email to the gateway block list, so their keys and tokens are refused at once;
2. deletes all their keys, and records how many.

Reactivating the person removes the block. Their old keys stay deleted. They create new ones.

While keys are still left to delete, the SCIM endpoint answers **503** with `Retry-After: 30`. Identity
providers retry on 503, so the change completes later. Offboarding of SaaS grants is described in
[offboarding.md](offboarding.md).

## How the gateway checks a key

Every request to the LLM or MCP gateway must pass these checks. A request that fails one gets **403**.

1. The request has a credential. If it carries `x-api-key`, or `Authorization: Bearer ambit_...`, it presents a
   key, and **only the key counts**. A token sent alongside is ignored.
2. The key's hash matches a stored key, and the key has the scope of this gateway.
3. The key has not passed its end (`until`). `until` is the earlier of the chosen end date and the last
   console sign-in plus `apiKeys.inactivityDays`.
4. The owner's email is not on the offboarding block list.
5. The owner's groups (from the key) allow the model, the MCP tool and the budget, as for a token.

A request without a key must carry a valid token, with a usable `email` claim unless `requireEmail` is
`false`.

Before the request goes on, the gateway removes the `Authorization` and `x-api-key` headers. The provider or
MCP server never receives the key. Ambit then adds the provider's own credential. Audit records show the key
ID, never the key.

## Operations

- **One console replica.** The chart refuses `console.replicas` above 1. The console Deployment uses
  `RollingUpdate` with `maxSurge: 0`: the old pod stops before the new one starts.
- **One writer.** The Lease `ambit-key-writer` names the console pod that may write keys. After a console
  restart, key writes are refused for about 15 seconds (the answer is 503) while the old Lease expires.
- **Admission policies.** The chart installs two `ValidatingAdmissionPolicy` objects. `ambit-keys-writer` lets
  only the console's service account write key Secrets, owner records and key events. `ambit-key-lease` lets
  only the console change or delete the Lease. The namespace controller and the garbage collector may
  delete these objects, so a namespace can always be removed. This needs `kube-controller-manager` started
  with `--use-service-account-credentials`. This is true on kind, kubeadm, EKS, GKE and AKS.
- **Trust.** A component that can create pods in the Ambit namespace can run as the console's service account.
  Such components (the ToolHive operator, the agentgateway controller) are trusted like namespace admins.
- **Hourly repair.** Every hour, and at each start, the console re-checks all keys. It deletes keys that have
  stopped, keys of offboarded people and unreadable keys, and re-applies the settings and each owner's groups.
- **Accepted risk.** The Lease does not fence a stale console pod perfectly. During a takeover of a few
  seconds, one stale write could be accepted. The effects are small: offboarding does not depend on key
  deletion (the block list refuses the email anyway); an extra key still carries only its owner's access
  and ends at its `until`; a key may keep older groups until the next repair, at most one hour.
- **Console down.** Keys keep working until their `until`. Creating, refreshing and revoking keys, and SCIM
  offboarding, wait until the console is back.
- **Never enable agentgateway CEL debug tracing.** It can print keys in clear text. The chart never turns it
  on.
- **Kubernetes 1.30 or later.** The chart refuses to render on older clusters.

## Measured

Kubernetes v1.37.0 (kind), agentgateway v1.6.0, 2026-10-11. One run, not a promise.

| Measure | Result |
|---|---|
| Memory, 0 to 1,000 keys | control plane 65 to 87 MiB, proxy 12 to 23 MiB |
| All 1,000 keys accepted | 8.4 s after loading (includes one sweep of 1,000 requests) |
| Revocation to first refusal (20 runs) | median 352 ms, maximum 414 ms |
| Console restart, repair of 20 keys | 24 s (about 15 s of it is the Lease wait) |
| Proxy pod restart to first success with a key | 3.4 s |
