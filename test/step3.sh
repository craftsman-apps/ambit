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
G=ambitproject.dev

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

ensure_llm_setup "$NS"

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
  c=$(llm "$A" "$GEMMA"); reached_provider "$c" || fail "4 alice -> gemma: $c $(head -c 200 "$W/llm")"
  pass "4 alice -> gemma: $VIA"
  c=$(llm "$M" "$NEMO"); reached_provider "$c" || fail "5 mira -> nemotron-super: $c $(head -c 200 "$W/llm")"
  pass "5 mira (ml-research) -> nemotron-super: $VIA (A2)"
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
# The controller writes only status: a reconcile leaves every Ambit object's generation as it was (earlier
# edits by people, e.g. through the console, are not the controller's).
gens() { kubectl -n "$NS" get "models.$G,accessrules.$G" -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.generation}{" "}{end}'; }
g0=$(gens)
# Creating a Budget is a watched event, so the controller reconciles everything; its status shows the pass ended.
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: test-reconcile }
spec: { group: test-reconcile, userRequestsPerDay: 1 }
YAML
kubectl -n "$NS" wait "budgets.$G" test-reconcile --for=jsonpath='{.status.conditions[0].type}'=Ready --timeout=60s >/dev/null || fail "9 the controller did not reconcile"
g1=$(gens)
kubectl -n "$NS" delete "budgets.$G" test-reconcile >/dev/null
[[ "$g0" == "$g1" ]] || fail "9 controller changed the spec of Ambit objects (generations before: $g0; after a reconcile: $g1)"
pass "9 generated object owned by its Model, no Argo CD metadata; a reconcile leaves Ambit objects' spec unchanged"

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
# 12 · Azure and Bedrock Models (fake credentials, no request reaches a cloud): agentgateway accepts the generated
# objects, an access rule naming them compiles, a dollar budget charges their prices; the gateway ServiceAccount is
# annotated. The objects are removed on every exit, passed or failed.
cleanup12() {
  kubectl -n "$NS" delete "models.$G" test-azure test-bedrock-keys test-bedrock-role --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" test-cloud --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" test-cloud --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete secret ambit-llm-azure-test ambit-llm-aws-test --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup12; on_exit $rc' EXIT   # test/lib.sh's handler still decides the exit code
kubectl -n "$NS" create secret generic ambit-llm-azure-test --from-literal=Authorization=fake-azure-key --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic ambit-llm-aws-test --from-literal=accessKey=AKIAFAKE --from-literal=secretKey=fake --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: test-azure }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure-test }, azure: { resourceName: ambit-test, resourceType: OpenAI },
        price: { inputPerMillionTokens: "2", outputPerMillionTokens: "8" } }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: test-bedrock-keys }
spec: { provider: Bedrock, model: us.anthropic.claude-sonnet-4-5-20250929-v1:0, credentialsRef: { name: ambit-llm-aws-test }, bedrock: { region: eu-west-1 },
        price: { inputPerMillionTokens: "3.3", outputPerMillionTokens: "16.5" } }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: test-bedrock-role }
spec: { provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0, price: { inputPerMillionTokens: "3", outputPerMillionTokens: "15" } }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: test-cloud }
spec: { groups: [test-cloud], models: [test-azure, test-bedrock-keys, test-bedrock-role] }
---
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: test-cloud }
spec: { group: test-cloud, groupDollarsPerDay: "5" }
YAML
for m in test-azure test-bedrock-keys test-bedrock-role; do
  kubectl -n "$NS" wait "models.$G" "$m" --for=condition=Ready --timeout=60s >/dev/null \
    || fail "12 $m not accepted by agentgateway: $(kubectl -n "$NS" get "models.$G" "$m" -o jsonpath='{.status.conditions[0].message}')"
done
kubectl -n "$NS" wait "accessrules.$G" test-cloud --for=condition=Ready --timeout=60s >/dev/null || fail "12 access rule naming the cloud models not Ready"
kubectl -n "$NS" wait "budgets.$G" test-cloud --for=condition=Ready --timeout=60s >/dev/null \
  || fail "12 dollar budget not Ready: $(kubectl -n "$NS" get "budgets.$G" test-cloud -o jsonpath='{.status.conditions[0].message}')"
