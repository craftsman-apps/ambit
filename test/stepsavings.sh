#!/usr/bin/env bash
# Feature B acceptance (the Savings page):
#   1 a compressed tool result writes one guard record to otel_logs: server, type, sizes and the caller's groups,
#     no email, no content; a result that is not compressed writes none
# Run as one locked unit (CLAUDE.md): lockf /tmp/ambit-kind.lock sh -c 'make image install && test/stepsavings.sh'
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
VALUES=(-f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml)
cleanup() {
  kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  # Evidence was off before this step.
  command helm upgrade ambit charts/ambit -n "$NS" "${VALUES[@]}" --wait --timeout 5m >/dev/null 2>&1 || true
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
done_step savings
