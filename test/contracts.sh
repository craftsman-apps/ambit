#!/usr/bin/env bash
# Contract checks: the specific things Ambit relies on in the components it ships, each failing with a
# message that names the dependency. test/upgrade.sh runs them after upgrading one component; they also
# run on their own against the pinned versions.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
attrs() { # attrs SQL-WHERE: the attribute names of the newest matching gateway record, sorted
  ch "SELECT arrayStringConcat(arraySort(mapKeys(LogAttributes)), ' ') FROM otel.otel_logs WHERE $1 ORDER BY Timestamp DESC LIMIT 1"
}

kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null || fail "C0 MCP servers not Ready"

# C1 · ToolHive: every image-based Server runs behind a proxy Service named mcp-<server>-proxy (the gateway's
#      static MCP targets and the controller's tool reader depend on that name).
for s in $(kubectl -n "$NS" get "servers.$G" -o jsonpath='{range .items[?(@.spec.image)]}{.metadata.name}{" "}{end}'); do
  kubectl -n "$NS" get service "mcp-$s-proxy" >/dev/null 2>&1 || fail "C1 ToolHive: no Service mcp-$s-proxy for Server $s"
done
pass "C1 ToolHive: each image Server has its mcp-<name>-proxy Service"

# C2 · agentgateway accepts every object the Ambit controller generates.
bad=$(kubectl -n "$NS" get agentgatewaypolicies,agentgatewaybackends,agentgatewaymodels -l app.kubernetes.io/managed-by=ambit -o json | uv run --no-project python -c '
import json, sys
bad = []
for o in json.load(sys.stdin)["items"]:
    st = o.get("status", {})
    conds = [c for a in st.get("ancestors", []) + st.get("parents", []) for c in a.get("conditions", [])] + st.get("conditions", [])
    if any(c.get("type") == "Accepted" and c.get("status") != "True" for c in conds):
        bad.append(o["kind"] + "/" + o["metadata"]["name"])
print(" ".join(bad))')
[[ -z "$bad" ]] || fail "C2 agentgateway refused generated objects: $bad"
pass "C2 agentgateway accepts every generated policy, backend and model"

# C3 · agentgateway access log → collector → ClickHouse: the attributes the Audit and Usage pages read.
A=$(token alice); M=$(token mira)
tools "$A"; call time_get_current_time '{"timezone":"UTC"}' >/dev/null; call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}' >/dev/null
command curl -s -o /dev/null -H "Authorization: Bearer $M" -H 'Content-Type: application/json' \
  -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' http://llm.localtest.me:8080/v1/chat/completions || true
since=$(date -u -v-2M +"%Y-%m-%d %H:%M:%S" 2>/dev/null || date -u -d '2 minutes ago' +"%Y-%m-%d %H:%M:%S")
for _ in $(seq 1 30); do
  mcp=$(attrs "Timestamp >= '$since' AND LogAttributes['gen_ai.tool.name'] = 'get_current_time'" 2>/dev/null || true)
  refused=$(attrs "Timestamp >= '$since' AND LogAttributes['gen_ai.tool.name'] = 'convert_time'" 2>/dev/null || true)
  llm=$(attrs "Timestamp >= '$since' AND LogAttributes['protocol'] = 'llm'" 2>/dev/null || true)
  [[ -n "$mcp" && -n "$refused" && -n "$llm" ]] && break; sleep 2
done
[[ -n "$mcp" ]] || fail "C3 no access-log record of an MCP tool call reached ClickHouse (attribute gen_ai.tool.name missing?)"
[[ -n "$llm" ]] || fail "C3 no access-log record of an LLM request reached ClickHouse (attribute protocol=llm missing?)"
missing=()
for a in jwt.email jwt.sub jwt.groups route protocol mcp.method.name mcp.target gen_ai.tool.name; do [[ " $mcp " == *" $a "* ]] || missing+=("MCP:$a"); done
for a in jwt.email jwt.groups route protocol; do [[ " $llm " == *" $a "* ]] || missing+=("LLM:$a"); done
[[ " $refused " == *" error "* ]] || missing+=("refused MCP:error")
[[ " $mcp " == *" http.status "* || " $mcp " == *" http.response.status_code "* ]] || missing+=("MCP:http.status|http.response.status_code")
(( ${#missing[@]} == 0 )) || fail "C3 agentgateway access log lacks attributes Ambit reads: ${missing[*]}"
status_name=$([[ " $mcp " == *" http.response.status_code "* ]] && echo http.response.status_code || echo http.status)
tokens=$([[ " $llm " == *" gen_ai.usage.input_tokens "* ]] && echo "present" || echo "absent (risk 13)")
pass "C3 access log has every attribute Ambit reads (status as $status_name; LLM token counts $tokens)"

# C4 · ClickHouse keeps audit records for the retention the chart sets (one year by default).
ttl=$(ch "SELECT engine_full FROM system.tables WHERE database='otel' AND name='otel_logs'")
[[ "$ttl" == *"toIntervalDay(365)"* ]] || fail "C4 ClickHouse otel_logs TTL is not 365 days: $ttl"
pass "C4 ClickHouse otel_logs keeps records 365 days"

# C5 · Dex serves the issuer the console signs in with.
iss=$(command curl -s http://console.localtest.me:8080/dex/.well-known/openid-configuration | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["issuer"])')
[[ "$iss" == "http://console.localtest.me:8080/dex" ]] || fail "C5 Dex issuer is '$iss'"
pass "C5 Dex issuer $iss"

# C6 · envoy ratelimit is up and loaded Ambit's domain config when budgets exist.
kubectl -n "$NS" rollout status deploy/ambit-ratelimit --timeout=120s >/dev/null || fail "C6 ratelimit not ready"
pass "C6 envoy ratelimit ready"
done_step contracts
