#!/usr/bin/env bash
# Step 8 acceptance: budgets (journey A3) and dependency failures (S4). A Budget caps eng at 2 LLM requests
# per person per day, counted in Valkey by envoy's ratelimit service. S4: what happens when the ratelimit
# service, ClickHouse or the identity provider is down — each tested by stopping it.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
GEMMA=google/gemma-4-31b-it:free
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
# outcome TOKEN: "blocked" when the gateway refused on budget, otherwise "passed:<status>".
outcome() {
  local code; code=$(command curl -s -D "$W/h" -o "$W/llm" -w '%{http_code}' --max-time 120 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$GEMMA\",\"max_tokens\":4,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" "$LLM")
  if [[ "$code" == 429 ]] && grep -qi '^x-ratelimit-remaining: 0' "$W/h" && ! grep -q 'Provider returned error' "$W/llm"; then echo blocked; else echo "passed:$code"; fi
}
flush() { kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true; }
restore() {
  kubectl -n "$NS" scale deploy/ambit-ratelimit statefulset/ambit-clickhouse --replicas=1 >/dev/null 2>&1 || true
  kubectl -n "$NS" scale deploy/keycloak --replicas=1 >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" eng --ignore-not-found >/dev/null 2>&1 || true
  flush
}
trap 'rc=$?; restore; on_exit $rc' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" rollout status deploy/ambit-ratelimit deploy/ambit-valkey --timeout=180s >/dev/null
flush
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng, userRequestsPerDay: 2, groupRequestsPerDay: 100 }
YAML
kubectl -n "$NS" wait "budgets.$G" eng --for=condition=Ready --timeout=60s >/dev/null || fail "0 budget not enforced: $(kubectl -n "$NS" get "budgets.$G" eng -o yaml)"
# The ConfigMap reaches the ratelimit pod on the kubelet's next sync (up to about two minutes).
for _ in $(seq 1 60); do
  kubectl -n "$NS" exec deploy/ambit-ratelimit -- cat /data/ratelimit/config/config.yaml 2>/dev/null | grep -q 'value: eng' && break; sleep 3
done
sleep 2
A=$(token alice); M=$(token mira)

# 1 · A3: alice's third request of the day is refused by the gateway; mira (not in eng) is not counted
r1=$(outcome "$A"); r2=$(outcome "$A"); r3=$(outcome "$A")
[[ "$r1" == passed* && "$r2" == passed* && "$r3" == blocked ]] || fail "1 alice: $r1, $r2, $r3 (want passed, passed, blocked)"
m=$(outcome "$M")
[[ "$m" == passed* ]] || fail "1 mira was counted against eng: $m"
pass "1 alice: $r1, $r2, then blocked with 429 (limit 2/day); mira unaffected ($m)"

# 2 · the console shows today's use
sign_in keycloak priya priya-test-pass
[[ $(api GET /api/budgets) == 200 ]] || fail "2 budgets api: $(cat "$W/api")"
used=$(field '[(b["groupUsed"], [(u["email"], u["used"]) for u in b["users"]]) for b in d if b["group"]=="eng"][0]')
[[ "$used" == "(3, [('alice@acme.test', 3)])" ]] || fail "2 budget view: $used"
pass "2 console Budgets: eng used 3 today; alice 3 (over her 2)"

# 3 · S4, FailOpen (default): with the ratelimit service down, requests pass uncounted; back up, the count holds
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=ambit-ratelimit --for=delete --timeout=120s >/dev/null 2>&1 || true
down=$(outcome "$A")
[[ "$down" == passed* ]] || fail "3 FailOpen: alice was refused while the ratelimit service was down ($down)"
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=1 >/dev/null && kubectl -n "$NS" rollout status deploy/ambit-ratelimit --timeout=120s >/dev/null
for _ in $(seq 1 20); do [[ $(outcome "$A") == blocked ]] && break; sleep 2; done
[[ $(outcome "$A") == blocked ]] || fail "3 alice not blocked again after the ratelimit service came back"
pass "3 FailOpen: ratelimit down -> alice passes ($down); back up -> blocked again (counts kept in Valkey)"

# 4 · S4, FailClosed: the same outage refuses requests
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set limits.failureMode=FailClosed --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-controller --timeout=120s >/dev/null
for _ in $(seq 1 30); do
  [[ $(kubectl -n "$NS" get agentgatewaypolicy ambit-llm-budgets -o jsonpath='{.spec.traffic.rateLimit.global.failureMode}') == FailClosed ]] && break; sleep 2
done
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=ambit-ratelimit --for=delete --timeout=120s >/dev/null 2>&1 || true
closed=$(outcome "$M")
[[ "$closed" == passed:5* ]] || fail "4 FailClosed: mira's request with the ratelimit service down: $closed (want a 5xx refusal)"
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=1 >/dev/null && kubectl -n "$NS" rollout status deploy/ambit-ratelimit --timeout=120s >/dev/null
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
pass "4 FailClosed: ratelimit down -> refused ($closed); FailOpen restored"

# 5 · S4, other dependencies: ClickHouse down does not affect traffic; the identity provider down does not
#     affect tokens already issued (the gateway keeps the signing keys)
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" scale statefulset/ambit-clickhouse --replicas=0 >/dev/null
kubectl -n "$NS" wait pod ambit-clickhouse-0 --for=delete --timeout=120s >/dev/null 2>&1 || true
tools "$A"
[[ -n "$TOOLS" ]] || fail "5 MCP failed with ClickHouse down"
kubectl -n "$NS" scale statefulset/ambit-clickhouse --replicas=1 >/dev/null
kubectl -n "$NS" scale deploy/keycloak --replicas=0 >/dev/null
kubectl -n "$NS" wait pod -l app.kubernetes.io/name=keycloak --for=delete --timeout=120s >/dev/null 2>&1 || true
tools "$A"
[[ -n "$TOOLS" ]] || fail "5 alice's existing token stopped working with the identity provider down"
kubectl -n "$NS" scale deploy/keycloak --replicas=1 >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
kubectl -n "$NS" rollout status statefulset/ambit-clickhouse --timeout=300s >/dev/null
pass "5 ClickHouse down: MCP still works (logs are not on the request path); identity provider down: existing tokens still work"
done_step 8
