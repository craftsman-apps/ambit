#!/usr/bin/env bash
# The quick start (docs: Quick start), on a fresh kind cluster with its own kubeconfig: no CoreDNS rewrite
# (kind/coredns-rewrite.sh), no Keycloak. Install with deploy/quickstart.yaml, sign in as the bootstrap admin,
# get a token from Ambit's Dex, call a model, and find the call in the audit log.
#   test/quickstart.sh                      # CI: a cluster on host port 8080
#   QS_PORT=18080 test/quickstart.sh        # beside the shared ambit cluster, which holds 8080
# Needs the image ambit-console:dev (make image builds it; QS_IMAGE names another). The cluster is deleted at the end (QS_KEEP=1 keeps it).
set -euo pipefail
cd "$(dirname "$0")/.."
NS=ambit-system CL=${QS_CLUSTER:-ambit-quickstart} PORT=${QS_PORT:-8080} IMG=${QS_IMAGE:-ambit-console:dev}
export BASE=http://console.localtest.me:$PORT
source test/lib.sh
LLM=http://llm.localtest.me:$PORT
export KUBECONFIG=$W/kubeconfig   # never the shared cluster's context
qs_exit() {
  local rc=$?
  [[ -n "${QS_KEEP:-}" ]] || kind delete cluster --name "$CL" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
  on_exit "$rc"
}
trap qs_exit EXIT

# 1 · cluster from kind/cluster.yaml (the file the release ships as kind-cluster.yaml), only name and port changed
uv run --no-project --with pyyaml python - "$CL" "$PORT" > "$W/cluster.yaml" <<'EOF'
import sys, yaml
c = yaml.safe_load(open("kind/cluster.yaml"))
c["name"] = sys.argv[1]
m = c["nodes"][0]["extraPortMappings"]
c["nodes"][0]["extraPortMappings"] = [dict(p, hostPort=int(sys.argv[2])) for p in m if p["containerPort"] == 30080]
print(yaml.safe_dump(c))
EOF
kind create cluster --config "$W/cluster.yaml" --kubeconfig "$KUBECONFIG" --wait 120s >/dev/null
kind load docker-image "$IMG" --name "$CL" >/dev/null
[[ -z "${QS_PRELOAD:-}" ]] || kind load image-archive "$QS_PRELOAD" --name "$CL" >/dev/null  # CI: cached third-party images
! kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' | grep -q localtest || fail "1 CoreDNS has a localtest rewrite"
pass "1 kind cluster without DNS rewrite"

# 2 · install: dependency CRDs (released as crds.yaml), then the two charts with the quick-start values
kubectl apply --server-side -f deploy/crds/ >/dev/null
command helm install ambit-crds charts/ambit-crds -n "$NS" --create-namespace --wait >/dev/null
command helm install ambit charts/ambit -n "$NS" -f deploy/quickstart.yaml --set console.url="$BASE" \
  --set console.image.repository="${IMG%:*}" --set console.image.tag="${IMG##*:}" --set console.image.pullPolicy=Never \
  --wait --timeout 10m >/dev/null
pass "2 installed with deploy/quickstart.yaml"

# 3 · the bootstrap admin signs in: the console reached Dex inside the cluster
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW"
[[ $(me_status) == 200 ]] && grep -q '"role":"admin"' "$W/me" || fail "3 sign-in: $(cat "$W/me")"
pass "3 signed in as the bootstrap admin"

# 4 · a token from Dex (public client ambit-cli, password grant)
TOK=$(command curl -s "$BASE/dex/token" -d grant_type=password -d client_id=ambit-cli -d scope="openid email profile" \
  --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" \
  | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])') || fail "4 no token from Dex"
pass "4 token from Dex"

# 5 · a model, then the first call: denied until an access rule allows it (the gateway accepted the token)
kubectl -n "$NS" create secret generic ambit-llm-mock --from-literal=Authorization=not-used >/dev/null
kubectl -n "$NS" apply -f test/llm/mock.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/mock-llm --timeout=180s >/dev/null
kubectl -n "$NS" wait models.ambitproject.dev/mock-echo --for=condition=Ready --timeout=120s >/dev/null
chat() { command curl -s -o "$W/llm" -w '%{http_code}' "$LLM/v1/chat/completions" -H 'Content-Type: application/json' "$@" \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"hello"}]}'; }
[[ $(chat) =~ ^40[13]$ ]] || fail "5 a call without a token was not refused: $(cat "$W/llm")"
# Ready means the controller generated the model; the gateway answers 404 until its configuration arrives.
for _ in $(seq 1 30); do c=$(chat -H "Authorization: Bearer $TOK"); [[ "$c" != 404 ]] && break; sleep 2; done
[[ "$c" == 403 ]] || fail "5 with a Dex token and no rule: got $c, want 403: $(cat "$W/llm")"
kubectl -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: everyone-mock }
spec: { allSignedIn: true, models: [mock-echo] }
EOF
for _ in $(seq 1 30); do c=$(chat -H "Authorization: Bearer $TOK"); [[ "$c" == 200 ]] && break; sleep 2; done
[[ "$c" == 200 ]] || fail "5 allowed call: $c $(cat "$W/llm")"
pass "5 first call: 401 or 403 without a token, 403 without a rule, 200 with one"

# 6 · the call is in the audit log (Usage and Audit read it)
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
for _ in $(seq 1 30); do
  [[ $(api GET "/api/audit?email=admin@ambit.local&from=$day&to=$next") == 200 ]] && grep -q 'mock/echo' "$W/api" && break
  sleep 3
done
grep -q 'mock/echo' "$W/api" || fail "6 audit has no record of the call: $(head -c 400 "$W/api")"
pass "6 the call is in the audit log"
done_step quickstart
