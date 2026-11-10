#!/usr/bin/env bash
# Latency benchmark (docs/performance.md): the time Ambit adds to one request. A pod in the cluster sends requests at
# fixed rates (open loop, test/load) to six targets, and the script prints p50/p95/p99 and throughput per cell:
#   llm-direct   the mock model server, no gateway           (baseline for the two LLM cases)
#   llm-jwt      the gateway, a company token (Keycloak JWT)  llm-key   the gateway, a personal API key
#   mcp-direct   the test MCP server (crm), no gateway        (baseline for the two MCP cases)
#   mcp-mask     tools/call through the gateway and guard, masking on
#   mcp-compress the same, with resultCompression: lossless
# Audit (evidence) and budgets (one Budget with a limit far above the load) are on, as in a company install.
# Run as one locked unit on a quiet machine (CLAUDE.md):
#   lockf /tmp/ambit-kind.lock sh -c 'make image install && test/bench.sh'
# Settings: RATES (requests per second, default "10 50 100"), RUNS (3), DURATION (70s, warm-up included),
# WARMUP (10s), OUT (directory for raw results; default a temporary one).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
RATES=${RATES:-10 50 100}; RUNS=${RUNS:-3}; DURATION=${DURATION:-70s}; WARMUP=${WARMUP:-10s}
OUT=${OUT:-$W}; mkdir -p "$OUT"
CURL_IMAGE=curlimages/curl:8.20.0@sha256:b3f1fb2a51d923260350d21b8654bbc607164a987e2f7c84a0ac199a67df812a
POD=bench-load
cleanup() {
  kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" bench --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/keys/$id" >/dev/null 2>&1 || true; done
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

# --- setup: test IdP, mock model, test MCP server, a Budget, a personal key, the sender pod
ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/protection/objects.yaml >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: bench }
spec: { group: ml-research, userRequestsPerDay: 100000000, groupRequestsPerDay: 100000000 }
YAML
kubectl -n "$NS" rollout status deploy/crm deploy/ambit-guard --timeout=180s >/dev/null
kubectl -n "$NS" wait "servers.$G" crm --for=condition=ToolsPinned --timeout=120s >/dev/null || fail "0 crm tools not pinned"
kubectl -n "$NS" wait "budgets.$G" bench --for=condition=Ready --timeout=60s >/dev/null || fail "0 budget not Ready"
kubectl -n "$NS" patch "servers.$G" crm --type json -p '[{"op":"remove","path":"/spec/resultCompression"}]' >/dev/null 2>&1 || true
sign_in keycloak user09 user09-test-pass || fail "0 sign in as user09"
mkkey bench llm

kubectl -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null
kubectl -n "$NS" run "$POD" --image="$CURL_IMAGE" --restart=Never --command -- sleep 86400 >/dev/null
kubectl -n "$NS" wait pod "$POD" --for=condition=Ready --timeout=120s >/dev/null
arch=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$W/load" ./test/load || fail "0 build the load tool"
kubectl -n "$NS" cp "$W/load" "$POD:/tmp/load" >/dev/null
in_pod() { kubectl -n "$NS" exec -i "$POD" -- "$@"; }
put() { in_pod sh -c "cat > /tmp/$1"; }   # put NAME < content: a file in the sender pod
echo '{"model":"mock/echo","messages":[{"role":"user","content":"Summarize the build log in one line."}]}' | put llm.json
# the gateway names a tool <server>_<tool>
echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_rows","arguments":{"n":50}}}' | put mcp-direct.json
echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"crm_get_rows","arguments":{"n":50}}}' | put mcp.json
printf '%s' "$KEY" | put key
: | put none

