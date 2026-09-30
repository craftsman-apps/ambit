#!/usr/bin/env bash
# Step 10 acceptance: operations (SRE journeys).
#   S1 three gateway replicas: an open MCP session survives losing two pods; budgets hold on every replica.
#   S2 a rolling update under steady traffic fails no request; budgets still hold afterwards.
#   S3 a shortened soak (SOAK_SECONDS, default 300): memory of every Ambit component stays flat.
#   S5 rotating the gateway's session key: new sessions keep working (open ones are recorded, see below).
#   S6 within minutes, list every running image and find which contain a named package.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
SOAK_SECONDS=${SOAK_SECONDS:-300}
gw_pods() { kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit --field-selector=status.phase=Running -o name; }
outcome() { # blocked (the gateway refused on budget) or passed:<code>
  local code; code=$(command curl -s -D "$W/h" -o "$W/llm" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' "$LLM")
  if [[ "$code" == 429 ]] && grep -qi '^x-ratelimit-remaining: 0' "$W/h" && ! grep -q 'Provider returned error' "$W/llm"; then echo blocked; else echo "passed:$code"; fi
}
mem() { # mem: "name=MiB ..." for Ambit's own pods (metrics-server). New pods have no metrics for a
  # minute, and kubectl top then fails for the whole list, so retry.
  local out
  for _ in $(seq 1 60); do
    out=$(kubectl -n "$NS" top pods --no-headers 2>/dev/null) && break
    out=""; sleep 5
  done
  [[ -n "$out" ]] || fail "S3 metrics-server returned no pod metrics for 5 minutes"
  awk '$1 ~ /^ambit/ {sub("Mi","",$3); print $1"="$3}' <<<"$out"
}
restore() {
  helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" eng --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true
}
trap 'rc=$?; restore; on_exit $rc' EXIT

ensure_llm_setup "$NS"
make -s metrics-server >/dev/null
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set gateway.replicas=3 --wait --timeout 5m >/dev/null
for _ in $(seq 1 60); do [[ $(gw_pods | wc -l | tr -d ' ') == 3 ]] && break; sleep 2; done
[[ $(gw_pods | wc -l | tr -d ' ') == 3 ]] || fail "0 gateway did not scale to 3: $(gw_pods)"
[[ $(kubectl -n "$NS" get pdb -o jsonpath='{.items[*].spec.minAvailable}') == *1* ]] || fail "0 no PodDisruptionBudget for the gateway"
pass "0 gateway at 3 replicas with a PodDisruptionBudget (minAvailable 1)"

# S1 · an open MCP session survives losing two of three gateway pods
A=$(token alice)
mcp_open "$A"
before=$(call time_get_current_time '{"timezone":"UTC"}')
for p in $(gw_pods | head -2); do kubectl -n "$NS" delete "$p" --wait=false >/dev/null; done
sleep 3
after=$(call time_get_current_time '{"timezone":"UTC"}')
[[ "$before" == ok* && "$after" == ok* ]] || fail "S1 session broke when pods went away: before '$before', after '$after'"
kubectl -n "$NS" wait --for=condition=Available deploy -l gateway.networking.k8s.io/gateway-name=ambit --timeout=180s >/dev/null
pass "S1 same MCP session (id ${SID:0:12}…) answered before and after two of three gateway pods were deleted"

# S1 · a budget holds on every replica: once over, every request is refused, whichever pod answers
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng, userRequestsPerDay: 1 }
YAML
for _ in $(seq 1 60); do kubectl -n "$NS" exec deploy/ambit-ratelimit -- cat /data/ratelimit/config/config.yaml 2>/dev/null | grep -q 'value: eng' && break; sleep 3; done
A=$(token alice)
outcome "$A" >/dev/null
blocked=0; for _ in $(seq 1 10); do [[ $(outcome "$A") == blocked ]] && blocked=$((blocked + 1)); done
[[ "$blocked" == 10 ]] || fail "S1 over budget but only $blocked of 10 requests refused across 3 replicas"
pass "S1 over budget: 10 of 10 requests refused across 3 gateway replicas (shared counter)"

