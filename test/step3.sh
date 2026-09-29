#!/usr/bin/env bash
# Step 3a acceptance: Model + AccessRule -> ambit-controller -> AgentgatewayModel with per-model access.
# Journeys A2 (only one group may use an expensive model) and U3 (a denied user sees why).
# Needs ~/.config/ambit/openrouter-api-key for the calls that reach OpenRouter; they use free models only.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
KC=http://keycloak.localtest.me:8080
LLM=http://llm.localtest.me:8080/v1/chat/completions
KEYFILE=${OPENROUTER_KEY_FILE:-$HOME/.config/ambit/openrouter-api-key}
GEMMA=google/gemma-4-31b-it:free
NEMO=nvidia/nemotron-3-super-120b-a12b:free
G=ambit.craftsman-apps.github.io

token() { command curl -s -d grant_type=password -d client_id=ambit-cli-test -d username="$1" -d password="$1-test-pass" \
  "$KC/realms/acme/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])'; }
# llm TOKEN MODEL: prints the HTTP status; body in $W/llm. Retries OpenRouter's free-tier 429 up to 3 times.
llm() {
  local -a h=(-H 'Content-Type: application/json'); [[ -n "$1" ]] && h+=(-H "Authorization: Bearer $1")
  local code i
  for i in 1 2 3 4; do
    code=$(command curl -s -o "$W/llm" -w '%{http_code}' --max-time 120 "${h[@]}" \
      -d "{\"model\":\"$2\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: pong\"}]}" "$LLM")
    [[ "$code" == 429 && $i -lt 4 ]] || break
    sleep 20
  done
  echo "$code"
}
kc_admin() {
  local tok; tok=$(command curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  command curl -s -X "$1" -H "Authorization: Bearer $tok" "$KC/admin/realms/acme$2"
}
expr_of() { kubectl -n "$NS" get agentgatewaymodel "$1" -o jsonpath='{.spec.policies.authorization.policy.matchExpressions[0]}'; }
wait_expr() { # wait_expr MODEL EXPECTED
  for _ in $(seq 1 30); do [[ "$(expr_of "$1")" == "$2" ]] && return 0; sleep 1; done
  fail "$1: expression is '$(expr_of "$1")', expected '$2'"
}

# Setup: test IdP with LLM settings, the provider key as a Secret, and the test catalog.
kubectl -n "$NS" apply -f test/keycloak/keycloak.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
HAVE_KEY=false
if [[ -s "$KEYFILE" ]]; then
  kubectl -n "$NS" create secret generic ambit-llm-openrouter --from-file=Authorization="$KEYFILE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  HAVE_KEY=true
else
  kubectl -n "$NS" create secret generic ambit-llm-openrouter --from-literal=Authorization=no-key --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi
kubectl -n "$NS" apply -f test/llm/objects.yaml >/dev/null

# 1 · controller running, models accepted by agentgateway, rules compiled
kubectl -n "$NS" rollout status deploy/ambit-controller --timeout=120s >/dev/null
kubectl -n "$NS" wait "models.$G" --all --for=condition=Ready --timeout=120s >/dev/null || fail "1 models not Ready: $(kubectl -n "$NS" get "models.$G" -o wide)"
kubectl -n "$NS" wait "accessrules.$G" --all --for=condition=Ready --timeout=60s >/dev/null || fail "1 access rules not Ready"
pass "1 controller ready; 2 models accepted by agentgateway; 2 access rules compiled"

A=$(token alice); M=$(token mira)
# 2 · no token
[[ $(llm "" "$GEMMA") == 401 ]] || fail "2 request without a token was not rejected"
pass "2 no token -> 401"

# 3 · U3: denied, with a machine-readable reason, before any provider call
[[ $(llm "$A" "$NEMO") == 403 ]] || fail "3 alice got $(cat "$W/llm") for the restricted model"
grep -q '"code":"model_authorization_denied"' "$W/llm" || fail "3 denial has no reason: $(cat "$W/llm")"
pass "3 alice (eng) -> nemotron-super: 403 model_authorization_denied"

# 4-5 · allowed calls reach OpenRouter (free models)
if $HAVE_KEY; then
  c=$(llm "$A" "$GEMMA"); [[ "$c" == 200 ]] || fail "4 alice -> gemma: $c $(head -c 200 "$W/llm")"
  pass "4 alice -> gemma: 200 from OpenRouter"
  c=$(llm "$M" "$NEMO"); [[ "$c" == 200 ]] || fail "5 mira -> nemotron-super: $c $(head -c 200 "$W/llm")"
  pass "5 mira (ml-research) -> nemotron-super: 200 from OpenRouter (A2)"
else
  echo "SKIP 4-5 no key at $KEYFILE; the checks that do not reach OpenRouter still ran"
fi

# 6 · A2: removing the group in the IdP removes access at the next token
uid=$(kc_admin GET "/users?username=mira&exact=true" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
gid=$(kc_admin GET "/groups?search=ml-research" | uv run --no-project python -c 'import json,sys;print([g["id"] for g in json.load(sys.stdin) if g["name"]=="ml-research"][0])')
kc_admin DELETE "/users/$uid/groups/$gid" >/dev/null
[[ $(llm "$(token mira)" "$NEMO") == 403 ]] || fail "6 mira kept access after leaving ml-research"
kc_admin PUT "/users/$uid/groups/$gid" >/dev/null
pass "6 mira removed from ml-research in Keycloak -> 403 at next token (restored afterwards)"

# 7 · deny by default: a model with no rule is closed to everyone
kubectl -n "$NS" delete "accessrules.$G" ml-research-nemotron >/dev/null
wait_expr nemotron-3-super false
[[ $(llm "$M" "$NEMO") == 403 ]] || fail "7 mira reached a model with no access rule"
kubectl -n "$NS" apply -f test/llm/objects.yaml >/dev/null
wait_expr nemotron-3-super 'jwt.groups.exists(g, g in ["ml-research"])'
pass "7 no rule -> expression false -> 403; rule restored"

# 8 · a rule naming a missing model is Degraded, and says which name
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: typo-rule }
spec: { groups: [eng], models: [gemma-4-31b, gemma-4-31] }
YAML
for _ in $(seq 1 30); do r=$(kubectl -n "$NS" get "accessrules.$G" typo-rule -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'); [[ "$r" == UnknownModel ]] && break; sleep 1; done
msg=$(kubectl -n "$NS" get "accessrules.$G" typo-rule -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
[[ "$r" == UnknownModel && "$msg" == *gemma-4-31* ]] || fail "8 typo rule: reason=$r message=$msg"
kubectl -n "$NS" delete "accessrules.$G" typo-rule >/dev/null
pass "8 rule with an unknown model -> Ready=False UnknownModel: $msg"

# 9 · GitOps rules on generated objects
o=$(kubectl -n "$NS" get agentgatewaymodel nemotron-3-super -o json)
uv run --no-project python - "$o" <<'PY' || fail "9 generated object breaks a GitOps rule"
import json,sys
o=json.loads(sys.argv[1]); md=o["metadata"]
refs=md.get("ownerReferences",[])
assert len(refs)==1 and refs[0]["kind"]=="Model" and refs[0]["name"]=="nemotron-3-super" and refs[0].get("controller"), refs
assert not [k for k in md.get("annotations",{}) if k.startswith("argocd.argoproj.io/")], md.get("annotations")
assert "app.kubernetes.io/instance" not in md.get("labels",{}), md.get("labels")
PY
for kind in models accessrules; do
  gens=$(kubectl -n "$NS" get "$kind.$G" -o jsonpath='{range .items[*]}{.metadata.generation}{" "}{end}')
  [[ -z "$(tr -d '1 ' <<<"$gens")" ]] || fail "9 controller changed the spec of $kind (generations: $gens)"
done
pass "9 generated object owned by its Model, no Argo CD metadata; Ambit objects' spec never written (generation 1)"

# 10 · a manual edit of a generated object is reverted
kubectl -n "$NS" patch agentgatewaymodel nemotron-3-super --type=json \
  -p '[{"op":"replace","path":"/spec/policies/authorization/policy/matchExpressions","value":["true"]}]' >/dev/null
wait_expr nemotron-3-super 'jwt.groups.exists(g, g in ["ml-research"])'
pass "10 manual edit (opening the model to everyone) reverted by the controller"

# 11 · chart renders identically with LLM traffic on
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml > "$W/r1"
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml > "$W/r2"
cmp -s "$W/r1" "$W/r2" || fail "11 two renders differ"
pass "11 chart renders identically"
echo "PASS: step 3a"
