# Alerts

With `alerts.enabled: true` the chart installs a `PrometheusRule` and a `PodMonitor` for the Prometheus
Operator. The rules read kube-state-metrics and cAdvisor (kube-prometheus-stack installs both), the
collector's own metrics, the guard's `ambit_guard_compress_*` metrics, and the console's `ambit_console_audit_send_failures_total`. Set `alerts.labels` to the labels your Prometheus selects rules and monitors by:

```yaml
alerts:
  enabled: true
  labels: { release: kube-prometheus-stack }
```

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