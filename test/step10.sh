#!/usr/bin/env bash
# Step 10 acceptance: operations (SRE journeys).
#   S3 every container in the namespace has a memory limit (chart values, or the LimitRange default).
#   S1 three gateway replicas: an open MCP session survives losing two pods; budgets hold on every replica.
#   S2 a rolling update under steady traffic fails no request; budgets still hold afterwards.
#   S3 a shortened soak (SOAK_SECONDS, default 300): no restarts, no refused ClickHouse writes, memory flat.
#   S5 rotating the gateway's session key: new sessions keep working (open ones are recorded, see below).
#   S6 within minutes, list every running image and find which contain a named package.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
SOAK_SECONDS=${SOAK_SECONDS:-300}
# First memory sample after this many seconds. The soak's MCP sessions are never closed, like a careless
# client; the gateway drops them after 30 idle minutes, so a 24-hour soak samples from 3600 s on:
#   SOAK_SECONDS=86400 SOAK_WARMUP=3600 test/step10.sh
SOAK_WARMUP=${SOAK_WARMUP:-45}
# Running gateway pods that are not shutting down: a pod replaced by a rollout stays Running while it terminates.
gw_pods() { kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit -o go-template='{{range .items}}{{if and (eq .status.phase "Running") (not .metadata.deletionTimestamp)}}pod/{{.metadata.name}}{{"\n"}}{{end}}{{end}}'; }
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
# rollout TARGET...: waits for a rollout; on timeout prints what the pods and events say, then fails.
rollout() {
  kubectl -n "$NS" rollout status "$@" --timeout=300s >/dev/null && return 0
  kubectl -n "$NS" get pods -o wide >&2
  kubectl -n "$NS" get rs -l gateway.networking.k8s.io/gateway-name=ambit >&2
  kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -25 >&2
  fail "rollout of $* did not finish in 5 minutes (pods and events above)"
}
restore() {
  helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" eng soak --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true
}
trap 'rc=$?; restore; on_exit $rc' EXIT

ensure_llm_setup "$NS"
make -s metrics-server >/dev/null
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
mcp_ready "$NS"
# A 4-CPU CI node is nearly full at this point; a small CPU request lets the rollout's extra pod fit.
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set gateway.replicas=3 \
  --set gateway.resources.requests.cpu=20m --wait --timeout 5m >/dev/null
