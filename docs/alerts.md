# Alerts

With `alerts.enabled: true` the chart installs a `PrometheusRule` and a `PodMonitor` for the Prometheus
Operator. The rules read kube-state-metrics and cAdvisor (kube-prometheus-stack installs both) and the
collector's own metrics. Set `alerts.labels` to the labels your Prometheus selects rules and monitors by:

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
| AmbitBudgetStoreDown | critical | the ratelimit service or Valkey had no ready pod for 1 minute (only with `limits.enabled`) | with `FailClosed` the gateway refuses LLM and MCP requests; with `FailOpen` requests pass uncounted |
| AmbitAuditLogsDropped | critical | audit records failed to reach ClickHouse, or the collector refused them, in the last 10 minutes (only with `evidence.enabled`) | check ClickHouse first |

## Finding what grew

Memory per container: `kubectl -n <ns> top pods --containers`. Known causes:

- **Collector:** it holds failed batches for retry while ClickHouse refuses writes, up to 64 MiB, and
  refuses new records above 75% of its limit. Lost records raise AmbitAuditLogsDropped; the cause is
  usually ClickHouse.
- **ClickHouse:** `kubectl -n <ns> exec ambit-clickhouse-0 -- clickhouse-client -q "SELECT metric, value
  FROM system.metrics WHERE metric = 'GlobalThread'"`. The chart's settings keep it near 90 threads; many
  more means its thread pools were changed.
- **Gateway:** about 7 KiB per open MCP session, dropped after 30 idle minutes.
