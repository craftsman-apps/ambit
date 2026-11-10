#!/usr/bin/env bash
# Step 7 acceptance: evidence (journey A5 "every call User01 made, including denied ones"; A3 usage).
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

# 0 · traffic: user01 (eng) one allowed and one refused tool call, one refused model, one unknown model;
#     user09 one model call
start=$(date -u +%Y-%m-%dT%H:%M:%S)
A=$(token user01); M=$(token user09)
tools "$A"
ok=$(call time_get_current_time '{"timezone":"UTC"}'); refused=$(call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}')
[[ "$ok" == ok* && "$refused" == error* ]] || fail "0 tool calls: $ok / $refused"
[[ $(llm_code "$A" "$NEMO") == 403 ]] || fail "0 user01 was not refused nemotron"
[[ $(llm_code "$A" step7/no-such-model) == 404 ]] || fail "0 user01's unknown model was not answered 404: $(head -c 200 "$W/llm")"
[[ $(command curl -s -o "$W/models" -w '%{http_code}' -H "Authorization: Bearer $A" "${LLM%/chat/completions}/models") == 200 ]] || fail "0 user01's model list: $(head -c 200 "$W/models")"
[[ $(llm_code ambit_step7step7step7step7step7step7step7step7 mock/unpriced) == 403 ]] || fail "0 an invalid key was not refused: $(head -c 200 "$W/llm")"
user09_llm=$(llm_code "$M" "$NEMO")
pass "0 user01: 1 allowed call, 1 refused tool, 1 refused model, 1 unknown model, 1 model list; 1 invalid key; user09's model call returned $user09_llm"

# 1 · A5: the audit search returns user01's calls, refused ones included, and nobody else's
sign_in keycloak user10 user10-test-pass
# The statuses of user01's model calls answered 403 or 404 since this step started.
refusals() { field '" ".join(sorted({c["status"] for c in d["calls"] if c["route"].endswith("ambit-llm") and c["time"] >= "'"$start"'" and c["status"] in ("403", "404")}))'; }
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
for _ in $(seq 1 60); do
  [[ $(api GET "/api/audit?email=user01@example.com&from=$day&to=$next") == 200 ]] &&
    [[ $(refusals) == "403 404" && $(field 'len([c for c in d["calls"] if c["tool"]=="convert_time"])') -ge 1 ]] && break
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
# Issue #87: a model refusal answered by the gateway itself (403 denied, 404 unknown) carries the caller since
# agentgateway v1.7.0, so user01's search finds both. The model name of these rows is still empty (issue #117).
[[ "$(refusals)" == "403 404" ]] || fail "1 user01's refused (403) and unknown (404) model calls are not both in their audit: '$(refusals)'"
# Issue #117: the refused rows name the model asked for, and the model list call is there with its path.
since() { echo '[c for c in d["calls"] if c["time"] >= "'"$start"'"]'; }
[[ $(field "sorted({c['model'] for c in $(since) if c['status'] in ('403', '404') and c['route'].endswith('ambit-llm')})") == "['$NEMO', 'step7/no-such-model']" ]] ||
  fail "1 the refused model rows do not name their models: $(field "[(c['status'], c['model']) for c in $(since) if c['status'] in ('403', '404')]")"
[[ $(field "len([c for c in $(since) if c.get('path') == '/v1/models'])") -ge 1 ]] || fail "1 user01's model list call is not in their audit"
[[ "$emails" == user01@example.com ]] || fail "1 other users' calls in user01's audit: $emails"
reason=$(field '[c["error"] for c in d["calls"] if c["tool"]=="convert_time"][0]')
pass "1 user01's audit: $got; refused tool reason: '$reason'; only user01's calls"

# 1b · issue #117: the request with an invalid key is in the invalid-key search, with no person and only six fields
for _ in $(seq 1 30); do
  [[ $(api GET "/api/audit?invalidKey=true&from=$day&to=$next") == 200 ]] && [[ $(field "len($(since))") -ge 1 ]] && break; sleep 2
