#!/usr/bin/env bash
# Fallback and several servers (Model.spec.fallback and Model.spec.additionalEndpoints) on kind, with the mock
# provider (test/llm/mock.yaml): a model name that starts with mock/fail500 or mock/fail429 answers with that status.
# 1 · the objects are accepted; the Ready message says who may use a Model but not its fallback
# 2 · a provider that returns 500, or 429, is skipped: mira's very first request is answered by the fallback, and
#     the answer names the fallback's model
# 3 · access: alice (eng) may use the failing Model but not its fallback; she gets 403, and the fallback receives nothing
# 4 · two servers of one Model share the requests
# 5 · the gateway's access log names the Model that answered
# 6 · removing the fallback removes the virtual model; the failing Model then answers with its own 500
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
LLM=http://llm.localtest.me:8080/v1/chat/completions
G=ambitproject.dev
MOCK=http://mock-llm.$NS.svc.cluster.local:8080/v1
MOCK2=http://mock-classifier.$NS.svc.cluster.local:8080/v1  # the same server, a second deployment

cleanup() {
  kubectl -n "$NS" delete "models.$G" fb-primary-500 fb-primary-429 fb-backup fb-shared --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" fb-ml fb-eng --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

# ask TOKEN MODEL: prints the HTTP status; body in $W/llm
ask() {
  command curl -s -o "$W/llm" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" "$LLM"
}
# hits DEPLOYMENT MODEL: requests the mock server in that deployment has seen for the model name
hits() {
  kubectl -n "$NS" exec "deploy/$1" -- python -c 'import urllib.request,json,sys;print(json.load(urllib.request.urlopen("http://localhost:8080/")).get("models",{}).get(sys.argv[1],0))' "$2"
}
# until SECONDS DESCRIPTION CMD...: runs CMD until it succeeds
until_ok() { local n=$1 what=$2; shift 2; for _ in $(seq 1 "$n"); do "$@" && return 0; sleep 1; done; fail "$what"; }

ensure_llm_setup "$NS"
kubectl -n "$NS" rollout status deploy/ambit-controller --timeout=120s >/dev/null
A=$(token alice); M=$(token mira)

# 1 · objects
price='price: { inputPerMillionTokens: "1.00", outputPerMillionTokens: "4.00" }'
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: fb-backup }
spec: { provider: OpenAI, model: mock/backup, baseURL: $MOCK, credentialsRef: { name: ambit-llm-mock }, $price }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: fb-primary-500 }
spec: { provider: OpenAI, model: mock/fail500, baseURL: $MOCK, credentialsRef: { name: ambit-llm-mock }, $price, fallback: [fb-backup] }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: fb-primary-429 }
spec: { provider: OpenAI, model: mock/fail429, baseURL: $MOCK, credentialsRef: { name: ambit-llm-mock }, $price, fallback: [fb-backup] }
---
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: fb-shared }
spec:
  provider: OpenAI
  model: mock/shared
  baseURL: $MOCK
  credentialsRef: { name: ambit-llm-mock }
  $price
  additionalEndpoints:
    - { baseURL: $MOCK2, credentialsRef: { name: ambit-llm-mock } }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: fb-ml }
spec: { groups: [ml-research], models: [fb-backup, fb-primary-500, fb-primary-429, fb-shared] }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: fb-eng }
spec: { groups: [eng], models: [fb-primary-500] }
YAML
for m in fb-backup fb-primary-500 fb-primary-429 fb-shared; do
  kubectl -n "$NS" wait "models.$G" "$m" --for=condition=Ready --timeout=120s >/dev/null \
    || fail "1 $m not Ready: $(kubectl -n "$NS" get "models.$G" "$m" -o jsonpath='{.status.conditions[0].message}')"
done
msg=$(kubectl -n "$NS" get "models.$G" fb-primary-500 -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
[[ "$msg" == *"fb-backup is not open to eng"* ]] || fail "1 the Ready message does not say who loses access on fallback: '$msg'"
pass "1 Models with fallback and extra endpoints accepted; Ready says: $msg"

# 2 · the first request after the provider fails is already answered by the fallback
for model in mock/fail500 mock/fail429 mock/fail500; do
  c=$(ask "$M" "$model")
  [[ "$c" == 200 ]] || fail "2 mira -> $model: $c $(head -c 200 "$W/llm")"
  grep -q '"model":"mock/backup"' "$W/llm" || fail "2 $model was not answered by the fallback: $(head -c 200 "$W/llm")"
done
pass "2 mock/fail500 and mock/fail429 (5xx and 429): mira is answered by mock/backup"

# 3 · alice may use the failing Model but not its fallback: 403, and nothing reaches the fallback
before=$(hits mock-llm mock/backup)
c=$(ask "$A" mock/fail500)
[[ "$c" == 403 ]] || fail "3 alice -> mock/fail500: $c $(head -c 200 "$W/llm") (expected 403: she may not use the fallback)"
[[ $(hits mock-llm mock/backup) == "$before" ]] || fail "3 the fallback received alice's request"
pass "3 alice (eng, not allowed on the fallback) -> 403; mock/backup saw no request"

# 4 · two servers of one Model share the requests
b1=$(hits mock-llm mock/shared); b2=$(hits mock-classifier mock/shared)
for _ in $(seq 1 30); do [[ $(ask "$M" mock/shared) == 200 ]] || fail "4 mock/shared: $(head -c 200 "$W/llm")"; done
d1=$(( $(hits mock-llm mock/shared) - b1 )); d2=$(( $(hits mock-classifier mock/shared) - b2 ))
(( d1 + d2 == 30 && d1 > 0 && d2 > 0 )) || fail "4 30 requests went $d1 to the first server and $d2 to the second"
pass "4 30 requests shared between two servers of mock/shared: $d1 and $d2"

# 5 · the access log names the model that answered
named() { [[ $(ask "$M" mock/fail500) == 200 ]] && kubectl -n "$NS" logs -l gateway.networking.k8s.io/gateway-name=ambit --tail=20 | grep -qF 'gen_ai.request.model=mock/backup'; }
until_ok 20 "5 the gateway log does not name mock/backup as the answering model" named
pass "5 the gateway's access log names mock/backup as the model that answered a request for mock/fail500"

# 6 · removing the fallback removes the virtual model; the Model answers with its own failure
kubectl -n "$NS" get agentgatewaymodel fb-primary-500-failover >/dev/null || fail "6 no virtual model before the change"
kubectl -n "$NS" patch "models.$G" fb-primary-500 --type=json -p '[{"op":"remove","path":"/spec/fallback"}]' >/dev/null
gone() { ! kubectl -n "$NS" get agentgatewaymodel fb-primary-500-failover >/dev/null 2>&1; }
until_ok 60 "6 the virtual model fb-primary-500-failover is still there" gone
plain() { [[ $(ask "$M" mock/fail500) == 500 ]]; }
until_ok 60 "6 mock/fail500 is not answered with its own 500 after the fallback was removed" plain
pass "6 fallback removed: virtual model deleted, mock/fail500 answers with its own 500"

cleanup; trap 'on_exit $?' EXIT
done_step fallback
