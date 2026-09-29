#!/usr/bin/env bash
# Step 3b acceptance: an admin changes access rules in the console (Direct mode) and the gateway follows.
# Runs after step3.sh (Keycloak, LLM route, test models and rules in place).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
KC=http://keycloak.localtest.me:8080
LLM=http://llm.localtest.me:8080/v1/chat/completions
KEYFILE=${OPENROUTER_KEY_FILE:-$HOME/.config/ambit/openrouter-api-key}
NEMO=nvidia/nemotron-3-super-120b-a12b:free
G=ambit.craftsmanapps.com

token() { command curl -s -d grant_type=password -d client_id=ambit-cli-test -d username="$1" -d password="$1-test-pass" \
  "$KC/realms/acme/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])'; }
llm() { # llm TOKEN MODEL -> status (retries free-tier 429)
  local code i
  for i in 1 2 3 4; do
    code=$(command curl -s -o "$W/llm" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' -H "Authorization: Bearer $1" \
      -d "{\"model\":\"$2\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: pong\"}]}" "$LLM")
    [[ "$code" == 429 && $i -lt 4 ]] || break; sleep 20
  done; echo "$code"
}
# api METHOD PATH [JSON]: console API call with the signed-in cookie jar; body in $W/api
api() {
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -b "$W/jar")
  [[ $# -ge 3 ]] && a+=(-H 'Content-Type: application/json' -d "$3")
  command curl "${a[@]}" "$BASE$2"
}
expr_of() { kubectl -n "$NS" get agentgatewaymodel "$1" -o jsonpath='{.spec.policies.authorization.policy.matchExpressions[0]}'; }
wait_expr() { for _ in $(seq 1 30); do [[ "$(expr_of "$1")" == "$2" ]] && return 0; sleep 1; done; fail "$1: '$(expr_of "$1")' != '$2'"; }
ORIG='jwt.groups.exists(g, g in ["ml-research"])'

# 1 · a viewer reads the catalog but cannot change it
sign_in keycloak alice alice-test-pass || fail "1 alice sign-in"
[[ $(api GET /api/models) == 200 ]] && grep -q '"name":"nemotron-3-super"' "$W/api" || fail "1 viewer cannot list models: $(cat "$W/api")"
[[ $(api POST /api/access-rules '{"name":"eng-nemotron","groups":["eng"],"models":["nemotron-3-super"]}') == 403 ]] || fail "1 viewer could create a rule"
pass "1 alice (viewer) lists models; creating a rule -> 403"

# 2 · an admin grants eng the restricted model; the gateway follows
sign_in keycloak priya priya-test-pass || fail "2 priya sign-in"
c=$(api POST /api/access-rules '{"name":"eng-nemotron","groups":["eng"],"models":["nemotron-3-super"]}')
[[ "$c" == 201 ]] || fail "2 create: $c $(cat "$W/api")"
wait_expr nemotron-3-super 'jwt.groups.exists(g, g in ["eng", "ml-research"])'
[[ $(kubectl -n "$NS" get "accessrules.$G" eng-nemotron -o jsonpath='{.metadata.annotations.ambit\.craftsmanapps\.com/created-by}') == priya@acme.test ]] \
  || fail "2 creator not recorded"
if [[ -s "$KEYFILE" ]]; then
  c=$(llm "$(token alice)" "$NEMO"); [[ "$c" == 200 ]] || fail "2 alice still denied after the rule: $c $(head -c 200 "$W/llm")"
  pass "2 priya (admin) creates eng-nemotron in the console -> alice gets 200 from nemotron-super"
else
  pass "2 priya creates eng-nemotron -> gateway policy now includes eng (SKIP the OpenRouter call: no key)"
fi

# 3 · the console shows the merged policy the gateway enforces
[[ $(api GET /api/access-rules/eng-nemotron) == 200 ]] || fail "3 detail"
grep -qF 'jwt.groups.exists(g, g in [\"eng\", \"ml-research\"])' "$W/api" || fail "3 effective policy wrong: $(cat "$W/api")"
pass "3 rule detail shows the merged policy: eng + ml-research"

# 4 · an edit takes effect: eng replaced by finance, alice denied again (no provider call)
c=$(api PUT /api/access-rules/eng-nemotron '{"groups":["finance"],"models":["nemotron-3-super"]}'); [[ "$c" == 200 ]] || fail "4 update: $c $(cat "$W/api")"
wait_expr nemotron-3-super 'jwt.groups.exists(g, g in ["finance", "ml-research"])'
[[ $(llm "$(token alice)" "$NEMO") == 403 ]] || fail "4 alice kept access after the edit"
pass "4 edit to finance -> alice 403 again"

# 5 · invalid input and cross-site forms are refused with a reason
[[ $(api POST /api/access-rules '{"name":"bad","groups":["eng"],"allSignedIn":true,"models":["gemma-4-31b"]}') == 400 ]] \
  && grep -q 'either groups or everyone' "$W/api" || fail "5 invalid rule accepted: $(cat "$W/api")"
c=$(command curl -s -o /dev/null -w '%{http_code}' -b "$W/jar" -X POST -d 'name=x' "$BASE/api/access-rules")
[[ "$c" == 415 ]] || fail "5 form post not refused: $c"
pass "5 invalid rule -> 400 with the reason; form-encoded post -> 415"

# 6 · rules managed in Git cannot be changed in the console
kubectl -n "$NS" annotate "accessrules.$G" ml-research-nemotron "argocd.argoproj.io/tracking-id=ambit-config:$G/AccessRule:$NS/ml-research-nemotron" --overwrite >/dev/null
[[ $(api DELETE /api/access-rules/ml-research-nemotron) == 409 ]] || fail "6 git-managed rule was deleted"
kubectl -n "$NS" annotate "accessrules.$G" ml-research-nemotron argocd.argoproj.io/tracking-id- >/dev/null
pass "6 Git-managed rule -> delete refused (409)"

# 7 · delete restores the previous state
[[ $(api DELETE /api/access-rules/eng-nemotron) == 204 ]] || fail "7 delete: $(cat "$W/api")"
wait_expr nemotron-3-super "$ORIG"
pass "7 delete eng-nemotron -> policy back to ml-research only"
echo "PASS: step 3b"
