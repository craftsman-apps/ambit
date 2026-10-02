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
# idp down|up: the identity provider becomes unreachable (its Service selects no pod) or reachable.
# Keycloak is not stopped: in dev mode a restart creates new signing keys, which a real IdP does not
# do, and the gateway would refuse new tokens until its JWKS cache (5 minutes at least) refreshes.
idp() {
  local sel=keycloak; [[ "$1" == down ]] && sel=nothing
  kubectl -n "$NS" patch svc keycloak -p "{\"spec\":{\"selector\":{\"app.kubernetes.io/name\":\"$sel\"}}}" >/dev/null 2>&1 || true
}
flush() { kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true; }
restore() {
  kubectl -n "$NS" scale deploy/ambit-ratelimit statefulset/ambit-clickhouse --replicas=1 >/dev/null 2>&1 || true
  idp up
  kubectl -n "$NS" delete "budgets.$G" eng --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" ml --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" patch "models.$G" mock-echo --type=merge -p '{"spec":{"price":{"inputPerMillionTokens":"1.00","outputPerMillionTokens":"4.00"}}}' >/dev/null 2>&1 || true
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
wait_rl "$NS" "ambit-llm.group_eng.user: unit=DAY requests_per_unit=2,"
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
idp down
for _ in $(seq 1 30); do # the gateway sees the empty endpoints after a few seconds
  [[ $(command curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://keycloak.localtest.me:8080/realms/acme/.well-known/openid-configuration) != 200 ]] && break
  sleep 2
done
[[ $(command curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://keycloak.localtest.me:8080/realms/acme/.well-known/openid-configuration) != 200 ]] ||
  fail "5 the identity provider is still reachable after 60 seconds"
tools "$A"
[[ -n "$TOOLS" ]] || fail "5 alice's existing token stopped working with the identity provider down"
idp up
kubectl -n "$NS" rollout status statefulset/ambit-clickhouse --timeout=300s >/dev/null
pass "5 ClickHouse down: MCP still works (logs are not on the request path); identity provider down: existing tokens still work"
# 6-11 · dollar budgets (ml-research, mira) with the priced mock model: each answer costs 3 units
#        (12 input and 3 output tokens at $1 / $4 per million = $0.000024, rounded up). 1 unit = $0.00001.
MOCK=http://llm.localtest.me:8080/v1
ask() { # ask TOKEN MODEL [CONTENT] [STREAM] [PATH]: prints the HTTP status; the body is in $W/llm
  command curl -s -o "$W/llm" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"stream\":${4:-false},\"input\":\"x\",\"messages\":[{\"role\":\"user\",\"content\":\"${3:-hi}\"}]}" "$MOCK${5:-/chat/completions}"
}
day=$(( $(date -u +%s) / 86400 * 86400 ))
vget() { kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli GET "$1" 2>/dev/null | tr -d '\r'; }
spent() { vget "ambit-llm_spend_group_ml-research_$day"; }
user_spent() { vget "ambit-llm_spend_group_ml-research_spend_user_mira@acme.test_$day"; }
wait_spent() { for _ in $(seq 1 30); do [[ "$(spent)" == "$1" ]] && return 0; sleep 1; done; fail "$2: spend $(spent), want $1"; }
# set_ml SPEC LOADED: change ml-research's Budget, then wait until envoy ratelimit has loaded LOADED (a line of
# its /rlconfig, e.g. "spend_group_ml-research: unit=DAY requests_per_unit=5,") and the gateway policy is accepted.
set_ml() {
  kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: ml }
spec: $1
YAML
  wait_rl "$NS" "ambit-llm.$2"
  wait_policy
  flush
  fresh
}
# fresh: new tokens for alice and mira; the waits in these checks outlast a Keycloak access token
fresh() { A=$(token alice); M=$(token mira); }
# wait_policy [TEXT]: the budget policy is accepted by agentgateway at its current generation (and its cost
# expression contains TEXT)
wait_policy() {
  for _ in $(seq 1 60); do
    local y cost; y=$(kubectl -n "$NS" get agentgatewaypolicy ambit-llm-budgets -o json 2>/dev/null)
    cost=$(kubectl -n "$NS" get agentgatewaypolicy ambit-llm-budgets -o jsonpath='{.spec.traffic.rateLimit.global.descriptors[*].cost}' 2>/dev/null)
    if [[ -n "$y" && ( -z "${1:-}" || "$cost" == *"$1"* ) ]] && uv run --no-project python -c '
import json,sys; o=json.load(sys.stdin); g=o["metadata"]["generation"]
c=[c for a in o.get("status",{}).get("ancestors",[]) for c in a.get("conditions",[]) if c["type"]=="Accepted"]
sys.exit(0 if c and all(x["status"]=="True" and x.get("observedGeneration",g)==g for x in c) else 1)' <<<"$y"; then
      sleep 3; return 0   # accepted; give the gateway pods a moment to apply it
    fi
    sleep 2
  done
  fail "budget policy not accepted${1:+ with $1}"
}
set_price() { # set_price IN OUT: the mock model's price, and wait for the cost expression to use it
  kubectl -n "$NS" patch "models.$G" mock-echo --type=merge -p "{\"spec\":{\"price\":{\"inputPerMillionTokens\":\"$1\",\"outputPerMillionTokens\":\"$2\"}}}" >/dev/null
}
# 6 · group cap $0.00005 (5 units): totals 3, then 6; the third request is refused
set_ml '{ group: ml-research, groupDollarsPerDay: "0.00005" }' 'spend_group_ml-research: unit=DAY requests_per_unit=5,'
c1=$(ask "$M" mock/echo); wait_spent 3 "6 first answer"
c2=$(ask "$M" mock/echo); wait_spent 6 "6 second answer"
c3=$(ask "$M" mock/echo)
[[ "$c1" == 200 && "$c2" == 200 && "$c3" == 429 ]] || fail "6 dollar cap: $c1 $c2 $c3 (want 200 200 429)"
pass "6 group cap \$0.00005: two answers (3 units each, total 6) pass, the third is refused with 429"

# 7 · exactly at the cap still passes: cap 6 units, after two answers the total is 6 (not above)
set_ml '{ group: ml-research, groupDollarsPerDay: "0.00006" }' 'spend_group_ml-research: unit=DAY requests_per_unit=6,'
ask "$M" mock/echo >/dev/null; wait_spent 3 "7 first"; ask "$M" mock/echo >/dev/null; wait_spent 6 "7 second"
c=$(ask "$M" mock/echo)
[[ "$c" == 200 ]] || fail "7 a request at exactly the cap answered $c (want 200: only above the cap is refused)"
pass "7 at exactly the cap (6 of 6 units) a request still passes; it is refused only above the cap"

# 8 · per-person cap: mira $0.00005; the group cap is high
set_ml '{ group: ml-research, groupDollarsPerDay: "100", userDollarsPerDay: "0.00005" }' 'spend_group_ml-research.spend_user: unit=DAY requests_per_unit=5,'
p1=$(ask "$M" mock/echo); p2=$(ask "$M" mock/echo)
for _ in $(seq 1 30); do [[ "$(user_spent)" == 6 ]] && break; sleep 1; done
p3=$(ask "$M" mock/echo)
[[ "$p1" == 200 && "$p2" == 200 && "$p3" == 429 && "$(user_spent)" == 6 ]] || fail "8 per-person cap: $p1 $p2 $p3, person spend $(user_spent)"
pass "8 per-person cap \$0.00005: mira's third request refused (her spend 6 units)"

# 9 · request and dollar limits together: whichever runs out first. 1 request a day per person, high dollar cap.
set_ml '{ group: ml-research, groupDollarsPerDay: "100", userRequestsPerDay: 1 }' 'group_ml-research.user: unit=DAY requests_per_unit=1,'
r1=$(ask "$M" mock/echo); r2=$(ask "$M" mock/echo)
[[ "$r1" == 200 && "$r2" == 429 ]] || fail "9 request limit with a dollar cap: $r1 $r2 (want 200 429)"
pass "9 request and dollar limits in one Budget: the request limit (1/day) stops mira first"

# 10 · models: unpriced refused to the capped group, allowed to others; a model priced 0 is allowed and costs 0
set_ml '{ group: ml-research, groupDollarsPerDay: "1" }' 'spend_group_ml-research: unit=DAY requests_per_unit=100000,'
u_m=$(ask "$M" mock/unpriced); u_a=$(ask "$A" mock/unpriced); f=$(ask "$M" mock/free)
sleep 3
[[ "$u_m" == 403 && "$u_a" == 200 && "$f" == 200 && "$(spent)" =~ ^0?$ ]] || fail "10 models: unpriced mira $u_m (403), alice $u_a (200); free $f (200), spend '$(spent)' (none)"
pass "10 unpriced model refused to the capped group (403), allowed to others; a model priced 0 allowed and not charged"

fresh
# 11 · not charged: an answer without token counts, a stream cut off before them, embeddings; a normal one is
n1=$(ask "$M" mock/echo NO-USAGE); ask "$M" mock/echo CUT true >/dev/null || true; e=$(ask "$M" mock/echo hi false /embeddings)
[[ "$n1" == 200 && "$e" == 200 ]] || fail "11 uncounted calls failed: no-usage $n1, embeddings $e"
sleep 5
[[ "$(spent)" =~ ^0?$ ]] || fail "11 uncounted calls were charged: $(spent)"   # the check before each answer adds 0, so 0 or no key
ask "$M" mock/echo >/dev/null; wait_spent 3 "11 a normal answer after them"
pass "11 not charged: no token counts, a stream cut off before them, embeddings (documented); a normal answer is (3 units)"

fresh
# 12 · a price change applies to new answers only (10 / 40 per million: 12 + 12 = 24 units an answer)
set_price 10 40; wait_policy '"mock/echo": 10000000u'
ask "$M" mock/echo >/dev/null; wait_spent 27 "12 answer at the new price (3 + 24)"
set_price 1.00 4.00; wait_policy '"mock/echo": 1000000u'
pass "12 price change: earlier spend kept (3), the next answer charged at the new price (24)"

fresh
# 14 · the price changes while a streamed answer is still running (its usage arrives 25 s after it starts):
#      it is charged at the price when it started (3 units), not the new one (24)
s0=$(spent)
( ask "$M" mock/echo SLOW true >"$W/slow" ) &
slow=$!
sleep 2
set_price 10 40; wait_policy '"mock/echo": 10000000u'
wait $slow
for _ in $(seq 1 30); do [[ "$(spent)" != "$s0" ]] && break; sleep 1; done
d=$(( $(spent) - ${s0:-0} ))
set_price 1.00 4.00; wait_policy '"mock/echo": 1000000u'
[[ "$(cat "$W/slow")" == 200 && "$d" == 3 ]] || fail "14 price change during a stream: status $(cat "$W/slow"), charged $d units (want 3)"
pass "14 price changed while a streamed answer ran: charged 3 units, the price when it started"

fresh
# 15 · a stream that turns usage reporting off is still charged for a dollar-capped group: the gateway turns
#      it back on (mira, ml-research). Alice's group has no dollar cap: her request is left as she sent it.
no_usage() { command curl -s -o "$W/$2" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$3\",\"stream\":true,\"stream_options\":{\"include_usage\":false},\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" "$MOCK/chat/completions"; }
s0=$(spent)
cm=$(no_usage "$M" nu-m mock/echo); ca=$(no_usage "$A" nu-a mock/unpriced)
for _ in $(seq 1 30); do [[ "$(spent)" != "$s0" ]] && break; sleep 1; done
d=$(( $(spent) - ${s0:-0} ))
[[ "$cm" == 200 && "$ca" == 200 && "$d" == 3 ]] && grep -q '"usage"' "$W/nu-m" && ! grep -q '"usage"' "$W/nu-a" ||
  fail "15 include_usage false: mira $cm charged $d units (want 200, 3, usage in her stream); alice $ca, usage in hers: $(grep -c '"usage"' "$W/nu-a")"
pass "15 include_usage false: mira's stream reported usage and was charged 3 units; alice's (no dollar cap) left unchanged"

# 13 · parallel requests started under the cap may all finish (documented); once spend is above it, refused
set_ml '{ group: ml-research, groupDollarsPerDay: "0.00005" }' 'spend_group_ml-research: unit=DAY requests_per_unit=5,'
for i in 1 2 3 4 5 6; do ( c=$(ask "$M" mock/echo); echo "$c" >"$W/par$i" ) & done; wait
ok=$(cat "$W"/par* | grep -c '^200$' || true)
for _ in $(seq 1 30); do [[ "$(spent)" -gt 5 ]] 2>/dev/null && break; sleep 1; done
next=$(ask "$M" mock/echo)
[[ "$ok" -ge 1 && "$(spent)" -gt 5 && "$next" == 429 ]] || fail "13 parallel: $ok answered, spend $(spent), next $next"
pass "13 six parallel requests: $ok finished (spend $(spent) units of 5, the documented overshoot); the next is refused"
kubectl -n "$NS" delete "budgets.$G" ml >/dev/null
done_step 8
