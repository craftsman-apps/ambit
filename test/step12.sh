#!/usr/bin/env bash
# Step 12 acceptance: model routing. A ModelRouter "auto" of type Rules picks a Model per request by keywords and
# length, inside each user's access rules (spec 2026-10-05-semantic-routing-design.md, PR 1 parts).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/llm/router.yaml >/dev/null
kubectl -n "$NS" wait modelrouters.ambitproject.dev/auto --for=condition=Ready --timeout=120s >/dev/null || fail "router not Ready: $(kubectl -n "$NS" get modelrouters.ambitproject.dev auto -o jsonpath='{.status.conditions}')"
LLM=http://llm.localtest.me:8080/v1
# answered TOKEN BODY [PATH] [HEADER]: prints "STATUS MODEL" (the mock answers with the model it was asked for)
answered() {
  local code; code=$(command curl -s -o "$W/r" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' ${4:+-H "$4"} -d "$2" "$LLM${3:-/chat/completions}")
  echo "$code $(uv run --no-project python -c "import json;print(json.load(open('$W/r')).get('model',''))" 2>/dev/null)"
}
msg() { printf '{"model":"auto","messages":[{"role":"user","content":%s}]}' "$(uv run --no-project python -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1")"; }
M=$(token mira); A=$(token alice); S=$(token sam)
long=$(printf 'a%.0s' $(seq 1 1997))😀   # 2001 UTF-8 bytes
short=$(printf 'a%.0s' $(seq 1 1995))😀  # 1999 UTF-8 bytes

# 1 · Rules
[[ "$(answered "$M" "$(msg 'Why does this PYTHON code fail?')")" == "200 mock/echo" ]] || fail "1 keyword (any case) → mock-echo: $(cat "$W/r")"
[[ "$(answered "$M" "$(msg "$long")")" == "200 mock/free" ]] || fail "1 2001 bytes → mock-free: $(cat "$W/r")"
[[ "$(answered "$M" "$(msg "$short")")" == "200 mock/unpriced" ]] || fail "1 1999 bytes → default: $(cat "$W/r")"
[[ "$(answered "$M" "$(msg 'hello')")" == "200 mock/unpriced" ]] || fail "1 no match → default: $(cat "$W/r")"
[[ "$(answered "$M" '{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"python"}]}]}')" == "200 mock/unpriced" ]] || fail "1 multi-part content → default: $(cat "$W/r")"
[[ "$(answered "$M" '{"model":"auto","messages":[{"role":"system","content":"python"}]}')" == "200 mock/unpriced" ]] || fail "1 no user message → default: $(cat "$W/r")"
parts=$(uv run --no-project python -c 'import json;print(json.dumps({"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"a"}]*2100}]}))')
[[ "$(answered "$M" "$parts")" == "200 mock/unpriced" ]] || fail "1 a 2100-part message must not match the length route: $(cat "$W/r")"
[[ -z "$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=ambit-router -o name 2>/dev/null)" ]] || fail "1 type Rules started ambit-router pods"
pass "1 Rules: keywords (any case), length in bytes, default; multi-part content and no user message reach the default; no extra pods"

# 4 · Access
[[ "$(answered "$A" "$(msg 'python')")" == "200 mock/unpriced" ]] || fail "4 alice may not use mock-echo → default, not an error: $(cat "$W/r")"
[[ "$(answered "$S" "$(msg 'python')" | cut -d' ' -f1)" == 400 ]] || fail "4 sam may not use the router → 400: $(cat "$W/r")"
pass "4 access: a user who may not use a route's model gets the default; a user who may not use the router gets 400"

# 5 · a client cannot send the routing label: the Gateway removes it before anything reads it
[[ "$(answered "$M" "$(msg 'hello')" "" 'x-ambit-route: coding')" == "200 mock/unpriced" ]] && ! grep -q 'x-ambit-route=' "$W/r" || fail "5 x-ambit-route reached the provider: $(cat "$W/r")"
pass "5 a client-sent x-ambit-route is removed at the Gateway"

# 8 · other paths reach only the default. If the mock cannot answer /v1/messages (the gateway translates it for
#     OpenAI-compatible providers), read the answering model from the access log instead of the response body.
[[ "$(answered "$M" '{"model":"auto","max_tokens":5,"messages":[{"role":"user","content":"python"}]}' /messages | cut -d' ' -f2)" == mock/unpriced ]] || fail "8 /v1/messages → default: $(cat "$W/r")"
pass "8 auto on /v1/messages goes to the default (only chat completions are routed)"
# 9 · the audit records the router and the answering model
row=""
for _ in $(seq 1 30); do
  row=$(kubectl -n "$NS" exec statefulset/ambit-clickhouse -- clickhouse-client -q "SELECT LogAttributes['ambit.router'], LogAttributes['gen_ai.request.model'] FROM otel.otel_logs WHERE LogAttributes['gen_ai.request.model'] = 'mock/echo' AND LogAttributes['ambit.router'] = 'auto' LIMIT 1")
  [[ -n "$row" ]] && break; sleep 2
done
[[ "$row" == $'auto\tmock/echo' ]] || fail "9 audit row: '$row'"
pass "9 audit: auto → mock/echo recorded"
done_step 12
