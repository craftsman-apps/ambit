# Upgrading

For what each release changes, read the [release notes](https://ambitproject.dev/releases).

`ambit upgrade -f values.yaml` does the upgrade in the right order (the dependency CRDs, `ambit-crds`, `ambit`) and
lists the sections below that apply to your upgrade. It writes ordinary Helm releases, so `ambit upgrade` and
`helm upgrade` are interchangeable, and `helm rollback` works on both. A release left in `pending-install` or
`pending-upgrade` (a laptop that stopped in the middle of an upgrade) is refused; see
[ambit upgrade](cli.md#ambit-upgrade) for `--recover-pending`.

## To the release with agents (0.6.0)

This release adds [agents](agents.md): records, keys with a scope and source ranges, and audit fields. **The agent API, the
agent fields of a key and the audit columns of an agent are experimental in 0.6.x**: they may change in 0.7.0, with a note on
this page, and never silently inside 0.6.x. What may be relied on in 0.6.0 is the `ambit_` key format, the seven audit
attributes (`ambit.agent`, `ambit.agent_mode`, `ambit.agent_owner_at_issue`, `ambit.platform_declared`, `ambit.grant_id`,
`ambit.token_issuer`, `ambit.ingress`) and the eight columns appended to `calls.csv`; exports only ever append columns.

- New Helm values `agents.enabled` (default `true`), `agents.maxTotal`, `agents.maxPerPerson`, `agents.keyMaxDays` and `agents.tokenIssuer` (reserved for OAuth; leave it empty). `agents.enabled: false` refuses new agents and agent keys; existing keys keep working until they end.
- The MCP access policy `ambit-mcp-access` gets one more constant clause, so **the rules version shown in Audit changes once** after the upgrade. The access of people does not change.
- The access log gets seven more attributes. A custom access-log policy of your own is not changed.
- With `--reuse-values` from an earlier release the new values are missing and the defaults apply.

## To the release with service accounts (0.5.0)

- **Compression records name the caller.** With `evidence.enabled` and a Server with `resultCompression`, the guard's
  record of each compressed text now also holds the caller's email and key ID, the tool and a call ID
  (`ambit.call_id`, also on the gateway's record of the call). New Helm value `protection.compressionAudit.caller`
  (default `true`); `false` keeps the person out of these records. Records written before the upgrade have none of
  these fields.
- **Tool pinning covers more of each tool.** A tool's fingerprint now also covers its title, its output schema and the
  standard annotations (`title`, `readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`); before, it
  covered the name, description and input schema only. Approvals made before the upgrade carry over: the next time
  Ambit reads a server's tools (when the controller starts, then every 10 minutes), it moves each approval to the new
  fingerprint if the tool's name, description and input schema are still the approved ones. It takes over the title,
  annotations and output schema that the server shows at that moment without review, because the older release did
  not record them. A tool that changed, and a new tool, stays hidden and refused until an admin approves it on the
  Servers page, as before. Until a server's approvals have moved, the guard also accepts the older fingerprint for
  them. While the `ambit-guard` pods restart during the upgrade, an older guard pod may hide the moved tools from `tools/list` for a moment. The controller logs `approvals made before 0.5.0 moved to the new tool fingerprint` with the server and the
  number of tools. `Server` status gains `tools[].legacyFingerprint`, used only for this step.
- **A shared server whose tools were never read is now closed.** A `Server` without `connection` and with no approved
  tools (its tools could not be read yet) used to pass all its tools without pinning. Now the guard hides and refuses
  them until the controller reads them; such a server has the condition `ToolsPinned=False`, reason `NotRead`. A
  `Server` with `connection` whose tools no admin's account has read stays usable, and now shows `ToolsPinned=False`,
  reason `NotPinned`, and "Tool pinning off" on the Servers page.
- **Argument limits can set `min`.** The `AccessRule` CRD gains `arguments[].min`. Nothing changes for existing rules:
  a limit with only `max` still allows 0 and negative numbers.
- **After a downgrade below 0.5.0, approve the tools again, and remove `min`.** The older guard does not know the new
  fingerprints, so it hides and refuses every tool approved under 0.5.0 until an admin approves the server's tools
  again on the Servers page. The older guard does not enforce `min`.
- **After a downgrade below 0.5.0, remove the email from the compression records.** The older Audit page lists every
  record that has the person's email, so it shows each compression record written by 0.5.0 as an empty extra row until
  the record ages out (`evidence.retention`). The older guard writes no email, so only the records written before the
  downgrade need this statement, once (the Savings summaries do not change):

  ```sql
  ALTER TABLE otel.otel_logs UPDATE LogAttributes = mapFilter((k, v) -> k NOT IN ('jwt.email', 'ambit.key_id'), LogAttributes)
  WHERE LogAttributes['ambit.kind'] = 'compression'
  ```

- **`allSignedIn: true` no longer grants service accounts.** It means every person with a valid token or personal key.
  No service account exists before this release, so nothing changes at the upgrade. When you add a service account,
  name its group or its identity in a rule: a rule that relies on `allSignedIn` will not give it access.
- **Tokens with an `@svc.ambit.internal` email are refused** by the gateways and the console sign-in, and SCIM
  refuses a user with such an email. Such an address
  cannot be a real mailbox (`.internal` is reserved for private use), so no person should have one.
- **New Helm values** `serviceAccounts.enabled` (default `true`), `serviceAccounts.maxDays` (default `90`) and
  `serviceAccounts.exposureDays` (default `7`). No admission policy changes (the CRD changes are above). The console records the new
  settings once in the key History at its first start.
- **More new Helm values, all optional:** `llm.promptGuard` (the prompt guard on the model path, `enabled: false` by
  default, so nothing changes until you turn it on), `evidence.exports.enabled` (default `true`),
  `evidence.exports.maxRows` (default `5000000`) and `evidence.exports.maxDays` (default `366`). Access reviews
  (Settings → Evidence) cover dates from the upgrade on, and only with `evidence.enabled`.
- **Some answers change.** A model request with no key or an empty token gets 403 with the message "authorization
  failed: no Ambit key or token was sent", in Anthropic's error format for `/v1/messages` and OpenAI's for the other paths. A prompt guard refusal is a JSON error (code
  `prompt_guard`), no longer plain text. In audit-only mode, the summary's "refused" count no longer includes prompt
  guard refusals.
- **The MCP guard masks more and withholds more.** The default masks find more written forms of card numbers,
  resident registration numbers (also foreign residents'), US Social Security numbers and emails, and a tool result that is not a JSON object is withheld instead of passed unchecked.
  Test a tool whose results contain long numbers before you upgrade production. The audit log no longer stores
  `mcp.error.message`; `mcp.error.code` stays.
- **Delete service accounts before a downgrade** below 0.5.0. An older console reads a service account as a person who
  never signed in, so its hourly repair deletes the account's keys within an hour (fail closed). Until that repair
  runs, the older gateways accept those keys as ordinary keys, also when `serviceAccounts.enabled` was `false`, and
  no longer refuse tokens with the reserved email.

## To the release with agentgateway v1.7.0

- **Apply the new dependency CRDs first** (`crds.yaml` of the release, see [Install](install.md#upgrade-and-uninstall)).
  The agentgateway CRDs only gain fields, so objects you have stay valid.
- **The gateway pods are replaced one at a time.** On a local kind cluster with two gateway replicas, 177 MCP
  requests sent during the upgrade all succeeded, and open MCP sessions, Budget counters and tool pins stayed.
- **Some answers change.** A tool the person may not use is answered `unknown resource` (before: `Unknown tool:
  <name>`). Gateway errors no longer include internal details. Every streamed chat request ends with a usage chunk,
  also when the client turned it off. `/v1/messages` to OpenAI's own API is sent as a Responses request. See the
  [release notes](https://ambitproject.dev/releases).
- **Embeddings count against daily request limits after the answer**, like other requests: a refused embeddings
  request (403 or 429) no longer counts. The controller deletes the policy `ambit-llm-usage`, which the gateway made
  unnecessary.
- **Audit rows written before the upgrade keep their caller-less model refusals.** Only refusals after the upgrade
  show the caller.

## To the release with access rules that name people

- **Upgrade the controller and the ambit-guard together** (the chart does this). An older guard does not know `users`
  and does not apply argument limits for people listed there.
- **`users` match verified emails only.** A JWT's email counts only when the token says `email_verified: true`
  (`accessRules.usersRequireVerifiedEmail`, default true). If your identity provider does not send `email_verified`,
  set it to false only if users cannot change their own email. `ambit check --production` has a new check, P12, that
  warns when it is false.

## To the release with audit-only mode

- **The controller now writes the access-log policy.** The AgentgatewayPolicy `ambit-access-log` has the same name as
  before, but the chart no longer renders it. The chart renders the ConfigMap `ambit-access-log-base` instead, and
  `ambit-controller` writes the policy from it (it adds `ambit.accessMode` and `ambit.rulesVersion`, and in audit-only
  mode the "would be refused" fields). agentgateway uses only the first access-log policy of a Gateway, so there must
  be exactly one. The controller creates the policy before it reports Ready, and creates it again whenever it is
  deleted. It has the label `app.kubernetes.io/managed-by: ambit-controller`.
- **There is a short gap with no access-log policy during the upgrade.** Helm deletes the policy that the old chart
  made, and the controller creates its own. Requests served in the gap have no audit record. The mode in that time is
  enforce, so no request is allowed without a record that it should have. On a local kind cluster with one gateway pod
  the gap was 0.4 to 0.6 seconds in five runs; on your cluster it can be longer. `ambit check --production --deep`
  (P4 and P11) fails when it runs inside the gap. The Audit page warns when no record arrived for an hour.
- **`ambit check --production` has a new check, P11,** for audit-only mode. Its line for records with a missing or
  failed mark runs one query in the ClickHouse pod, which needs `create` on `pods/exec` (see the
  [CLI reference](cli.md#permissions)).
- **`ambit-access-mode` stays after `helm uninstall`.** The controller writes this ConfigMap (the published mode and
  rules version). It does no harm. Delete it with `kubectl delete configmap -n ambit-system ambit-access-mode` when you
  remove Ambit.
- **A rules version is new.** The first pass after the upgrade computes the version of your current rules once and
  publishes it after agentgateway has applied it.

### Rolling back to an earlier chart

The earlier chart contains an `ambit-access-log` object. Helm cannot take over the object that the controller wrote
(Helm 3 refuses with "invalid ownership metadata"; Helm 4 stops with a field conflict with the manager
`ambit-controller`), and the running controller writes the policy again within a second of a delete. Run these steps
in this order:

```sh
kubectl scale deployment -n ambit-system ambit-controller --replicas=0
kubectl wait --for=delete pod -n ambit-system -l app.kubernetes.io/name=ambit-controller --timeout=120s
kubectl delete agentgatewaypolicy -n ambit-system ambit-access-log
helm rollback ambit <revision> -n ambit-system
```

The rollback brings the old chart's controller back. The wait in the second command matters: a controller pod that is
still stopping writes the policy again, and the rollback fails the same way.

- **If the cluster is in audit-only mode, switch to `enforce` first, and wait until the switch is complete** (the
  banner has gone, and `kubectl get configmap -n ambit-system ambit-access-mode -o jsonpath='{.data.mode}'` prints
  `enforce`). The old chart does not know `access.mode`, and its controller sets every authorization to enforcing
  without a warning to your team.
- **Do not force a rollback that Helm 4 stopped with the field conflict.** Helm has already rolled back some objects at
  that point, and the failed revision is the latest. Run the three steps above again, and then `helm rollback` again.
- During a rollback there is again a gap without an access-log policy, from the delete until Helm creates the old
  policy. It lasts seconds.


## To the release with personal API keys

- **Tokens need an email.** Gateway JWTs without a usable `email` claim are refused from this release. To
  accept them again, set `llm.jwt.requireEmail=false` and `mcp.jwt.requireEmail=false`. Offboarding then
  cannot block such tokens before they expire.
- **A call without a token gets 403, not 401**, while personal keys are on (the default). With
  `apiKeys.enabled=false` the gateways check tokens strictly again. Update scripts that test for 401.
- **Kubernetes 1.30 or later is required.** The chart refuses to render on older clusters.
- **The console restarts one pod at a time.** The console Deployment now uses `RollingUpdate` with
  `maxSurge: 0`. The old console stops before the new one starts, so the console is briefly unavailable
  during an upgrade. LLM requests, MCP requests and key checks keep working during a console upgrade, because the gateways read keys from Secrets. Requests that need a Connection token (SaaS access through the console's token broker) fail until the new console is ready.
- **Emails are lower-case in budgets and audit records.** The gateways now identify a person by their email
  with `A`-`Z` lower-cased. Per-person budget counters for an email with capitals start again from zero. Audit
  records from this release show the lower-case email: search for it in lower case. Older records keep the
  email as it was sent.
- **Sessions from before the upgrade cannot create keys.** People see "Sign in again to create keys" until
  they sign out and sign in again.

See [api-keys.md](api-keys.md) for the new feature.