# The new CPU request changes the pod template, so the gateway rolls: wait until all 3 replicas are updated and
# ready, then for the replaced pods to be gone.
for _ in $(seq 1 90); do
  [[ $(kubectl -n "$NS" get deploy -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].status.updatedReplicas}/{.items[0].status.readyReplicas}/{.items[0].status.replicas}') == 3/3/3 ]] && break; sleep 2
done
for _ in $(seq 1 60); do [[ $(gw_pods | wc -l | tr -d ' ') == 3 ]] && break; sleep 2; done
[[ $(gw_pods | wc -l | tr -d ' ') == 3 ]] || fail "0 gateway did not scale to 3: $(gw_pods)"
[[ $(kubectl -n "$NS" get pdb -o jsonpath='{.items[*].spec.minAvailable}') == *1* ]] || fail "0 no PodDisruptionBudget for the gateway"
nolimit=$(kubectl -n "$NS" get pods --field-selector=status.phase=Running -o json | uv run --no-project python -c '
import json, sys
print(" ".join(p["metadata"]["name"] + "/" + c["name"] for p in json.load(sys.stdin)["items"]
               for c in p["spec"]["containers"] if not c.get("resources", {}).get("limits", {}).get("memory")))')
[[ -z "$nolimit" ]] || fail "0 containers without a memory limit: $nolimit"
pass "0 gateway at 3 replicas with a PodDisruptionBudget (minAvailable 1); every container has a memory limit"

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
# The ConfigMap reaches the ratelimit pod on the kubelet's next sync (up to about two minutes):
# wait until the gateway refuses alice (her first request uses her 1 request a day).
A=$(token alice)
for _ in $(seq 1 60); do [[ $(outcome "$A") == blocked ]] && break; sleep 3; done
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
rollout deploy -l gateway.networking.k8s.io/gateway-name=ambit
wait $traffic
read -r ok bad <"$W/s2"
[[ "$bad" == 0 && "$ok" -gt 50 ]] || fail "S2 rolling update: $ok ok, $bad failed ($(sort "$W/s2-bad" | uniq -c | tr '\n' ' '))"
[[ $(outcome "$(token alice)") == blocked ]] || fail "S2 budget not enforced after the update"
pass "S2 rolling restart of gateway, console, controller and guard under traffic: $ok MCP requests, 0 failed; budget still enforced"

# S3 · soak: steady MCP and LLM traffic. No Ambit container restarts, ClickHouse accepts every write, and
#      memory stays flat: per pod, the average of the last quarter of samples against the first quarter.
#      (Two single readings are not enough: the collector's memory swings by 100 MiB between them.)
kubectl -n "$NS" delete "budgets.$G" eng >/dev/null
# A dollar budget, so each LLM answer also runs the cost expression and the spend counter.
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: soak }
spec: { group: ml-research, groupDollarsPerDay: "1000" }
YAML
SOAK_SAMPLE=${SOAK_SAMPLE:-$(( SOAK_SECONDS >= 7200 ? 300 : 15 ))}
restarts() { # "pod=restarts ..." for Ambit's own pods, all containers summed
  kubectl -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{range .status.containerStatuses[*]} {.restartCount}{end}{"\n"}{end}' |
    awk '$1 ~ /^ambit/ {s=0; for (i=2; i<=NF; i++) s+=$i; printf "%s=%d ", $1, s}'
}
ch_failed_inserts() { # inserts ClickHouse refused since $1 (epoch seconds)
  kubectl -n "$NS" exec ambit-clickhouse-0 -- clickhouse-client -q \
    "SELECT count() FROM system.query_log WHERE type IN ('ExceptionBeforeStart','ExceptionWhileProcessing') AND query_kind IN ('Insert','AsyncInsertFlush') AND event_time >= toDateTime($1)" | tr -d '\r'
}
mem >/dev/null
r0=$(restarts); t0=$(date +%s)
# Each round is a well-behaved MCP client (a fresh token, a session, tools/list, then DELETE of the session;
# sessions left open cost the gateway about 7 KiB each until they expire) and one LLM answer from the mock.
( end=$((SECONDS + SOAK_SECONDS)); while (( SECONDS < end )); do
    tools "$(token mira)" >/dev/null 2>&1 || true
    command curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $TOK" -H "Mcp-Session-Id: $SID" "$MCP" || true
    command curl -s -o /dev/null --max-time 30 -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
      -d '{"model":"mock/echo","messages":[{"role":"user","content":"hi"}]}' "$LLM" || true
  done ) &
load=$!
sleep "$SOAK_WARMUP"
: >"$W/soak"
while (( $(date +%s) - t0 < SOAK_SECONDS - 15 )); do
  echo "$(( $(date +%s) - t0 )) $(mem | tr '\n' ' ')" >>"$W/soak"
  sleep "$SOAK_SAMPLE"
done
wait $load
r1=$(restarts)
[[ "$r0" == "$r1" ]] || fail "S3 restarts or replaced pods during the soak: before [$r0] after [$r1]"
bad=$(ch_failed_inserts "$t0")
[[ "$bad" == 0 ]] || fail "S3 ClickHouse refused $bad inserts during the soak (system.query_log)"
kubectl -n "$NS" delete "budgets.$G" soak >/dev/null
verdict=$(uv run --no-project python - "$W/soak" <<'PY'
import sys
rows = []  # (seconds, {pod: MiB})
for line in open(sys.argv[1]):
    t, *pairs = line.split()
    rows.append((int(t), {p: int(v) for p, v in (x.split("=") for x in pairs)}))
if len(rows) < 4:
    sys.exit(f"only {len(rows)} memory samples; raise SOAK_SECONDS or lower SOAK_SAMPLE")
q = len(rows) // 4
avg = lambda part, pod: sum(r[pod] for _, r in part) / len(part)
worst, lines = 0.0, []
for pod in sorted(rows[0][1]):
    first, last = avg(rows[:q], pod), avg(rows[-q:], pod)
    growth = (last - first) / first if first else 0.0
    if last - first > 16:  # smaller moves are noise
        worst = max(worst, growth)
    lines.append(f"{pod} {first:.0f}->{last:.0f}Mi")
# Hourly averages (or quarters, for a soak under 4 hours), for the record.
span = 3600 if rows[-1][0] - rows[0][0] >= 4 * 3600 else max(1, (rows[-1][0] - rows[0][0]) // 4 + 1)
with open(sys.argv[1] + ".table", "w") as out:
    buckets = {}
    for t, r in rows:
        buckets.setdefault((t - rows[0][0]) // span, []).append(r)
    pods = sorted(rows[0][1])
    out.write("from_s\t" + "\t".join(pods) + "\n")
    for b, rs in sorted(buckets.items()):
        out.write(f"{rows[0][0] + b * span}\t" + "\t".join(f"{sum(r[p] for r in rs) / len(rs):.0f}" for p in pods) + "\n")
print(f"{worst:.2f} {len(rows)} samples; " + ", ".join(lines))
PY
) || fail "S3 $verdict"
cat "$W/soak.table" >&2
growth=${verdict%% *}
uv run --no-project python -c "import sys; sys.exit(0 if float(sys.argv[1]) < 0.5 else 1)" "$growth" || fail "S3 memory grew by ${growth} (over 50% and 16 MiB): ${verdict#* }"
pass "S3 ${SOAK_SECONDS}s soak (MCP and LLM traffic), no restarts, no refused ClickHouse writes; memory from ${SOAK_WARMUP}s, last quarter against first: worst growth ${growth} (limit 0.50): ${verdict#* }"

# S5 · rotate the gateway's session key (agentgateway keeps it in a Secret): new sessions work at once;
#      sessions opened before the rotation are recorded as the finding (the key encrypts session IDs).
A=$(token alice) # tokens last 5 minutes; the soak outlived the earlier one
mcp_open "$A"; old_sid=$SID
secret=secret/ambit-session-key # created by the agentgateway controller from the AgentgatewayParameters
kubectl -n "$NS" get "$secret" >/dev/null 2>&1 || fail "S5 no gateway session-key Secret $secret"
# agentgateway's controller recreates the Secret and rolls the gateway itself (a checksum of the key is on
# the pod template). Until every pod has the new key, a session can fail on a pod with the other key.
gen() { kubectl -n "$NS" get deploy -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].metadata.generation}'; }
g0=$(gen)
kubectl -n "$NS" delete "$secret" >/dev/null
for _ in $(seq 1 60); do [[ $(gen) != "$g0" ]] && break; sleep 2; done
[[ $(gen) != "$g0" ]] || fail "S5 the gateway was not rolled after its session key changed"
rollout deploy -l gateway.networking.k8s.io/gateway-name=ambit
for _ in $(seq 1 60); do # old pods (old key) still terminating
  [[ -z $(kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[?(@.metadata.deletionTimestamp)].metadata.name}') ]] && break; sleep 2
done
new=$(tools "$A" >/dev/null 2>&1 && echo ok || echo failed)
SID=$old_sid
oldres=$(mcp_send '{"jsonrpc":"2.0","id":9,"method":"tools/list"}' | head -c 80)
[[ "$new" == ok ]] || fail "S5 new sessions fail after rotating the session key"
pass "S5 session key rotated (${secret#secret/} recreated, gateway rolled by its controller): new sessions work; a session opened before answers '${oldres:-no response}' (clients reconnect)"

# S6 · list every running image, and find which contain a named package
start=$SECONDS
hack/inventory.sh >"$W/images"
n=$(wc -l <"$W/images" | tr -d ' ')
hack/inventory.sh golang.org/x/net >"$W/pkg" 2>/dev/null || true
summary=$(tail -1 "$W/pkg")
[[ "$n" -gt 5 && "$summary" == *"image(s) contain golang.org/x/net"* ]] || fail "S6 inventory: $n images; $(cat "$W/pkg")"
pass "S6 $n running images listed with digests; package search took $((SECONDS - start)) s: $summary"
done_step 10
