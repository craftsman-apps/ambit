# Upgrading

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
