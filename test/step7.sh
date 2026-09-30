#!/usr/bin/env bash
# Step 7 acceptance: evidence (journey A5 "every call Alice made, including denied ones"; A3 usage).
# Gateway access logs -> OpenTelemetry Collector -> ClickHouse (one-year TTL) -> console Audit and Usage.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
LLM=http://llm.localtest.me:8080/v1/chat/completions
NEMO=nvidia/nemotron-3-super-120b-a12b:free
CURL_IMAGE=curlimages/curl:8.20.0@sha256:b3f1fb2a51d923260350d21b8654bbc607164a987e2f7c84a0ac199a67df812a
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
llm_code() { command curl -s -o "$W/llm" -w '%{http_code}' --max-time 120 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$2\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with: pong\"}]}" "$LLM"; }
ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- clickhouse-client --user ambit --password "$(kubectl -n "$NS" get secret ambit-clickhouse -o jsonpath='{.data.password}' | base64 -d)" -q "$1"; }

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" rollout status statefulset/ambit-clickhouse --timeout=300s >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-otel-collector --timeout=180s >/dev/null
kubectl -n "$NS" wait "servers.ambitproject.dev" --all --for=condition=Ready --timeout=300s >/dev/null

# 0 · traffic: alice (eng) one allowed and one refused tool call, one refused model; mira one model call
A=$(token alice); M=$(token mira)
tools "$A"
ok=$(call time_get_current_time '{"timezone":"UTC"}'); refused=$(call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}')
[[ "$ok" == ok* && "$refused" == error* ]] || fail "0 tool calls: $ok / $refused"
[[ $(llm_code "$A" "$NEMO") == 403 ]] || fail "0 alice was not refused nemotron"
mira_llm=$(llm_code "$M" "$NEMO")
pass "0 alice: 1 allowed call, 1 refused tool, 1 refused model; mira's model call returned $mira_llm"

# 1 · A5: the audit search returns alice's calls, refused ones included, and nobody else's
sign_in keycloak priya priya-test-pass
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
for _ in $(seq 1 60); do
  [[ $(api GET "/api/audit?email=alice@acme.test&from=$day&to=$next") == 200 ]] &&
    [[ $(field 'len([c for c in d["calls"] if c["route"].endswith("ambit-llm")])') -ge 1 && $(field 'len([c for c in d["calls"] if c["tool"]=="convert_time"])') -ge 1 ]] && break
  sleep 2
done
got=$(uv run --no-project python - "$W/api" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
rows = {"%s:%s:%s:%s" % (c["method"] or "llm", c["server"] or c["model"], c["tool"], c["status"])
        for c in d["calls"] if c["method"] in ("tools/call", "")}
print("|".join(sorted(rows)))
PY
)
emails=$(field '",".join(sorted({c["email"] for c in d["calls"]}))')
[[ "$got" == *"tools/call:time:get_current_time:200"* ]] || fail "1 allowed call missing: $got"
[[ "$got" == *"tools/call:time:convert_time:400"* ]] || fail "1 refused tool call missing: $got"
[[ "$got" == *"llm::"*":403"* ]] || fail "1 refused model call missing: $got"
[[ "$emails" == alice@acme.test ]] || fail "1 other users' calls in alice's audit: $emails"
reason=$(field '[c["error"] for c in d["calls"] if c["tool"]=="convert_time"][0]')
pass "1 alice's audit: $got; refused tool reason: '$reason'; only alice's calls"

# 2 · the period filter works; only admins may search
[[ $(api GET "/api/audit?email=alice@acme.test&from=2026-03-01&to=2026-04-01") == 200 && $(field 'len(d["calls"])') == 0 ]] || fail "2 March should be empty"
sign_in keycloak alice alice-test-pass
[[ $(api GET "/api/audit?email=alice@acme.test") == 403 ]] || fail "2 a viewer could read the audit log"
pass "2 period filter respected (March: 0 calls); alice (viewer) gets 403"

# 3 · one-year retention on the table, and only the collector and the console reach ClickHouse
ttl=$(ch "SELECT engine_full FROM system.tables WHERE database='otel' AND name='otel_logs'")
[[ "$ttl" == *"TTL"*"8760"* || "$ttl" == *"TTL"*"toIntervalHour(8760)"* || "$ttl" == *"TTL"*"365"* ]] || fail "3 no one-year TTL: $ttl"
out=$(kubectl -n "$NS" run ch-probe --rm -i --restart=Never --quiet --image="$CURL_IMAGE" --command -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://ambit-clickhouse:8123/ping 2>/dev/null || true)
[[ "${out:0:3}" != 200 ]] || fail "3 a random pod reached ClickHouse"
pass "3 otel_logs has TTL $(grep -o 'TTL[^S]*' <<<"$ttl" | head -c 60); other pods cannot reach ClickHouse"

# 4 · A3: LLM requests per group per day (tokens: agentgateway v1.5.0 does not log them for model routes)
sign_in keycloak priya priya-test-pass
for _ in $(seq 1 30); do
  [[ $(api GET /api/usage) == 200 && $(field 'sum(u["requests"] for u in d["usage"] if u["group"]=="ml-research")') -gt 0 ]] && break; sleep 2
done
n=$(field 'sum(u["requests"] for u in d["usage"] if u["group"]=="ml-research")')
[[ "$n" -gt 0 ]] || fail "4 no ml-research requests in usage: $(cat "$W/api")"
pass "4 usage: ml-research made $n LLM request(s) today (token counts are not logged by agentgateway v1.5.0 for model routes)"
done_step 7