done
row=$(field "[c for c in $(since) if c['path'] == '/v1/chat/completions' and c['status'] == '403'][:1]")
[[ "$row" != "[]" ]] || fail "1b no invalid-key request: $(head -c 300 "$W/api")"
extra=$(field "sorted({k for c in $(since) for k, v in c.items() if v not in ('', 0, None) and k not in ('time', 'route', 'path', 'status', 'error', 'clientAddress', 'identity')})")
[[ "$extra" == "[]" ]] || fail "1b the invalid-key rows carry more fields: $extra"
[[ $(api GET "/api/audit?invalidKey=true&email=user01@example.com") == 400 ]] || fail "1b email and invalidKey together were accepted"
pass "1b invalid-key search: the 403 on /v1/chat/completions, no person, only time, route, path, status, error and client address"

# 2 · the period filter works; only admins may search
[[ $(api GET "/api/audit?email=user01@example.com&from=2026-03-01&to=2026-04-01") == 200 && $(field 'len(d["calls"])') == 0 ]] || fail "2 March should be empty"
sign_in keycloak user01 user01-test-pass
[[ $(api GET "/api/audit?email=user01@example.com") == 403 ]] || fail "2 a viewer could read the audit log"
pass "2 period filter respected (March: 0 calls); user01 (viewer) gets 403"

# 3 · one-year retention on the table, and only the collector and the console reach ClickHouse
ttl=$(ch "SELECT engine_full FROM system.tables WHERE database='otel' AND name='otel_logs'")
[[ "$ttl" == *"TTL"*"8760"* || "$ttl" == *"TTL"*"toIntervalHour(8760)"* || "$ttl" == *"TTL"*"365"* ]] || fail "3 no one-year TTL: $ttl"
out=$(kubectl -n "$NS" run ch-probe --rm -i --restart=Never --quiet --image="$CURL_IMAGE" --command -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://ambit-clickhouse:8123/ping 2>/dev/null || true)
[[ "${out:0:3}" != 200 ]] || fail "3 a random pod reached ClickHouse"
pass "3 otel_logs has TTL $(grep -o 'TTL[^S]*' <<<"$ttl" | head -c 60); other pods cannot reach ClickHouse"

# 4 · A3 and requirement 17: LLM requests, tokens and cost per group per day. user09 calls the priced test
#     model twice (once streamed); each answer carries 12 input and 3 output tokens. The check measures the
#     increase from these two requests, not totals that earlier requests could already fill.
sign_in keycloak user10 user10-test-pass
mr='[u for u in d["usage"] if u["group"]=="ml-research"]'
totals() { [[ $(api GET /api/usage) == 200 ]] && field "f\"{sum(u['inputTokens'] for u in $mr)} {sum(u['outputTokens'] for u in $mr)} {sum(u.get('cost') or 0 for u in $mr):.9f}\""; }
read -r in0 out0 cost0 <<<"$(totals)"
for s in false true; do
  code=$(command curl -s -o "$W/llm" -w '%{http_code}' -H "Authorization: Bearer $M" -H 'Content-Type: application/json' \
    -d "{\"model\":\"mock/echo\",\"stream\":$s,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" http://llm.localtest.me:8080/v1/chat/completions)
  [[ "$code" == 200 ]] || fail "4 mock model call (stream $s) answered $code: $(head -c 200 "$W/llm")"
done
for _ in $(seq 1 30); do
  read -r in1 out1 cost1 <<<"$(totals)"
  (( in1 - in0 >= 24 && out1 - out0 >= 6 )) && break; sleep 2
done
(( in1 - in0 >= 24 && out1 - out0 >= 6 )) || fail "4 usage did not grow by both answers' tokens: input $in0 -> $in1, output $out0 -> $out1"
uv run --no-project python -c "import sys; sys.exit(0 if float(sys.argv[2]) - float(sys.argv[1]) >= 0.000048 - 1e-12 else 1)" "$cost0" "$cost1" || fail "4 cost did not grow by the priced answers: $cost0 -> $cost1"
pass "4 usage: two answers (one streamed) added $((in1 - in0)) input and $((out1 - out0)) output tokens and \$$(uv run --no-project python -c "import sys; print(f'{float(sys.argv[2]) - float(sys.argv[1]):.6f}')" "$cost0" "$cost1") to ml-research's cost"
done_step 7
