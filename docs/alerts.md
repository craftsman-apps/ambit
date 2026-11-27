# Alerts

With `alerts.enabled: true` the chart installs a `PrometheusRule` and a `PodMonitor` for the Prometheus
Operator. The rules read kube-state-metrics and cAdvisor (kube-prometheus-stack installs both), the
collector's own metrics, the guard's `ambit_guard_compress_*` metrics, and the console's `ambit_console_audit_send_failures_total`. Set `alerts.labels` to the labels your Prometheus selects rules and monitors by:

```yaml
alerts:
  enabled: true
  labels: { release: kube-prometheus-stack }
```

The alerts of agents (`AmbitAgentStopNotConfirmed`, `AmbitStopPolicyDegraded`) and of OAuth sign-in (`AmbitHydra*`, `AmbitOAuth*`, and the values `alerts.oauth.*`) are new in 0.6.0 and experimental: their names and values may change in 0.7.0, with an upgrade note.

| Alert | Severity | Fires when | First step |
|---|---|---|---|
| AmbitContainerRestarting | warning | a container restarted 2 or more times in 30 minutes | `kubectl -n <ns> describe pod <pod>`: read Last State |
| AmbitContainerOOMKilled | warning | a container was killed at its memory limit and restarted in the last 30 minutes | raise its limit in the chart values, or find what grew (below) |
| AmbitMemoryNearLimit | warning | a container used over 90% of its memory limit for 15 minutes | as above, before the kill |
| AmbitWorkloadUnavailable | warning | a Deployment or StatefulSet had fewer ready pods than it should for 10 minutes | `kubectl -n <ns> get events --sort-by=.lastTimestamp` |
| AmbitGuardCompressionFailing | warning | result compression of a server had an error or ran out of time in the last 30 minutes (only with `protection.enabled`). No call failed: the guard passed the original text | set `resultCompression.mode: off` on the Server if it continues, and read `kubectl -n <ns> logs deploy/ambit-guard` |
| AmbitGuardRecordsDropped | warning | the guard dropped compression records in the last 30 minutes (only with `protection.enabled` and `evidence.enabled`). No call failed. The Savings figures are lower than the real ones | `send_failed`: the collector was not reachable, read `kubectl -n <ns> logs deploy/ambit-otel-collector`. `queue_full`: more records than the guard can send. `no_groups`: the gateway sent no groups for the call |
| AmbitBudgetStoreDown | critical | the ratelimit service or Valkey had no ready pod for 1 minute (only with `limits.enabled`) | with `FailClosed` the gateway refuses LLM and MCP requests; with `FailOpen` requests pass uncounted |
| AmbitAuditLogsDropped | critical | audit records failed to reach ClickHouse, or the collector refused them, in the last 10 minutes (only with `evidence.enabled`) | check ClickHouse first |
| AmbitConsoleAuditSendFailed | warning | the console could not send records of MCP app requests, approvals, denials or refused attempts to the collector in the last 30 minutes (only with `evidence.enabled`). The requests and decisions happened; the Audit page and `otel_logs` miss these records | the `ServerRequest` objects still hold the requests and decisions: `kubectl -n <ns> get serverrequests`. Read why: `kubectl -n <ns> logs deploy/ambit-console`, then check the collector |
| AmbitControllerRecordsDropped | warning | the controller dropped access snapshot records in the last 30 minutes (only with `evidence.enabled`). The rules were applied; an access review export for this time has a gap until the next change or the daily record at 00:05 UTC | `send_failed`: the collector was not reachable, read `kubectl -n <ns> logs deploy/ambit-otel-collector`. `queue_full`: more records than the controller can send. `too_large`: the AccessRules together are larger than 1 MiB, read `kubectl -n <ns> logs deploy/ambit-controller` |
| AmbitAgentStopNotConfirmed | critical | an agent stop was requested more than 60 seconds ago and the named Gateway has not accepted the stop policy. The agent's keys are deleted, but the Gateway may still serve a token or a key it already knows | `kubectl -n <ns> get agentgatewaypolicy \| grep ambit-agent-stops`; `kubectl -n <ns> logs deploy/ambit-controller`; check the agentgateway controller. The agent stays in state `stopping` until the Gateway confirms |
| AmbitStopPolicyDegraded | critical | the stop list could not be turned into gateway policies (more than 1,000 entries, or an unreadable list). The last stop policies stay in force; new stops are not applied | `kubectl -n <ns> get configmap ambit-agent-stops-ack -o yaml` (field `degraded`) |
| AmbitHydraNotReady | critical | no Hydra pod was ready for 2 minutes (only with `oauth.enabled`). Nobody can connect a platform or refresh a connection; calls with an access token that was already issued work for up to 15 minutes | the database first (Hydra is not ready when it cannot reach it): `kubectl -n <ns> logs ambit-hydra-0 -c hydra`, `-c check-database`; then [Outage](oauth.md#outage) |
| AmbitOAuthTokenErrors | critical | more than 5% of the requests to Hydra's public endpoints (`/oauth2/token`, `/oauth2/revoke`, the key list) answered 5xx for 5 minutes, with at least one request every 50 seconds (only with `oauth.enabled`). Read from the Gateway's own request counter (`agentgateway_requests_total`, route `ambit-auth-hydra`), through the PodMonitor `ambit-public-gateway` | Hydra and its database, as above |
| AmbitHydraJanitorFailed | warning | a run of the clean-up job `ambit-hydra-janitor` failed and did not succeed on a retry (only with `oauth.enabled`). Expired tokens stay until the next run | `kubectl -n <ns> logs job/<job name>`: the usual cause is the database address or password |
| AmbitHydraJanitorStale | warning | the clean-up has not succeeded for `alerts.oauth.janitorMaxAgeHours` (6) hours, or never did although its CronJob is older than that (only with `oauth.enabled`; raise the value if you change `oauth.janitor.schedule`) | `kubectl -n <ns> get cronjob ambit-hydra-janitor` |
| AmbitOAuthDatabaseBackupOld | warning | **your** expression says that the newest backup of Hydra's database is older than `alerts.oauth.backupMaxAgeHours` (26) (only when `alerts.oauth.backupAgeSecondsExpr` is set) | your backup system; [Backup](oauth.md#backup) |
| AmbitOAuthDatabaseConnectionsHigh | warning | **your** expression says that more than `alerts.oauth.connectionsMaxRatio` (0.8) of the database's connections are in use (only when `alerts.oauth.connectionsUsedRatioExpr` is set) | [Pool sizing](oauth.md#pool-sizing) |

## The database alerts (OAuth, experimental)

Ambit cannot see inside your database, so the last two alerts above read **your** metrics. Put a PromQL expression in the values, and the alert
exists; leave it empty and there is none. The names depend on your database and its exporter. These are examples, **not tested by Ambit**:

```yaml
alerts:
  enabled: true
  oauth:
    # seconds since the newest backup
    backupAgeSecondsExpr: 'time() - max(cnpg_collector_last_available_backup_timestamp)'     # CloudNativePG
    backupMaxAgeHours: 26
    # the share of the database's connections in use, from 0 to 1
    connectionsUsedRatioExpr: 'sum(pg_stat_activity_count) / max(pg_settings_max_connections)'  # postgres_exporter
    connectionsMaxRatio: 0.8
```

For a managed service, use the metric that the service publishes: the age of its newest automatic snapshot or backup, and its
connection count against its limit (Amazon CloudWatch, Google Cloud Monitoring and Azure Monitor each have both). The rule fires when the
expression stays above its limit for 10 minutes. Test it once: set the limit low and see the alert fire.

## Finding what grew

Memory per container: `kubectl -n <ns> top pods --containers`. Known causes:

- **Collector:** it holds failed batches for retry while ClickHouse refuses writes, up to 64 MiB, and
  refuses new records above 75% of its limit. Lost records raise AmbitAuditLogsDropped; the cause is
  usually ClickHouse.
- **ClickHouse:** `kubectl -n <ns> exec ambit-clickhouse-0 -- clickhouse-client -q "SELECT metric, value
  FROM system.metrics WHERE metric = 'GlobalThread'"`. The chart's settings keep it near 90 threads; many
  more means its thread pools were changed.
- **Gateway:** about 7 KiB per open MCP session, dropped after 30 minutes without use. With more than one gateway pod, a
  session that a client closes (HTTP DELETE) is dropped only in the pod that receives the DELETE; a pod that served the
  session earlier keeps its copy until it has not used it for 30 minutes. So under steady MCP traffic the memory of a gateway
  pod rises for the first 30 minutes and then levels off; it is not a leak. Measured on kind with three pods and about 36 new
  sessions a minute, each closed: 12 MiB at the start, 19 MiB after 35 minutes, then between 17 and 20 MiB for the next 75
  minutes (issue #383). Size `gateway.resources.limits.memory` for the level, not for the first minutes. The level is the
  pod's own memory plus 30 minutes of the sessions that the pod served; it grows with the number of new sessions a minute.
  Memory that keeps rising after more than 40 minutes of constant traffic needs another explanation.