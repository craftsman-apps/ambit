#!/usr/bin/env bash
# Feature B acceptance (the Savings page):
#   1 a compressed tool result writes one guard record to otel_logs: server, type, sizes and the caller's groups,
#     no email, no content; a result that is not compressed writes none
#   2 the console created the summary tables with the retention from the chart; the record reaches the summary;
#     GET /api/savings is admin only and shows the call, with no email in the answer
#   3 the summary views on this ClickHouse (the pinned image): equal to the Usage query, all-groups row, router rows,
#     guard records do not change the Audit answer (go test, scratch database, own test user)
#   4 only with SAVINGS_BENCH=1: the 30-day query-cost check (go test, numbers from system.query_log)
# Run as one locked unit (CLAUDE.md): lockf /tmp/ambit-kind.lock sh -c 'make image install && test/stepsavings.sh'
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
VALUES=(-f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml)
cleanup() {
  kubectl -n "$NS" exec ambit-clickhouse-0 -- rm -f /tmp/console.test /tmp/otel_logs.sql >/dev/null 2>&1 || true
  kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  # Evidence was off before this step (SAVINGS_KEEP=1 keeps it on, for repeated local runs).
  [[ "${SAVINGS_KEEP:-}" == 1 ]] || command helm upgrade ambit charts/ambit -n "$NS" "${VALUES[@]}" --wait --timeout 5m >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
raw() { mcp_send "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}"; }
set_mode() { kubectl -n "$NS" patch "servers.$G" crm --type merge -p "{\"spec\":{\"resultCompression\":{\"mode\":\"$1\"}}}" >/dev/null; }

ensure_llm_setup "$NS"
helm upgrade ambit charts/ambit -n "$NS" "${VALUES[@]}" --set evidence.enabled=true --wait --timeout 8m >/dev/null || fail "0 helm upgrade with evidence.enabled"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" apply -f test/protection/objects.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/crm deploy/ambit-guard deploy/ambit-otel-collector --timeout=240s >/dev/null
kubectl -n "$NS" wait "servers.$G" crm --for=condition=ToolsPinned --timeout=120s >/dev/null || fail "0 crm tools not pinned"
kubectl -n "$NS" get deploy ambit-guard -o jsonpath='{.spec.template.spec.containers[0].env[*].name}' | grep -q AMBIT_OTLP_ENDPOINT || fail "0 the guard has no AMBIT_OTLP_ENDPOINT"
for _ in $(seq 1 30); do tools "$(token alice)"; [[ "$TOOLS" == *crm_get_rows* ]] && break; sleep 2; done
[[ "$TOOLS" == *crm_get_rows* ]] || fail "0 alice does not see crm_get_rows: $TOOLS"

# 1 · off: no record. Then lossless: one record for one compressed text.
since=$(date -u +"%Y-%m-%d %H:%M:%S")
set_mode off; sleep 5
raw crm_get_rows '{"n":300}' >/dev/null
sleep 5
[[ $(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['ambit.kind']='compression' AND Timestamp >= '$since'") == 0 ]] || fail "1 a record was written with mode off"
set_mode lossless
wait_for 90 "raw crm_get_rows '{\"n\":300}' >/dev/null; [[ \$(ch \"SELECT count() FROM otel.otel_logs WHERE LogAttributes['ambit.kind']='compression' AND Timestamp >= '$since'\") -gt 0 ]]" || fail "1 no guard record reached ClickHouse in 90 s"
row=$(ch "SELECT LogAttributes['mcp.target'], LogAttributes['ambit.compress.type'], LogAttributes['ambit.compress.mode'], toUInt64(LogAttributes['ambit.compress.bytes_in']) > toUInt64(LogAttributes['ambit.compress.bytes_out']), LogAttributes['jwt.groups'] FROM otel.otel_logs WHERE LogAttributes['ambit.kind']='compression' AND Timestamp >= '$since' LIMIT 1")
[[ "$row" == $'crm\tjson\tlossless\t1\t["eng"]' ]] || fail "1 unexpected record: $row"
whole=$(ch "SELECT toString(LogAttributes) || Body FROM otel.otel_logs WHERE LogAttributes['ambit.kind']='compression' AND Timestamp >= '$since' LIMIT 1")
[[ "$whole" != *@* && "$whole" != *jwt.email* && "$whole" != *item-* ]] || fail "1 the record has an email or content: $whole"
pass "1 guard record in otel_logs: crm, json, lossless, smaller output, groups [\"eng\"], no email, no content"

# 2 · the summary tables exist (the console creates them when otel_logs exists) and count the record of step 1
n_objects() { ch "SELECT count() FROM system.tables WHERE database = 'otel' AND name LIKE 'ambit_%_daily_v1%'"; }
wait_for 120 '[[ $(n_objects) == 4 ]]' || fail "2 the console did not create the four summary objects in 120 s"
ttl=$(ch "SELECT create_table_query FROM system.tables WHERE database = 'otel' AND name = 'ambit_llm_daily_v1'")
[[ "$ttl" == *"toIntervalDay(1095)"* ]] || fail "2 the retention is not 1095 days (evidence.summaryRetention 26280h): $ttl"
raw crm_get_rows '{"n":300}' >/dev/null   # a call after the views exist
rows_of() { ch "SELECT toUInt64(sum(results)) FROM otel.ambit_compress_daily_v1 WHERE grp = '$1' AND server = 'crm'"; }
wait_for 60 '[[ $(rows_of eng) -gt 0 ]]' || fail "2 the summary has no row for group eng and server crm"
[[ $(rows_of '') -ge $(rows_of eng) ]] || fail "2 the all-groups row is smaller than a group row"
sign_in keycloak alice alice-test-pass || fail "2 alice sign-in"
[[ $(api GET /api/savings) == 403 ]] || fail "2 a viewer reached /api/savings"
sign_in keycloak priya priya-test-pass || fail "2 priya sign-in"
c=$(api GET /api/savings); [[ "$c" == 200 ]] || fail "2 admin /api/savings: $c $(head -c 300 "$W/api")"
uv run --no-project python - "$W/api" <<'PY' || fail "2 /api/savings answer"
import json, sys
raw = open(sys.argv[1]).read()
d = json.loads(raw)["savings"]
assert d["estimated"] is True
c = d["totals"]["compression"]
assert c["results"] > 0 and c["tokensSaved"] > 0 and c["bytesIn"] > c["bytesOut"], c
assert any(g["group"] == "eng" for g in d["groups"]), d["groups"]
assert any(s["server"] == "crm" and s["type"] == "json" for s in d["servers"]), d["servers"]
assert "@" not in raw and "email" not in raw.lower().replace("no email", ""), "an email or the word email is in the answer"
PY
pass "2 summary tables (1095 days), the call is counted for eng and crm, /api/savings: viewer 403, admin sees it, no email"

# 3 · the views on the pinned image, in a scratch database. The Go tests run inside the ClickHouse pod, which has the
#     password in its environment; nothing reads or prints it.
CH_POD=ambit-clickhouse-0
scratch() { ch "DROP DATABASE IF EXISTS $1 SYNC" >/dev/null; ch "CREATE DATABASE $1" >/dev/null; }
# gotest DATABASE REGEXP [ENV...]: runs the compiled console tests in the pod against the scratch database.
gotest() {
  local db=$1 re=$2; shift 2
  kubectl -n "$NS" exec "$CH_POD" -- sh -c "cd /tmp && env AMBIT_TEST_CLICKHOUSE_ADDR=127.0.0.1:8123 AMBIT_TEST_CLICKHOUSE_USER=ambit AMBIT_TEST_CLICKHOUSE_PASSWORD=\"\$CLICKHOUSE_PASSWORD\" AMBIT_TEST_OTEL_LOGS_DDL=/tmp/otel_logs.sql $* ./console.test -test.run '$re' -test.v -test.timeout 90m"
}
command -v go >/dev/null || fail "3 needs go on the PATH"
arch=$(kubectl -n "$NS" exec "$CH_POD" -- uname -m); case $arch in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; esac
GOOS=linux GOARCH=$arch CGO_ENABLED=0 go test -c -o "$W/console.test" ./internal/console || fail "3 cannot build the console tests"
ch "SHOW CREATE TABLE otel.otel_logs FORMAT TSVRaw" > "$W/otel_logs.sql"
kubectl -n "$NS" exec -i "$CH_POD" -- sh -c 'cat > /tmp/console.test && chmod +x /tmp/console.test' < "$W/console.test"
kubectl -n "$NS" exec -i "$CH_POD" -- sh -c 'cat > /tmp/otel_logs.sql' < "$W/otel_logs.sql"
scratch savings_test
out=$(gotest savings_test '^TestSummariesOnClickHouse$' 2>&1) || { echo "$out" | tail -30; fail "3 summary views on ClickHouse"; }
grep -q -- "--- PASS: TestSummariesOnClickHouse" <<<"$out" || fail "3 test did not run: $(tail -5 <<<"$out")"
scratch savings_since
out=$(gotest savings_since '^TestSinceIsTheViewCreationDay$' 2>&1) || { echo "$out" | tail -30; fail "3 since / Usage on the creation day"; }
grep -q -- "--- PASS: TestSinceIsTheViewCreationDay" <<<"$out" || fail "3 since test did not run: $(tail -5 <<<"$out")"
pass "3 summary views: equal to the Usage query, one all-groups row, router rows, guard records leave Audit unchanged (ClickHouse $(ch 'SELECT version()'))"

# 4 · query cost (slow; a few GB of disk): SAVINGS_BENCH=1
if [[ "${SAVINGS_BENCH:-}" == 1 ]]; then
  scratch savings_bench; scratch savings_bench_card
  gotest savings_bench '^(TestQueryCost|TestSummaryCardinality)$' AMBIT_SAVINGS_BENCH=1 2>&1 | grep -E '^(--- |ok|FAIL|PASS)|seeded|rows|groups:' || true
  ch "SYSTEM FLUSH LOGS" >/dev/null
  echo "query_log (log_comment, runs, read_rows, read_bytes, query_duration_ms, memory_usage):"
  ch "SELECT log_comment, count(), any(read_rows), any(read_bytes), round(avg(query_duration_ms)), max(memory_usage) FROM system.query_log WHERE type = 'QueryFinish' AND log_comment LIKE 'savings-%' GROUP BY log_comment ORDER BY log_comment"
  echo "Usage page after #215 (all its queries, average of 3 runs: rows, bytes, ms):"
  ch "SELECT round(sum(read_rows) / 3), round(sum(read_bytes) / 3), round(sum(query_duration_ms) / 3) FROM system.query_log WHERE type = 'QueryFinish' AND log_comment = 'usage-new'"
  echo "per run (cold first):"
  ch "SELECT log_comment, read_rows, read_bytes, query_duration_ms FROM system.query_log WHERE type = 'QueryFinish' AND log_comment IN ('savings-old', 'savings-new', 'usage-new') ORDER BY event_time_microseconds"
  echo "parts and sizes:"
  ch "SELECT table, count() AS parts, sum(rows) AS rows, formatReadableSize(sum(data_compressed_bytes)) AS compressed, formatReadableSize(sum(data_uncompressed_bytes)) AS uncompressed FROM system.parts WHERE database IN ('savings_bench', 'savings_bench_card') AND active GROUP BY table ORDER BY table"
  ch "DROP DATABASE IF EXISTS savings_bench SYNC" >/dev/null || true
  ch "DROP DATABASE IF EXISTS savings_bench_card SYNC" >/dev/null || true
fi
done_step savings