LLM_GW=http://llm.localtest.me:8080/v1/chat/completions
MCP_GW=http://mcp.localtest.me:8080/mcp
LLM_DIRECT=http://mock-llm.$NS.svc:8080/v1/chat/completions
MCP_DIRECT=http://crm.$NS.svc:8080/mcp
ACCEPT='Accept: application/json, text/event-stream'
# fresh_token USER: a new Keycloak token into the pod's /tmp/jwt (tokens live 5 minutes; one run is shorter)
fresh_token() { local t; t=$(token "$1"); [[ -n "$t" ]] || fail "no token for $1"; printf '%s' "$t" | put jwt; }
# mcp_session: opens an MCP session through the gateway with /tmp/jwt; prints its id
mcp_session() {
  in_pod sh -c 'curl -s -D - -o /dev/null -H "Authorization: Bearer $(cat /tmp/jwt)" -H "Content-Type: application/json" -H "'"$ACCEPT"'" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"bench\",\"version\":\"1\"}}}" '"$MCP_GW"' |
    awk "tolower(\$1)==\"mcp-session-id:\"{print \$2}" | tr -d "\r"'
}
# mcp_run_session: a new session through the gateway (fresh token) whose first tools/call works; sets SID.
# Without this check, one run lost every request to a session that did not work (no Mcp-Session-Id).
mcp_run_session() {
  local r=""
  for _ in 1 2 3; do
    fresh_token user01; SID=$(mcp_session)
    [[ -n "$SID" ]] && r=$(one "$MCP_GW" jwt mcp.json "$ACCEPT" "Mcp-Session-Id: $SID")
    [[ "$r" == *'"result"'* ]] && return 0
    echo "  MCP session not ready: ${r:0:200}"; sleep 2
  done
  fail "no working MCP session through the gateway: ${r:0:300}"
}
# one URL TOKENFILE BODY [HEADER...]: the response body of one request from the pod
one() {
  local u=$1 t=$2 b=$3 hs=""; shift 3
  for x in "$@"; do hs+=" -H '$x'"; done
  in_pod sh -c "curl -s -H \"Authorization: Bearer \$(cat /tmp/$t)\" -H 'Content-Type: application/json'$hs -d @/tmp/$b '$u'"
}
# set_compression lossless|off: the guard reads Servers from a cache, so wait until a result shows the mode:
# pretty-printed rows (off), or rows without the indentation (lossless)
set_compression() {
  local sid want='!='
  if [[ $1 == off ]]; then
    want='=='
    kubectl -n "$NS" patch "servers.$G" crm --type json -p '[{"op":"remove","path":"/spec/resultCompression"}]' >/dev/null 2>&1 || true
  else
    kubectl -n "$NS" patch "servers.$G" crm --type merge -p '{"spec":{"resultCompression":{"mode":"lossless"}}}' >/dev/null
  fi
  fresh_token user01; sid=$(mcp_session)
  wait_for 90 '[[ "$(one "$MCP_GW" jwt mcp.json "$ACCEPT" "Mcp-Session-Id: $sid")" '"$want"' *"\\n    {"* ]]' || fail "0 the guard did not apply compression $1"
}

# --- check every target once before measuring
fresh_token user09
[[ $(one "$LLM_DIRECT" none llm.json) == *'"choices"'* ]] || fail "0 the mock model does not answer directly"
[[ $(one "$LLM_GW" jwt llm.json) == *'"choices"'* ]] || fail "0 the gateway refused the JWT: $(one "$LLM_GW" jwt llm.json)"
wait_for 30 '[[ $(one "$LLM_GW" key llm.json) == *"\"choices\""* ]]' || fail "0 the gateway refused the personal key: $(one "$LLM_GW" key llm.json)"
[[ $(one "$MCP_DIRECT" none mcp-direct.json "$ACCEPT") == *minsu.kim@example.com* ]] || fail "0 the crm server does not answer directly"
fresh_token user01; SID=$(mcp_session); [[ -n "$SID" ]] || fail "0 no MCP session through the gateway"
r=$(one "$MCP_GW" jwt mcp.json "$ACCEPT" "Mcp-Session-Id: $SID")
[[ "$r" == *'<EMAIL>'* && "$r" != *minsu.kim@example.com* ]] || fail "0 the guard did not mask the result: ${r:0:300}"
pass "0 every target answers; the guard masks the MCP result"