# S2 · a rolling update of every Ambit Deployment under steady traffic fails no request
M=$(token mira)
( ok=0; bad=0; end=$((SECONDS + 75))
  while (( SECONDS < end )); do
    c=$(command curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $M" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"s2","version":"1"}}}' "$MCP")
    if [[ "$c" == 200 ]]; then ok=$((ok + 1)); else bad=$((bad + 1)); echo "$c" >>"$W/s2-bad"; fi
    sleep 0.3
  done
  echo "$ok $bad" >"$W/s2" ) &
traffic=$!
sleep 5
kubectl -n "$NS" rollout restart deploy -l gateway.networking.k8s.io/gateway-name=ambit >/dev/null
kubectl -n "$NS" rollout restart deploy/ambit-console deploy/ambit-controller deploy/ambit-guard >/dev/null
kubectl -n "$NS" rollout status deploy -l gateway.networking.k8s.io/gateway-name=ambit --timeout=300s >/dev/null
wait $traffic
read -r ok bad <"$W/s2"
[[ "$bad" == 0 && "$ok" -gt 50 ]] || fail "S2 rolling update: $ok ok, $bad failed ($(sort "$W/s2-bad" | uniq -c | tr '\n' ' '))"
[[ $(outcome "$(token alice)") == blocked ]] || fail "S2 budget not enforced after the update"
pass "S2 rolling restart of gateway, console, controller and guard under traffic: $ok MCP requests, 0 failed; budget still enforced"

# S3 · shortened soak: steady MCP traffic, memory of every component stays flat
kubectl -n "$NS" delete "budgets.$G" eng >/dev/null
mem >/dev/null
# Each request is a well-behaved client: a fresh token (they last 5 minutes), a session, tools/list, then
# DELETE of the session. Sessions left open cost the gateway about 7 KiB each until they expire (measured).
( end=$((SECONDS + SOAK_SECONDS)); while (( SECONDS < end )); do
    tools "$(token mira)" >/dev/null 2>&1 || true
    command curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $TOK" -H "Mcp-Session-Id: $SID" "$MCP" || true
  done ) &
load=$!
sleep 45; base=$(mem)
sleep $((SOAK_SECONDS - 60)); final=$(mem)
wait $load
verdict=$(uv run --no-project python - "$base" "$final" <<'PY'
import sys
b = dict(x.split("=") for x in sys.argv[1].split())
f = dict(x.split("=") for x in sys.argv[2].split())
worst, lines = 0.0, []  # growth of pods that grew by more than 16 MiB (smaller moves are noise)
for pod, v in f.items():  # the same pod before and after (nothing restarts during the soak)
    if not int(b.get(pod, 0)):
        continue
    base = int(b[pod])
    growth = (int(v) - base) / base
    if int(v) - base > 16:
        worst = max(worst, growth)
    lines.append(f"{pod} {base}->{v}Mi")
print(f"{worst:.2f} " + ", ".join(sorted(lines)))
PY
)
growth=${verdict%% *}
uv run --no-project python -c "import sys; sys.exit(0 if float(sys.argv[1]) < 0.5 else 1)" "$growth" || fail "S3 memory grew by ${growth} (over 50% and 16 MiB): ${verdict#* }"
pass "S3 ${SOAK_SECONDS}s soak under steady MCP traffic: worst growth ${growth} (limit 0.50): ${verdict#* }"

# S5 · rotate the gateway's session key (agentgateway keeps it in a Secret): new sessions work at once;
#      sessions opened before the rotation are recorded as the finding (the key encrypts session IDs).
A=$(token alice) # tokens last 5 minutes; the soak outlived the earlier one
mcp_open "$A"; old_sid=$SID
secret=secret/ambit-session-key # created by the agentgateway controller from the AgentgatewayParameters
kubectl -n "$NS" get "$secret" >/dev/null 2>&1 || fail "S5 no gateway session-key Secret $secret"
kubectl -n "$NS" delete "$secret" >/dev/null
kubectl -n "$NS" rollout restart deploy -l gateway.networking.k8s.io/gateway-name=ambit >/dev/null
kubectl -n "$NS" rollout status deploy -l gateway.networking.k8s.io/gateway-name=ambit --timeout=300s >/dev/null
new=$(tools "$A" >/dev/null 2>&1 && echo ok || echo failed)
SID=$old_sid
oldres=$(mcp_send '{"jsonrpc":"2.0","id":9,"method":"tools/list"}' | head -c 80)
[[ "$new" == ok ]] || fail "S5 new sessions fail after rotating the session key"
pass "S5 session key rotated (${secret#secret/} recreated): new sessions work; a session opened before answers '${oldres:-no response}' (clients reconnect)"

# S6 · list every running image, and find which contain a named package
start=$SECONDS
hack/inventory.sh >"$W/images"
n=$(wc -l <"$W/images" | tr -d ' ')
hack/inventory.sh golang.org/x/net >"$W/pkg" 2>/dev/null || true
summary=$(tail -1 "$W/pkg")
[[ "$n" -gt 5 && "$summary" == *"image(s) contain golang.org/x/net"* ]] || fail "S6 inventory: $n images; $(cat "$W/pkg")"
pass "S6 $n running images listed with digests; package search took $((SECONDS - start)) s: $summary"
done_step 10
