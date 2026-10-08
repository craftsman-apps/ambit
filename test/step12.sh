#!/usr/bin/env bash
# Step 12 acceptance: model routing. A ModelRouter "auto" of type Rules picks a Model per request by keywords and
# length, inside each user's access rules (spec 2026-10-05-semantic-routing-design.md, PR 1 parts).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/llm/router.yaml >/dev/null
# Start without a router of type LLM (a previous run may have left smart): ambit-router must be at 0 for check 1.
kubectl -n "$NS" delete -f test/llm/router-llm.yaml --ignore-not-found >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=ambit-router --for=delete --timeout=120s >/dev/null 2>&1 || true
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
[[ "$(answered "$S" "$(msg 'python')" | cut -d' ' -f1)" == 400 ]] && grep -q virtual_model_no_matching_target "$W/r" || fail "4 sam may not use the router → 400 virtual_model_no_matching_target: $(cat "$W/r")"
# a router alice may use whose default she may not (mock-free is ml-research only), and no route matches: 400
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: ambitproject.dev/v1alpha1
kind: ModelRouter
metadata: { name: auto-ml }
spec: { default: mock-free }
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: router-users-ml }
spec: { groups: [eng], models: [auto-ml] }
YAML
for _ in $(seq 1 30); do [[ "$(kubectl -n "$NS" get modelrouters.ambitproject.dev auto-ml -o jsonpath='{.status.conditions[0].reason}')" == DefaultNotAllowed ]] && break; sleep 2; done
reason=$(kubectl -n "$NS" get modelrouters.ambitproject.dev auto-ml -o jsonpath='{.status.conditions[0].reason}')
sleep 5
code=$(answered "$A" '{"model":"auto-ml","messages":[{"role":"user","content":"hi"}]}' | cut -d' ' -f1)
kubectl -n "$NS" delete modelrouters.ambitproject.dev auto-ml >/dev/null; kubectl -n "$NS" delete accessrules.ambitproject.dev router-users-ml >/dev/null
[[ "$code" == 400 && "$reason" == DefaultNotAllowed ]] && grep -q virtual_model_no_matching_target "$W/r" || fail "4 alice, router allowed, default not: $code, reason $reason, $(cat "$W/r")"
pass "4 access: a user who may not use a route's model gets the default; a user who may not use the router, or may use it but not its default, gets 400 virtual_model_no_matching_target (the router reports DefaultNotAllowed)"

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