# --- measure: run r of every cell, then run r+1, so slow drift of the machine spreads over all cells
# load CASE RATE RUN URL TOKENFILE BODY [HEADER...]: one run; appends "case rate run p50 p95 p99 completed errors" to $OUT/runs.tsv
load() {
  local c=$1 rate=$2 run=$3 u=$4 t=$5 b=$6; shift 6
  local -a h=(); for x in "$@"; do h+=(-header "$x"); done
  local line
  line=$(in_pod /tmp/load -url "$u" -rate "$rate" -duration "$DURATION" -warmup "$WARMUP" -token-file "/tmp/$t" -body-file "/tmp/$b" ${h[@]+"${h[@]}"})
  echo "$c $rate $run $line" | tee -a "$OUT/runs.log"
  [[ "$line" =~ ^p50=([0-9.]+)ms\ p95=([0-9.]+)ms\ p99=([0-9.]+)ms\ offered=[0-9]+\ completed=([0-9]+)\ errors=([0-9]+)$ ]] ||
    fail "$c at $rate req/s, run $run: unexpected output of test/load: $line"
  # a run where more than 1 % of the requests failed measures the failure, not the gateway
  (( BASH_REMATCH[4] > 0 && BASH_REMATCH[5] * 100 <= BASH_REMATCH[4] + BASH_REMATCH[5] )) ||
    fail "$c at $rate req/s, run $run: more than 1 % of the requests failed: $line"
  echo "$c $rate $run ${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]} ${BASH_REMATCH[4]} ${BASH_REMATCH[5]}" >> "$OUT/runs.tsv"
}
: > "$OUT/runs.tsv"; : > "$OUT/runs.log"
for run in $(seq 1 "$RUNS"); do
  set_compression off
  for rate in $RATES; do
    load llm-direct "$rate" "$run" "$LLM_DIRECT" none llm.json
    fresh_token user09; load llm-jwt "$rate" "$run" "$LLM_GW" jwt llm.json
    load llm-key "$rate" "$run" "$LLM_GW" key llm.json
    load mcp-direct "$rate" "$run" "$MCP_DIRECT" none mcp-direct.json "$ACCEPT"
    mcp_run_session; load mcp-mask "$rate" "$run" "$MCP_GW" jwt mcp.json "$ACCEPT" "Mcp-Session-Id: $SID"
  done
  set_compression lossless
  for rate in $RATES; do
    mcp_run_session; load mcp-compress "$rate" "$run" "$MCP_GW" jwt mcp.json "$ACCEPT" "Mcp-Session-Id: $SID"
  done
done
set_compression off

# --- summary: median of the runs [lowest-highest]; the added time is the median of a case minus the median of its
# direct baseline at the same rate. The noise floor of a cell is the larger spread (highest minus lowest run) of the case
# and of its baseline:
# an added time smaller than it is shown as "below noise (< X)", not as a number.
uv run --no-project python - "$OUT/runs.tsv" "$WARMUP" "$DURATION" <<'PY' | tee "$OUT/summary.md"
import sys, statistics as st, collections
rows = [l.split() for l in open(sys.argv[1]) if l.strip()]
sec = lambda s: float(s.rstrip("s"))
measured = sec(sys.argv[3]) - sec(sys.argv[2])
cells = collections.defaultdict(list)
for c, rate, run, p50, p95, p99, done, err in rows:
    cells[(c, int(rate))].append((float(p50), float(p95), float(p99), int(done), int(err)))
base = {"llm-jwt": "llm-direct", "llm-key": "llm-direct", "mcp-mask": "mcp-direct", "mcp-compress": "mcp-direct"}
order = ["llm-direct", "llm-jwt", "llm-key", "mcp-direct", "mcp-mask", "mcp-compress"]
rates = lambda c: sorted({r for (k, r) in cells if k == c})
med = lambda v, i: st.median(x[i] for x in v)
print("Added time (ms), median of the runs; below noise = smaller than the spread between runs of the case or of its direct baseline")
print()
print("| Case | Rate (req/s) | Added p50 | Added p95 | Added p99 |")
print("|---|---|---|---|---|")
for c in order:
    if c not in base:
        continue
    for rate in rates(c):
        v, b = cells[(c, rate)], cells.get((base[c], rate))
        if not b:
            continue
        out = []
        for i in range(3):
            added = round(med(v, i) - med(b, i), 1)  # decided at the printed precision
            noise = round(max(max(x[i] for x in w) - min(x[i] for x in w) for w in (v, b)), 1)
            out.append(f"{added:.1f}" if added > noise else f"below noise (< {max(noise, -added, 0.1):.1f})")
        print(f"| {c} | {rate} | {' | '.join(out)} |")
print()
print("All measurements (ms): median of the runs [lowest-highest]")
print()
print("| Case | Rate (req/s) | p50 | p95 | p99 | Throughput (req/s) | Errors |")
print("|---|---|---|---|---|---|---|")
for c in order:
    for rate in rates(c):
        v = cells[(c, rate)]
        col = lambda i: f"{med(v, i):.1f} [{min(x[i] for x in v):.1f}-{max(x[i] for x in v):.1f}]"
        print(f"| {c} | {rate} | {col(0)} | {col(1)} | {col(2)} | {med(v, 3) / measured:.1f} | {sum(x[4] for x in v)} |")
print(f"\n{len(rows)} runs; each run {sys.argv[3]} with the first {sys.argv[2]} not measured.")
PY
done_step bench