# Budget Ready does not look at prices: the generated cost expression must charge each cloud model (prices in micros).
ok=""
for _ in $(seq 1 30); do
  pol=$(kubectl -n "$NS" get agentgatewaypolicies -l app.kubernetes.io/managed-by=ambit -o json)
  ok=$(uv run --no-project python -c 'import sys
q = chr(34)
s = sys.stdin.read().replace(chr(92) + q, q)  # undo JSON escaping of the CEL text
prices = {"gpt-4.1": (2000000, 8000000), "us.anthropic.claude-sonnet-4-5-20250929-v1:0": (3300000, 16500000),
          "anthropic.claude-sonnet-4-5-20250929-v1:0": (3000000, 15000000)}
print("yes" if all(f"{q}{m}{q}: {p}u" in s for m, ps in prices.items() for p in ps) else "")' <<<"$pol")
  [[ -n "$ok" ]] && break; sleep 1
done
[[ -n "$ok" ]] || fail "12 the cost expression does not charge the cloud models' prices"
hdr=$(kubectl -n "$NS" get agentgatewaymodel test-azure -o jsonpath='{.spec.policies.auth.location.header.name}')
noauth=$(kubectl -n "$NS" get agentgatewaymodel test-bedrock-role -o jsonpath='{.spec.policies.auth}')
[[ "$hdr" == api-key && -z "$noauth" ]] || fail "12 auth: azure header '$hdr', bedrock-role auth '$noauth'"
# The ServiceAccount the gateway pods actually run as carries exactly the configured annotation.
sa=$(kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].spec.serviceAccountName}')
val=$(kubectl -n "$NS" get sa "$sa" -o jsonpath='{.metadata.annotations.ambitproject\.dev/test}')
[[ -n "$sa" && "$val" == kind ]] || fail "12 gateway ServiceAccount '$sa' annotation ambitproject.dev/test is '$val', want 'kind'"
cleanup12; trap 'on_exit $?' EXIT
pass "12 Azure and Bedrock (keys and AWS identity) accepted by agentgateway; rule compiles; budget charges their prices; gateway ServiceAccount annotated"
# 13 · Vertex AI Models (a key file that is not real, and the gateway's identity; no request reaches Google):
# agentgateway accepts both, a rule names them, the dollar budget charges their prices.
cleanup13() {
  kubectl -n "$NS" delete "models.$G" test-vertex-key test-vertex-identity --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" test-vertex --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" test-vertex --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete secret ambit-llm-gcp-test --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup13; on_exit $rc' EXIT
kubectl -n "$NS" create secret generic ambit-llm-gcp-test --from-literal=credentials.json='{}' --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: test-vertex-key }
spec: { provider: VertexAI, model: gemini-2.5-pro, credentialsRef: { name: ambit-llm-gcp-test }, vertexai: { projectId: ambit-test, region: europe-west4 },
        price: { inputPerMillionTokens: "1.25", outputPerMillionTokens: "10" } }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: test-vertex-identity }
spec: { provider: VertexAI, model: "claude-sonnet-4-5@20250929", vertexai: { projectId: ambit-test }, price: { inputPerMillionTokens: "3", outputPerMillionTokens: "15" } }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: test-vertex }
spec: { groups: [test-vertex], models: [test-vertex-key, test-vertex-identity] }
---
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: test-vertex }
spec: { group: test-vertex, groupDollarsPerDay: "5" }
YAML
for m in test-vertex-key test-vertex-identity; do
  kubectl -n "$NS" wait "models.$G" "$m" --for=condition=Ready --timeout=60s >/dev/null \
    || fail "13 $m not accepted by agentgateway: $(kubectl -n "$NS" get "models.$G" "$m" -o jsonpath='{.status.conditions[0].message}')"
done
kubectl -n "$NS" wait "accessrules.$G" test-vertex --for=condition=Ready --timeout=60s >/dev/null || fail "13 access rule naming the Vertex AI models not Ready"
kubectl -n "$NS" wait "budgets.$G" test-vertex --for=condition=Ready --timeout=60s >/dev/null || fail "13 dollar budget not Ready"
ok=""
for _ in $(seq 1 30); do
  pol=$(kubectl -n "$NS" get agentgatewaypolicies -l app.kubernetes.io/managed-by=ambit -o json)
  ok=$(uv run --no-project python -c 'import sys, re
q = chr(34)
s = sys.stdin.read().replace(chr(92) + q, q)
# The cost expression multiplies inputTokens by one price map and outputTokens by the other: check each side.
m = re.search(r"inputTokens\) \* \{(.*?)\}\[llm\.requestModel\].*?outputTokens\) \* \{(.*?)\}\[llm\.requestModel\]", s)
prices = {"gemini-2.5-pro": (1250000, 10000000), "claude-sonnet-4-5@20250929": (3000000, 15000000)}
ok = m and all(f"{q}{k}{q}: {i}u" in m.group(1) and f"{q}{k}{q}: {o}u" in m.group(2) for k, (i, o) in prices.items())
print("yes" if ok else "")' <<<"$pol")
  [[ -n "$ok" ]] && break; sleep 1
done
[[ -n "$ok" ]] || fail "13 the cost expression does not charge the Vertex AI models' prices"
key=$(kubectl -n "$NS" get agentgatewaymodel test-vertex-key -o jsonpath='{.spec.policies.auth.gcp.secretRef.name}')
noauth=$(kubectl -n "$NS" get agentgatewaymodel test-vertex-identity -o jsonpath='{.spec.policies.auth}')
[[ "$key" == ambit-llm-gcp-test && -z "$noauth" ]] || fail "13 auth: key-file Model '$key', identity Model auth '$noauth'"
cleanup13; trap 'on_exit $?' EXIT
pass "13 Vertex AI (key file and gateway identity) accepted by agentgateway; rule compiles; budget charges their prices"
done_step 3a