# ---------- Type LLM: ambit-router asks a classifier (spec rev 5, PR 2 parts 2, 4, 5, 7, 9) ----------
k_minor=$(kubectl version -o json | uv run --no-project python -c 'import json,sys;print(int(json.load(sys.stdin)["serverVersion"]["minor"].rstrip("+")))')
(( k_minor >= 30 )) || fail "0 Kubernetes >= 1.30 is needed (native preStop sleep of ambit-router)"
fresh() { M=$(token mira); A=$(token alice); S=$(token sam); }  # Keycloak tokens last about 5 minutes
# ask MODEL TOKEN TEXT [HEADER...]: prints "STATUS MODEL"; the answer stays in $W/r
ask() {
  local model=$1 tok=$2 text=$3; shift 3
  local -a h=(); local x; for x in "$@"; do h+=(-H "$x"); done
  local b; b=$(uv run --no-project python -c 'import json,sys;print(json.dumps({"model":sys.argv[1],"messages":[{"role":"user","content":sys.argv[2]}]}))' "$model" "$text")
  local code; code=$(command curl -s -o "$W/r" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' ${h[@]+"${h[@]}"} -d "$b" "$LLM/chat/completions")
  echo "$code $(uv run --no-project python -c "import json;print(json.load(open('$W/r')).get('model',''))" 2>/dev/null)"
}
mockstat() { kubectl -n "$NS" exec deploy/mock-classifier -- python -c 'import urllib.request;print(urllib.request.urlopen("http://localhost:8080/").read().decode())'; }
classified() { mockstat | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["classify"])'; }
hash_now() { kubectl -n "$NS" get configmap ambit-router-config -o jsonpath='{.data.config\.json}' | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["hash"])'; }
state() { kubectl -n "$NS" get modelrouters.ambitproject.dev smart -o jsonpath='{.status.classifierState}'; }
replicas() { kubectl -n "$NS" get deploy ambit-router -o jsonpath='{.spec.replicas}'; }
policy() { kubectl -n "$NS" get agentgatewaypolicies.agentgateway.dev ambit-llm-classify -o name 2>/dev/null; }
wait_until() { # wait_until SECONDS CONDITION: polls every 0.5 s; prints the seconds it took
  local t0=$SECONDS i; for i in $(seq 1 $(($1 * 2))); do eval "$2" && { echo $((SECONDS - t0)); return 0; }; sleep 0.5; done; return 1
}
# pod_config POD: the configuration hash an ambit-router pod has loaded (its /config), through a port-forward
pod_config() {
  local port=$((18100 + RANDOM % 800)) pf
  kubectl -n "$NS" port-forward "pod/$1" "$port:8081" >/dev/null 2>&1 & pf=$!
  for _ in $(seq 1 20); do command curl -s "localhost:$port/config" 2>/dev/null && break; sleep 0.2; done
  kill $pf 2>/dev/null; wait $pf 2>/dev/null || true
}
router_pods() { kubectl -n "$NS" get pods -l app.kubernetes.io/name=ambit-router --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'; }
coding='please fix this code'
# argo_check: a real Argo CD sync of ambit-router's Deployment, with the documented ignoreDifferences rule, leaves
# the replicas to the controller (2 while a type-LLM router exists, then 0). Needs the GitHub App of step 4c; set
# AMBIT_SKIP_ARGO=1 to skip (CI has no App). The manifest goes to its own branch, router-check, of the test
# repository; pushes are fast-forward only.
argo_check() {
  if [[ "${AMBIT_SKIP_ARGO:-}" == 1 ]]; then echo "  SKIP Argo CD check (AMBIT_SKIP_ARGO=1)"; return; fi
  local repo=craftsman-apps/ambit-config-test key=${AMBIT_GITHUB_APP_KEY:-$HOME/.config/ambit/github-app.pem} id tok inst
  id=${AMBIT_GITHUB_APP_ID:-$(cat "$HOME/.config/ambit/github-app-id" 2>/dev/null || true)}
  [[ -n "$id" && -s "$key" ]] || fail "7 the Argo CD check needs the GitHub App of step 4c (or AMBIT_SKIP_ARGO=1)"
  gapp() { uv run --no-project --with 'pyjwt[crypto]' python test/gitops/github_app.py "$id" "$key" "$repo" "$@"; }
  make -s argocd >/dev/null
  for _ in 1 2 3; do tok=$(gapp 2>/dev/null || true); [[ "$tok" == ghs_* ]] && break; sleep 3; done
  [[ "$tok" == ghs_* ]] || fail "7 no GitHub App installation token"
  inst=$(gapp --installation-id)
  kubectl -n argocd create secret generic ambit-config-repo --dry-run=client -o yaml \
    --from-literal=type=git --from-literal=url="https://github.com/$repo" \
    --from-literal=githubAppID="$id" --from-literal=githubAppInstallationID="$inst" \
    --from-file=githubAppPrivateKey="$key" | kubectl label --local -f - argocd.argoproj.io/secret-type=repository -o yaml | kubectl apply -f - >/dev/null
  # the Deployment exactly as the installed release renders it, so Argo CD and Helm agree on the pod template
  helm get values ambit -n "$NS" -o yaml > "$W/release-values.yaml"
  rm -rf "$W/rc"
  git clone -q -b router-check "https://x-access-token:$tok@github.com/$repo.git" "$W/rc" 2>/dev/null || { mkdir -p "$W/rc" && git -C "$W/rc" init -q -b router-check; }
  mkdir -p "$W/rc/router-check"
  command helm template ambit charts/ambit -n "$NS" -f "$W/release-values.yaml" -s templates/router.yaml |
    uv run --no-project --with pyyaml python -c 'import sys,yaml;d=[x for x in yaml.safe_load_all(sys.stdin) if x and x["kind"]=="Deployment"][0];d["metadata"]["namespace"]=sys.argv[1];print(yaml.safe_dump(d))' "$NS" > "$W/rc/router-check/deployment.yaml"
  ( cd "$W/rc" && git add -A && { git diff --cached --quiet || git -c user.name=ambit-test -c user.email=ambit-test@users.noreply.github.com commit -qm "ambit-router Deployment for the step 12 Argo CD check"; } &&
    git push -q "https://x-access-token:$tok@github.com/$repo.git" router-check ) || fail "7 push router-check"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: { name: ambit-router-check, namespace: argocd }
spec:
  project: default
  source: { repoURL: "https://github.com/$repo", targetRevision: router-check, path: router-check }
  destination: { server: https://kubernetes.default.svc, namespace: $NS }
  ignoreDifferences:
    - { group: apps, kind: Deployment, name: ambit-router, jsonPointers: [/spec/replicas] }
  syncPolicy:
    automated: { selfHeal: true, prune: false }
    syncOptions: [RespectIgnoreDifferences=true]
YAML
  argo_drop() { kubectl -n argocd patch application ambit-router-check --type json -p '[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null 2>&1 || true
    kubectl -n argocd delete application ambit-router-check --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
  trap 'rc=$?; argo_drop; on_exit $rc' EXIT
  sync() { kubectl -n argocd get application ambit-router-check -o jsonpath='{.status.sync.status}' 2>/dev/null; }
  wait_until 300 '[[ $(sync) == Synced ]]' >/dev/null || { argo_drop; fail "7 Argo CD: ambit-router-check not Synced: $(kubectl -n argocd get application ambit-router-check -o jsonpath='{.status.conditions}')"; }
  for want in 2 0; do
    [[ $want == 0 ]] && kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type merge -p '{"spec":{"classifier":{"type":"Rules"}}}' >/dev/null
    wait_until 120 '[[ $(replicas) == '"$want"' ]]' >/dev/null || { argo_drop; fail "7 Argo CD: replicas never reached $want"; }
    kubectl -n argocd annotate application ambit-router-check argocd.argoproj.io/refresh=hard --overwrite >/dev/null
    for _ in $(seq 1 18); do
      sleep 5
      [[ $(replicas) == "$want" && $(sync) == Synced ]] || { argo_drop; fail "7 Argo CD changed the replicas (want $want, have $(replicas)) or went $(sync)"; }
    done
  done
  argo_drop
  trap 'on_exit $?' EXIT
  kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
  wait_until 180 '[[ $(state) == Ready ]]' >/dev/null || fail "7 back to LLM after the Argo CD check"
  echo "  Argo CD: synced; replicas stayed 2, then 0, for 90 s each"
}

# 2 · LLM
fresh
kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
secs=$(wait_until 180 '[[ $(state) == Ready ]]') || fail "2 smart not Ready: $(kubectl -n "$NS" get modelrouters.ambitproject.dev smart -o jsonpath='{.status}')"
echo "  classifier Off → Ready: $secs s"
wait_until 20 '[[ "$(ask smart "$M" "$coding")" == "200 mock/echo" ]]' >/dev/null || fail "2 coding prompt → mock-echo: $(cat "$W/r")"
grep -q 'x-ambit-route=coding' "$W/r" && grep -q "x-ambit-config=$(hash_now)" "$W/r" || fail "2 the target must see the label and its hash: $(cat "$W/r")"
row=""
for _ in $(seq 1 30); do
  row=$(kubectl -n "$NS" exec statefulset/ambit-clickhouse -- clickhouse-client -q "SELECT LogAttributes['ambit.router'], LogAttributes['ambit.route'], LogAttributes['gen_ai.request.model'] FROM otel.otel_logs WHERE LogAttributes['ambit.router'] = 'smart' AND LogAttributes['ambit.route'] = 'coding' LIMIT 1")
  [[ -n "$row" ]] && break; sleep 2
done
[[ "$row" == $'smart\tcoding\tmock/echo' ]] || fail "2 audit row: '$row'"
[[ "$(ask smart "$M" CLASSIFY-EXTRA)" == "200 mock/unpriced" ]] || fail "2 an answer with extra text is no label: $(cat "$W/r")"
kubectl -n "$NS" scale deploy/mock-classifier --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app=mock-classifier --for=delete --timeout=60s >/dev/null 2>&1 || true
t0=$(date +%s)
[[ "$(ask smart "$M" "$coding")" == "200 mock/unpriced" ]] && (( $(date +%s) - t0 <= 3 )) || fail "2 classifier down → default within 3 s: $(cat "$W/r")"
kubectl -n "$NS" scale deploy/mock-classifier --replicas=1 >/dev/null; kubectl -n "$NS" rollout status deploy/mock-classifier --timeout=120s >/dev/null
t0=$(date +%s)
[[ "$(ask smart "$M" CLASSIFY-SLOW)" == "200 mock/unpriced" ]] && (( $(date +%s) - t0 <= 3 )) || fail "2 slow classifier → default after the 1.5 s timeout: $(cat "$W/r")"
pass "2 LLM: coding → mock-echo with label and hash, audited (coding); extra text, classifier down and slow → default, no 5xx"

# 4 · access
fresh
[[ "$(ask smart "$A" "$coding")" == "200 mock/unpriced" ]] || fail "4 alice may not use mock-echo → default: $(cat "$W/r")"
before=$(classified)
[[ "$(ask smart "$S" "$coding" | cut -d' ' -f1)" == 400 ]] || fail "4 sam may not use smart → 400: $(cat "$W/r")"
[[ "$(classified)" == "$before" ]] || fail "4 a user not allowed the router caused a classifier call"
pass "4 access (LLM): a user not allowed the target gets the default; a user not allowed the router gets 400 and causes no classifier call"

# 5 · spoofed label and hash
[[ "$(ask smart "$M" hello "x-ambit-route: coding" "x-ambit-config: $(hash_now)")" == "200 mock/unpriced" ]] && ! grep -q 'x-ambit-route=' "$W/r" || fail "5 a client-sent label and hash must not route: $(cat "$W/r")"
pass "5 a client-sent x-ambit-route and x-ambit-config are removed at the Gateway"

# 7 · switching, configuration change, rollout and scale-down under load, key rotation, Argo CD
fresh
kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type merge -p '{"spec":{"classifier":{"type":"Rules"}}}' >/dev/null
secs=$(wait_until 120 '[[ $(replicas) == 0 && -z "$(policy)" ]]') || fail "7 type Rules: ambit-router not at 0 or policy left: $(replicas) $(policy)"
echo "  LLM → Rules: policy removed and scaled to 0 in $secs s"
kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
secs=$(wait_until 180 '[[ $(state) == Ready ]]') || fail "7 back to LLM: not Ready"
echo "  Rules → LLM: Ready in $secs s"
wait_until 20 '[[ "$(ask smart "$M" "$coding")" == "200 mock/echo" ]]' >/dev/null || fail "7 not routing after Ready"
LT=$W/load-token
uv run --no-project python -c 'import json;print(json.dumps({"model":"smart","user":"load","messages":[{"role":"user","content":"please fix this code"}]}))' > "$W/b-smart.json"
uv run --no-project python -c 'import json;print(json.dumps({"model":"mock/unpriced","user":"load","messages":[{"role":"user","content":"hello"}]}))' > "$W/b-direct.json"
# built once: `go run` compiles first, and on a slow runner the load then started after the change below
go build -o "$W/load" ./test/load || fail "7 build the load tool"
load() { "$W/load" -rate 20 -duration "$1" -warmup 0s -token-file "$LT" -body-file "$W/b-smart.json" -body-file2 "$W/b-direct.json" -answers-file "$2"; }
# a) one edit changes the category's meaning (description → new hash) and its target (lines): a label made under
#    H1 may only reach H1's target, one made under H2 only H2's target
fresh; echo "$M" > "$LT"
load 60s "$W/ans-config" > "$W/load-config" 2>&1 & lp=$!
sleep 20
H1=$(hash_now)
kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type merge -p '{"spec":{"classifier":{"categories":[{"name":"coding","description":"program code, version 2"}]},"routes":[{"model":"mock-free","category":"coding"}]}}' >/dev/null
t0=$(date +%s.%N)
wait_until 60 '[[ $(hash_now) != "$H1" ]]' >/dev/null || fail "7 configuration hash did not change"
H2=$(hash_now)
for p in $(router_pods); do wait_until 60 '[[ "$(pod_config '"$p"')" == "$H2" ]]' >/dev/null || fail "7 pod $p did not load $H2"; done
echo "  watch latency (ConfigMap change → every ambit-router pod has it): $(uv run --no-project python -c "import time;print(round(time.time()-$t0,1))") s (upper bound, includes port-forwards)"
wait $lp || fail "7 load run: $(cat "$W/load-config")"
uv run --no-project python - "$W/ans-config" "$H1" "$H2" <<'PY' || fail "7 configuration change under load: $(tail -3 "$W/ans-config")"
import json, sys
f, h1, h2 = sys.argv[1:4]
rows = [json.loads(l) for l in open(f)]
bad = [r for r in rows if r["status"] != 200]
assert not bad, f"{len(bad)} non-200 answers, first {bad[0]}"
for r in rows:
    m, c = r["model"], r["content"]
    assert m in ("mock/echo", "mock/free", "mock/unpriced"), r
    if m == "mock/echo": assert f"x-ambit-config={h1}" in c, f"old target with another hash: {r}"
    if m == "mock/free": assert f"x-ambit-config={h2}" in c, f"new target with another hash: {r}"
n = {m: sum(1 for r in rows if r["model"] == m) for m in ("mock/echo", "mock/free", "mock/unpriced")}
assert n["mock/echo"] and n["mock/free"], n
print("  answers:", len(rows), n)
PY
kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type json -p '[{"op":"replace","path":"/spec/routes","value":[{"model":"mock-echo","category":"coding"}]}]' >/dev/null
# b) a rollout (Helm upgrade) and a scale-down under load: no 5xx; Helm leaves the replicas alone (2, then 0)
wait_until 60 '[[ "$(ask smart "$M" "$coding")" == "200 mock/echo" ]]' >/dev/null || fail "7 not routing before the rollout"
fresh; echo "$M" > "$LT"
load 300s "$W/ans-roll" > "$W/load-roll" 2>&1 & lp=$!  # stopped 15 s after the last event below
sleep 10
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set-string "routing.router.podAnnotations.restart=$(date +%s)" --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-router --timeout=180s >/dev/null
[[ $(replicas) == 2 ]] || fail "7 a Helm upgrade changed ambit-router's replicas to $(replicas)"
kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type merge -p '{"spec":{"classifier":{"type":"Rules"}}}' >/dev/null
wait_until 120 '[[ $(replicas) == 0 ]]' >/dev/null || fail "7 not scaled down"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set-string "routing.router.podAnnotations.restart=$(date +%s)" --wait --timeout 5m >/dev/null
[[ $(replicas) == 0 ]] || fail "7 a Helm upgrade changed ambit-router's replicas from 0 to $(replicas)"
sleep 15; kill -TERM $lp 2>/dev/null  # the load tool stops sending, lets requests in flight finish, writes its results
wait $lp || fail "7 load run: $(cat "$W/load-roll")"
uv run --no-project python -c "
import json,sys
rows=[json.loads(l) for l in open('$W/ans-roll')]
bad=[r for r in rows if r['status']!=200]
assert not bad, f'{len(bad)} of {len(rows)} answers not 200, first {bad[0]}'
print('  rollout and scale-down under load:', len(rows), 'answers, all 200')" || fail "7 rollout or scale-down under load"
kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
wait_until 180 '[[ $(state) == Ready ]]' >/dev/null || fail "7 back to LLM after the scale-down"
# c) a rotated classifier key reaches the first classification that uses it
fresh
new="rotated-$(date +%s)"
kubectl -n "$NS" create secret generic ambit-llm-mock --from-literal=Authorization="$new" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
t0=$SECONDS
wait_until 150 '{ ask smart "$M" "$coding" >/dev/null; [[ "$(mockstat)" == *"Bearer $new"* ]]; }' >/dev/null || fail "7 the rotated key was not used within 150 s"
echo "  key rotation → first classification with the new key: $((SECONDS - t0)) s"
kubectl -n "$NS" create secret generic ambit-llm-mock --from-literal=Authorization=not-used --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fresh
argo_check
pass "7 switching: Rules ↔ LLM; a configuration change under load never reached a target of another configuration; rollout and scale-down under load without 5xx; Helm and Argo CD leave the replicas alone; key rotation without restart"

# 9 · ambit-router faults: one replica, so the pod holding a request is known. A 10 s classifier timeout keeps the
#     request inside ambit-router while it is paused or killed (the slow classification takes 3 s).
fresh
H0=$(hash_now)
kubectl -n "$NS" patch modelrouters.ambitproject.dev smart --type merge -p '{"spec":{"classifier":{"timeout":"10s"}}}' >/dev/null
wait_until 60 '[[ $(hash_now) != "$H0" ]]' >/dev/null || fail "9 timeout change not published"
# signal SIG: to ambit-router's process in $POD, sent from the kind node. Inside the pod ambit-router is PID 1, and
# Linux ignores SIGSTOP and SIGKILL sent to a namespace's PID 1 from inside it (found 2026-10-07: a debug container
# could not stop or kill it).
signal() {
  local cid; cid=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].containerID}'); cid=${cid#containerd://}
  docker exec "${KIND_CLUSTER:-ambit}-control-plane" sh -c "kill -$1 \$(crictl inspect --output go-template --template '{{.info.pid}}' $cid)"
}
restore_faults() {
  [[ -n "${POD:-}" ]] && signal CONT >/dev/null 2>&1 || true
  kubectl -n "$NS" scale deploy/ambit-controller --replicas=1 >/dev/null
}
trap 'rc=$?; restore_faults; on_exit $rc' EXIT
kubectl -n "$NS" scale deploy/ambit-controller --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=ambit-controller --for=delete --timeout=60s >/dev/null 2>&1 || true
kubectl -n "$NS" scale deploy/ambit-router --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=ambit-router --for=delete --timeout=90s >/dev/null 2>&1 || true
[[ "$(ask smart "$M" "$coding")" == "200 mock/unpriced" ]] || fail "9 no ambit-router pod → FailOpen, default: $(cat "$W/r")"
kubectl -n "$NS" scale deploy/ambit-router --replicas=1 >/dev/null; kubectl -n "$NS" rollout status deploy/ambit-router --timeout=120s >/dev/null
POD=$(router_pods)
wait_until 30 '[[ "$(ask smart "$M" "$coding")" == "200 mock/echo" ]]' >/dev/null || fail "9 one replica not routing"
# paused while it holds a request: the request waits, then completes when the process continues
c0=$(classified)
( ask smart "$M" CLASSIFY-SLOW > "$W/paused" ) & rp=$!
wait_until 10 '(( $(classified) > c0 ))' >/dev/null || fail "9 the request did not reach the classifier"
signal STOP
sleep 5 # longer than the 3 s classification: only a paused ambit-router keeps the request open this long
kill -0 $rp 2>/dev/null || fail "9 the request finished while ambit-router was paused: the signal did not arrive ($(cat "$W/paused"))"
signal CONT
wait $rp
[[ "$(cut -d' ' -f1 "$W/paused")" == 200 ]] || fail "9 paused ambit-router: the request must complete after it continues: $(cat "$W/paused")"
# killed (the process crashes) while it holds a request: that request fails, the next ones do not
restarts=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
c0=$(classified)
( ask smart "$M" CLASSIFY-SLOW > "$W/killed" ) & rp=$!
wait_until 10 '(( $(classified) > c0 ))' >/dev/null || fail "9 the request did not reach the classifier"
signal KILL
wait $rp
wait_until 60 '[[ $(kubectl -n "$NS" get pod "$POD" -o jsonpath="{.status.containerStatuses[0].restartCount}") -gt $restarts ]]' >/dev/null || fail "9 the kill did not reach ambit-router (no restart)"
[[ "$(cut -d' ' -f1 "$W/killed")" == 500 ]] || fail "9 a request held by a killed ambit-router fails with 500 (fact 2), got $(cat "$W/killed")"
wait_until 60 '[[ $(kubectl -n "$NS" get pod "$POD" -o jsonpath="{.status.containerStatuses[0].ready}") == true ]]' >/dev/null || fail "9 ambit-router did not restart"
wait_until 30 '[[ "$(ask smart "$M" "$coding" | cut -d" " -f1)" == 200 ]]' >/dev/null || fail "9 the next request after the kill"
restore_faults
trap 'on_exit $?' EXIT
kubectl -n "$NS" apply -f test/llm/router-llm.yaml >/dev/null
wait_until 120 '[[ $(replicas) == 2 ]]' >/dev/null || fail "9 the controller did not restore 2 replicas"
pass "9 ambit-router faults: none running → FailOpen; paused → the request waits and completes; killed → that request 500, the next 200"
done_step 12
