#!/usr/bin/env bash
# Feature A acceptance: compression of MCP tool results in ambit-guard (Server.spec.resultCompression).
#   1 off (no field): a result keeps the size the tool sent    5 a result over the 4 MiB cap passes, masked
#   2 lossless: JSON rows smaller, same rows, email masked     6 the guard's /metrics counts it per server
#   3 shorten: marker, hint, every ERROR line, email masked    7 kill switch: off restores the original size
#   4 HTML is not changed in any mode                          8 memory: two 4 MiB results at once, no OOMKilled
# Run as one locked unit (CLAUDE.md): lockf /tmp/ambit-kind.lock sh -c 'make image install && test/stepcompress.sh'
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
GUARD="app.kubernetes.io/name=ambit-guard"
cleanup() { kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true; }
trap 'rc=$?; cleanup; on_exit $rc' EXIT

# raw NAME ARGS: the JSON-RPC response of a tools/call in the open session.
raw() { mcp_send "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}"; }
# size: bytes of the first text of a response on stdin.
size() { uv run --no-project python -c 'import json,sys;print(len(json.loads(sys.stdin.read())["result"]["content"][0]["text"].encode()))'; }
text() { uv run --no-project python -c 'import json,sys;print(json.loads(sys.stdin.read())["result"]["content"][0]["text"])'; }
set_mode() { kubectl -n "$NS" patch "servers.$G" crm --type merge -p "{\"spec\":{\"resultCompression\":{\"mode\":\"$1\"}}}" >/dev/null; }
# until_size NAME ARGS OP BYTES: polls up to 60 s (the guard reads a cache) until the size is OP (lt, eq) BYTES.
until_size() {
  local end=$((SECONDS + 60)) s
  while :; do
    s=$(raw "$1" "$2" | size 2>/dev/null || echo 0)
    case $3 in lt) (( s > 0 && s < $4 )) && { SIZE=$s; return 0; } ;; eq) (( s == $4 )) && { SIZE=$s; return 0; } ;; esac
    (( SECONDS < end )) || return 1
    sleep 2
  done
}
restarts() { kubectl -n "$NS" get pod -l "$GUARD" -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'; }
metrics() { kubectl get --raw "/api/v1/namespaces/$NS/pods/$(kubectl -n "$NS" get pod -l "$GUARD" -o jsonpath='{.items[0].metadata.name}'):8081/proxy/metrics"; }

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" delete -f test/protection/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" apply -f test/protection/objects.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/crm deploy/ambit-guard --timeout=180s >/dev/null
kubectl -n "$NS" wait "servers.$G" crm --for=condition=ToolsPinned --timeout=120s >/dev/null || fail "0 crm tools not pinned"
for _ in $(seq 1 30); do tools "$(token alice)"; [[ "$TOOLS" == *crm_get_rows* ]] && break; sleep 2; done
[[ "$TOOLS" == *crm_get_rows* && "$TOOLS" == *crm_get_log* && "$TOOLS" == *crm_get_page* ]] || fail "0 alice does not see the compression test tools: $TOOLS"
before=$(restarts)

# 1 · no field: the result is as the tool sent it (pretty-printed, email masked)
ROWS='{"n":300}'; LOG='{"kib":300}'
out=$(raw crm_get_rows "$ROWS")
base=$(size <<<"$out")
[[ $(text <<<"$out") == *$'\n    "id"'* && "$out" != *minsu.kim@example.com* && "$out" == *"<EMAIL>"* ]] || fail "1 result is not the plain masked rows: ${out:0:300}"
log_base=$(raw crm_get_log "$LOG" | size)
page_base=$(raw crm_get_page '{}' | size)
pass "1 no resultCompression: rows $base bytes, log $log_base bytes, page $page_base bytes (masked)"

# 2 · lossless: rows smaller, same rows, email masked
set_mode lossless
until_size crm_get_rows "$ROWS" lt "$base" || fail "2 rows did not get smaller in 60 s"
out=$(raw crm_get_rows "$ROWS")
uv run --no-project python - "$out" "$base" <<'PY' || fail "2 rows changed or not masked"
import json, sys
t = json.loads(sys.argv[1])["result"]["content"][0]["text"]
d = json.loads(t)
rows = [dict(zip(d["columns"], r)) for r in d["rows"]] if isinstance(d, dict) else d
assert len(rows) == 300 and [r["id"] for r in rows] == list(range(300)), len(rows)
assert rows[-1]["contact"] == "<EMAIL>" and "minsu.kim@example.com" not in t
print(f"{sys.argv[2]} -> {len(t.encode())} bytes")
PY
pass "2 lossless: rows $base -> $SIZE bytes, 300 rows kept, email masked"

# 3 · shorten: the log is shortened with markers, every ERROR line kept, email masked
set_mode shorten
until_size crm_get_log "$LOG" lt "$((log_base / 4))" || fail "3 log did not get smaller in 60 s"
t=$(raw crm_get_log "$LOG" | text)
errors=$(( $(grep -c ' ERROR request ' <<<"$t") ))
[[ $errors -ge 2 && "$t" == *"[ambit: "*"repeated lines omitted here]"* && "$t" == *"call the tool again with a narrower query"* ]] || fail "3 marker, hint or ERROR lines missing: ${t:0:400}"
[[ "$t" != *minsu.kim@example.com* ]] || fail "3 email reached the client"
pass "3 shorten: log $log_base -> $SIZE bytes, $errors ERROR lines kept, marker and hint present, email masked"

# 4 · HTML is not changed in any mode
[[ $(raw crm_get_page '{}' | size) == "$page_base" ]] || fail "4 the HTML page changed in shorten mode"
set_mode lossless; sleep 5
[[ $(raw crm_get_page '{}' | size) == "$page_base" ]] || fail "4 the HTML page changed in lossless mode"
pass "4 HTML page unchanged in lossless and shorten ($page_base bytes)"

# 5 · a 5 MiB text is over the cap: passed uncompressed and still masked, no restart
set_mode shorten; sleep 5
big=$(raw crm_get_log '{"kib":5120}')
[[ $(size <<<"$big") -gt $((5 << 20)) && "$big" != *minsu.kim@example.com* && "$big" == *"<EMAIL>"* && "$(text <<<"$big")" != *"[ambit: "* ]] || fail "5 5 MiB result: ${big:0:200}"
[[ $(restarts) == "$before" ]] || fail "5 ambit-guard restarted"
pass "5 5 MiB log passed uncompressed ($(size <<<"$big") bytes), masked, guard not restarted"

# 6 · metrics per server
m=$(metrics)
for want in 'ambit_guard_compress_results_total{outcome="compressed",server="crm",type="json"}' \
            'ambit_guard_compress_results_total{outcome="compressed",server="crm",type="logs"}' \
            'ambit_guard_compress_results_total{outcome="skipped_too_large",server="crm",type="unknown"}' \
            'ambit_guard_compress_results_total{outcome="skipped_unknown",server="crm",type="unknown"}' \
            'ambit_guard_compress_bytes_in_total{server="crm",type="logs"}' \
            'ambit_guard_compress_omitted_total{server="crm",type="logs"}'; do
  grep -qF "$want" <<<"$m" || fail "6 metric missing: $want"
done
pass "6 /metrics has compressed, skipped_too_large and skipped_unknown counters with server=\"crm\""

# 7 · kill switch: off, and the original size is back at once
set_mode off
until_size crm_get_rows "$ROWS" eq "$base" || fail "7 rows did not return to $base bytes"
until_size crm_get_log "$LOG" eq "$log_base" || fail "7 log did not return to $log_base bytes"
pass "7 mode off: rows and log are back to their original sizes"

# 8 · memory: two 4 MiB results at once in shorten mode; the guard must not be OOMKilled. A new guard pod starts the
# peak (the kernel's memory.peak of the container's cgroup, which counts what the memory limit counts) from zero.
set_mode shorten
kubectl -n "$NS" rollout restart deploy/ambit-guard >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-guard --timeout=180s >/dev/null
wait_for 120 'tools "$(token alice)"; [[ "$TOOLS" == *crm_get_log* ]]' || fail "8 tools not back after the guard restart"
until_size crm_get_log "$LOG" lt "$((log_base / 4))" || fail "8 shorten did not apply"
before=$(restarts)
for i in 1 2; do ( mcp_open "$(token alice)"; raw crm_get_log '{"kib":4000}' | size >"$W/mem$i" ) & done
wait
for i in 1 2; do [[ $(cat "$W/mem$i") -gt 0 && $(cat "$W/mem$i") -lt $((4000 * 1024 / 4)) ]] || fail "8 result $i was not shortened: $(cat "$W/mem$i")"; done
[[ $(restarts) == "$before" ]] || fail "8 ambit-guard restarted: $(kubectl -n "$NS" get pod -l "$GUARD" -o jsonpath='{.items[0].status.containerStatuses[0].lastState}')"
cid=$(kubectl -n "$NS" get pod -l "$GUARD" -o jsonpath='{.items[0].status.containerStatuses[0].containerID}'); cid=${cid#*://}
node=${KIND_NODE:-${KIND_CLUSTER:-ambit}-control-plane}
peak=$(docker exec "$node" sh -c "cat \$(find /sys/fs/cgroup -type d -name \"cri-containerd-$cid.scope\" | head -1)/memory.peak")
limit=$(kubectl -n "$NS" get pod -l "$GUARD" -o jsonpath='{.items[0].spec.containers[0].resources.limits.memory}')
pass "8 two 4000 KiB logs at once in shorten mode: sizes $(cat "$W/mem1") and $(cat "$W/mem2") bytes, peak memory $((peak >> 20)) MiB of $limit, no restart"
done_step compress
