#!/usr/bin/env bash
# Agents (docs/agents.md, experimental): a person makes agents for themselves, admins make itself-mode agents; an agent
# key carries its scope and source ranges, and the gateway enforces them next to the owner's AccessRules; the access log
# and the Audit page name the agent; only the console writes agent records. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
field() { uv run --no-project python -c "import json,sys;d=json.load(open(sys.argv[1]));print($1)" "$W/api"; }
# ag METHOD PATH [JSON]: console API as the signed-in user, retried while a console takes over the key Lease (503)
ag() { local c; for _ in $(seq 1 15); do c=$(api "$@"); [[ "$c" == 503 ]] || break; sleep 2; done; echo "$c"; }
policy_expr() { kubectl -n "$NS" get agentgatewaypolicy ambit-mcp-access -o jsonpath='{.spec.backend.mcp.authorization.policy.matchExpressions[0]}'; }
past() { date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ; }
# init_code KEY: the HTTP status of an MCP initialize with the key (no session is kept)
init_code() {
  command curl -s -o "$W/init" -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stepagents","version":"1"}}}' "$MCP"
}
# agent_key AGENT KEYNAME [RANGES-JSON]: a key for the agent as the signed-in person; sets KEY and KID
agent_key() {
  local c; c=$(ag POST "/api/agents/$1/keys" "{\"name\":\"$2\",\"days\":30,\"sourceRanges\":${3:-[]}}")
  [[ "$c" == 201 ]] || fail "key $2 for $1: HTTP $c $(cat "$W/api")"
  KEY=$(field 'd["key"]'); KID=$(field 'd["view"]["id"]')
}
AGENTS=(); KEYIDS=()
cleanup() {
  kubectl -n "$NS" delete "accessrules.$G" agents-everyone-mock --ignore-not-found >/dev/null 2>&1 || true
  local pw; pw=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true
  if sign_in local admin@ambit.local "$pw" >/dev/null 2>&1; then
    for n in ${AGENTS[@]+"${AGENTS[@]}"}; do api DELETE "/api/agents/$n" >/dev/null 2>&1 || true; done
    for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/admin/keys/$id" >/dev/null 2>&1 || true; done
  fi
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null && mcp_ready "$NS"
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW" || fail "0 bootstrap admin sign-in"
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "0 reset-limits hook: $(cat "$W/api")"
# user09 (ml-research) may use every tool of both test servers: what an agent's scope takes away is then visible.
sign_in keycloak user09 user09-test-pass || fail "0 user09 sign-in"
SUFFIX=$(date +%s | tail -c 7)

# 1 · a person makes an agent for themselves (scope: one tool), gets a key, and the gateway shows and allows only that tool
c=$(ag POST /api/agents "{\"displayName\":\"Grok Bot $SUFFIX\",\"platform\":\"grok\",\"scope\":[\"time/get_current_time\"]}")
[[ "$c" == 201 ]] || fail "1 create agent: HTTP $c $(cat "$W/api")"
A1=$(field 'd["name"]'); AGENTS+=("$A1")
[[ $(field 'd["owner"]+" "+d["mode"]+" "+d["state"]') == "user09@example.com person active" ]] || fail "1 agent record: $(cat "$W/api")"
agent_key "$A1" connector; K1=$KEY; K1ID=$KID; KEYIDS+=("$KID")
wait_for 90 'try_tools "$K1"; [[ "$TOOLS" == "time_get_current_time" ]]' || fail "1 the agent sees: $TOOLS (scope: time/get_current_time)"
tools "$K1"
r=$(call time_get_current_time '{"timezone":"UTC"}'); [[ "$r" == ok* ]] || fail "1 allowed tool: $r"
r1=$(call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"Asia/Seoul"}'); r2=$(call everything_echo '{"message":"x"}')
[[ "$r1" == error* && "$r2" == error* ]] || fail "1 tools outside the scope were allowed: $r1 / $r2"
tools "$(token user09)"; [[ "$TOOLS" == *everything_echo* && "$TOOLS" == *time_convert_time* ]] || fail "1 the owner's own token lost tools: $TOOLS"
pass "1 agent $A1: tools/list = time_get_current_time, the call works, convert_time and everything_echo are refused; user09's own token still sees $TOOLS"

# 2 · a scope change reaches the key (the gateway reads the key's metadata): a whole server, then nothing
[[ $(ag PATCH "/api/agents/$A1" '{"scope":["time"]}') == 200 ]] || fail "2 patch scope: $(cat "$W/api")"
wait_for 60 'try_tools "$K1"; [[ "$TOOLS" == "time_convert_time time_get_current_time" ]]' || fail "2 whole server time: the agent sees $TOOLS"
[[ $(ag PATCH "/api/agents/$A1" '{"scope":[]}') == 200 ]] || fail "2 patch empty scope: $(cat "$W/api")"
wait_for 60 'try_tools "$K1"; [[ -z "$TOOLS" ]]' || fail "2 an empty scope: the agent sees $TOOLS"
tools "$K1"; r=$(call time_get_current_time '{"timezone":"UTC"}'); [[ "$r" == error* ]] || fail "2 an empty scope allowed a call: $r"
[[ $(ag PATCH "/api/agents/$A1" '{"scope":["time/get_current_time"]}') == 200 ]] || fail "2 patch back: $(cat "$W/api")"
wait_for 60 'try_tools "$K1"; [[ "$TOOLS" == "time_get_current_time" ]]' || fail "2 back to one tool: $TOOLS"
pass "2 scope time -> 2 tools; [] -> none (a call is refused); one tool again -> 1 tool; each change took effect within 60 s"

# 3 · a person's own key (no agent) is not limited by any scope
mkkey own mcp; OWN=$KEY
wait_for 60 'try_tools "$OWN"; [[ "$TOOLS" == *everything_echo* && "$TOOLS" == *time_convert_time* ]]' || fail "3 an ordinary key lost tools: $TOOLS"
pass "3 user09's ordinary MCP key still sees every tool of the owner's rules"

# 4 · source ranges: a key from other addresses is refused (403), one that allows any address works; bad ranges are 400
c=$(ag POST /api/agents "{\"displayName\":\"Ranges $SUFFIX\",\"platform\":\"cloudflare\",\"scope\":[\"time\"]}"); [[ "$c" == 201 ]] || fail "4 create agent: HTTP $c $(cat "$W/api")"
A2=$(field 'd["name"]'); AGENTS+=("$A2")
agent_key "$A2" elsewhere '["192.0.2.0/24","2001:db8::/32"]'; K4=$KEY; KEYIDS+=("$KID")
agent_key "$A2" everywhere '["0.0.0.0/0","::/0"]'; K4B=$KEY; KEYIDS+=("$KID")
c=$(ag POST "/api/agents/$A2/keys" '{"name":"third","days":30}'); [[ "$c" == 429 ]] || fail "4 a third key of one agent: HTTP $c"
wait_for 90 '[[ $(init_code "$K4B") == 200 ]]' || fail "4 a key for any address: $(cat "$W/init")"
[[ $(init_code "$K4") == 403 ]] || fail "4 a key limited to other addresses was accepted: $(cat "$W/init")"
c=$(ag POST "/api/agents/$A2/keys" '{"name":"x","days":30,"sourceRanges":["not-a-range"]}'); [[ "$c" == 400 ]] || fail "4 a bad range: HTTP $c"
pass "4 a key limited to 192.0.2.0/24 and 2001:db8::/32 gets 403; a key for 0.0.0.0/0 and ::/0 gets 200; a bad range 400; a third key 429"

# 5 · limits: 51 scope entries and a 365-day agent key are refused; the policy text does not grow with the agents
before=$(policy_expr | wc -c | tr -d ' ')
for i in $(seq 1 8); do
  c=$(ag POST /api/agents "{\"displayName\":\"filler $i\",\"scope\":[\"time\"]}"); [[ "$c" == 201 ]] || fail "5 filler agent $i: HTTP $c $(cat "$W/api")"
  AGENTS+=("$(field 'd["name"]')")
done
c=$(ag POST /api/agents '{"displayName":"eleventh"}'); [[ "$c" == 409 ]] || fail "5 the 11th agent of one person: HTTP $c"
sleep 5
after=$(policy_expr | wc -c | tr -d ' ')
[[ "$before" == "$after" && "$before" -gt 100 ]] || fail "5 the MCP access policy changed size with 10 agents: $before -> $after"
big=$(python3 -c 'import json;print(json.dumps({"displayName":"big","scope":["s%d"%i for i in range(51)]}))')
c=$(ag POST /api/agents "$big"); [[ "$c" == 400 ]] || fail "5 51 scope entries: HTTP $c"
c=$(ag POST "/api/agents/$A1/keys" '{"name":"year","days":365}'); [[ "$c" == 400 ]] || fail "5 a 365-day agent key: HTTP $c"
for n in "${AGENTS[@]:2}"; do [[ $(ag DELETE "/api/agents/$n") == 204 ]] || true; done   # the fillers (and the agents after them): free the slots
pass "5 10 agents per person (the 11th: 409); the access policy is $after characters with 1 agent and with 10; 51 scope entries 400; a 365-day key 400"

# 6 · itself mode: only admins; the agent is a service account with an owner, a platform and a scope
c=$(ag POST /api/agents "{\"displayName\":\"x\",\"mode\":\"itself\",\"name\":\"nightly-$SUFFIX\",\"group\":\"ml-research\"}"); [[ "$c" == 403 ]] || fail "6 a person created an itself-mode agent: HTTP $c"
sign_in local admin@ambit.local "$PW" || fail "6 admin sign-in"
NIGHTLY="nightly-$SUFFIX"
c=$(ag POST /api/agents "{\"displayName\":\"Nightly report\",\"mode\":\"itself\",\"name\":\"$NIGHTLY\",\"group\":\"ml-research\",\"platform\":\"agentcore\",\"owner\":\"user09@example.com\",\"scope\":[\"everything/echo\"]}")
[[ "$c" == 201 ]] || fail "6 create itself-mode agent: HTTP $c $(cat "$W/api")"
AGENTS+=("$NIGHTLY")
[[ $(field 'd["identity"]') == "$NIGHTLY@svc.ambit.internal" ]] || fail "6 identity: $(cat "$W/api")"
c=$(ag POST "/api/admin/service-accounts/$NIGHTLY/keys" '{"name":"bypass","scope":"mcp","days":30}'); [[ "$c" == 400 ]] || fail "6 an unscoped key of the agent's account: HTTP $c"
agent_key "$NIGHTLY" job; K6=$KEY; KEYIDS+=("$KID")
wait_for 90 'try_tools "$K6"; [[ "$TOOLS" == "everything_echo" ]]' || fail "6 the itself-mode agent sees: $TOOLS"
tools "$K6"; r=$(call everything_echo '{"message":"from the agent"}'); [[ "$r" == ok*"from the agent"* ]] || fail "6 echo: $r"
pass "6 itself-mode agent $NIGHTLY@svc.ambit.internal: only admins create it; its key sees only everything_echo and calls it; the account page cannot issue an unscoped key"

# 7 · Audit: the calls name the agent, mode, declared platform, owner at issue and the Gateway; filters and the export
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
arecs() { [[ $(api GET "/api/audit?agent=$1&from=$day&to=$next") == 200 ]] && field 'len(d["calls"])'; }
wait_for 120 '[[ $(arecs "$A1") -ge 3 ]]' || fail "7 no Audit records for agent $A1: $(head -c 800 "$W/api")"
[[ $(field 'sum(1 for c in d["calls"] if c.get("agent")=="'"$A1"'" and c.get("agentMode")=="person" and c.get("platformDeclared")=="grok" and c.get("agentOwnerAtIssue")=="user09@example.com" and c.get("ingress","") in ("","private") and c["keyId"]=="'"$K1ID"'" and c["identity"]=="key" and c.get("agentOwnerNow")=="user09@example.com")') -ge 3 ]] || fail "7 agent fields: $(head -c 1200 "$W/api")"
[[ $(field 'sum(1 for c in d["calls"] if c.get("agent","")!="'"$A1"'")') == 0 ]] || fail "7 the agent filter returned other calls"
wait_for 60 '[[ $(api GET "/api/audit?platform_declared=agentcore&from=$day&to=$next") == 200 && $(field "sum(1 for c in d[\"calls\"] if c.get(\"agent\")==\"$NIGHTLY\" and c.get(\"agentMode\")==\"itself\")") -ge 1 ]]' || fail "7 platform filter: $(head -c 800 "$W/api")"
[[ $(api GET "/api/audit?email=user09@example.com&from=$day&to=$next") == 200 ]] && [[ $(field 'sum(1 for c in d["calls"] if c.get("agent","")=="" and c["identity"]=="jwt")') -ge 1 ]] || fail "7 the owner's own token calls carry no agent: $(head -c 600 "$W/api")"
[[ $(api GET "/api/audit?ingress=public&from=$day&to=$next") == 400 ]] || fail "7 ingress alone must be refused"
command curl -s -b "$W/jar" -o "$W/audit.zip" "$BASE/api/evidence/audit.zip?from=$day&to=$next"
unzip -p "$W/audit.zip" calls.csv | A1="$A1" python3 -c '
import csv, sys, os
rows = list(csv.reader(sys.stdin)); h = rows[0]
want = "time,email,what,target,outcome,status,error,input_tokens,output_tokens,mark,rules_version,identity,key_id,subject,route,method,server,tool,model,router,path,access_mode,client_address,prompt_guard,call_id,agent,agent_mode,agent_owner_at_issue,agent_owner_now,platform_declared,grant_id,token_issuer,ingress"
assert ",".join(h) == want, ",".join(h)
i = {n: k for k, n in enumerate(h)}
hit = [r for r in rows[1:] if r[i["agent"]] == os.environ["A1"] and r[i["agent_mode"]] == "person" and r[i["agent_owner_at_issue"]] == "user09@example.com"
       and r[i["agent_owner_now"]] == "user09@example.com" and r[i["platform_declared"]] == "grok" and r[i["ingress"]] in ("", "private")]
assert hit, "no row of the agent"
' || fail "7 calls.csv: header or the agent's rows are wrong"
pass "7 Audit: calls of $A1 (person, grok, owner at issue and now user09, private, key $K1ID); filters agent= and platform_declared= work; calls.csv has the 8 new columns after call_id"

# 8 · only the console writes agent records and the key metadata
kubectl -n "$NS" create configmap forged-agent --from-literal=agent.json='{}' >/dev/null
if kubectl -n "$NS" label configmap forged-agent ambitproject.dev/agent=true 2>"$W/err"; then fail "8 relabelling a ConfigMap into an agent record was admitted"; fi
grep -q "only the Ambit console may write personal API key objects" "$W/err" || fail "8 wrong refusal: $(cat "$W/err")"
kubectl -n "$NS" delete configmap forged-agent >/dev/null
if kubectl -n "$NS" patch configmap "ambit-agent-$A1" --type merge -p '{"data":{"agent.json":"{}"}}' 2>"$W/err"; then fail "8 an admin's kubectl changed an agent record"; fi
grep -q "only the Ambit console may write personal API key objects" "$W/err" || fail "8 wrong refusal for the patch: $(cat "$W/err")"
pass "8 a ConfigMap relabelled into an agent record, and a direct change of one, are refused by admission"

# 9 · deleting an agent deletes its keys; the gateway refuses them
sign_in keycloak user09 user09-test-pass
[[ $(init_code "$K4B") == 200 ]] || fail "9 the key must work before the delete"
[[ $(ag DELETE "/api/agents/$A2") == 204 ]] || fail "9 delete agent: $(cat "$W/api")"
wait_for 30 '[[ $(init_code "$K4B") == 403 ]]' || fail "9 a deleted agent's key still works: $(cat "$W/init")"
[[ $(ag GET "/api/agents/$A2") == 404 ]] || fail "9 the agent is still there"
pass "9 deleting the agent revoked its keys: refused (403)"

# ---- stopping an agent (docs/agents.md): the gateway's stop policy, the acknowledgment, and the measured times ----
CONNECT=${AMBIT_E2E_CONNECT:-127.0.0.1:8080}
now_ms() { perl -MTime::HiRes=time -e 'printf("%d", time*1000)'; }
agent_state() { api GET "/api/agents/$1" >/dev/null && field 'd["state"]'; }
# new_agent_key: a person agent of user09 with one key; sets AGENT, K, KID (the agent is deleted at the end, the key with it)
new_agent_key() {
  local c; c=$(ag POST /api/agents "{\"displayName\":\"stop test $1\",\"scope\":[\"time\"]}"); [[ "$c" == 201 ]] || fail "$1 create agent: HTTP $c $(cat "$W/api")"
  AGENT=$(field 'd["name"]'); AGENTS+=("$AGENT")
  agent_key "$AGENT" probe '[]'; K=$KEY; KEYIDS+=("$KID")
  wait_for 90 '[[ $(init_code "$K") == 200 ]]' || fail "$1 a new agent key is not accepted: $(cat "$W/init")"
}
# poll KEY OUTFILE: calls the MCP URL with the key every 25 ms in the background; sets POLLPID
go build -o "$W/stopmeasure" ./test/stopmeasure || fail "build stopmeasure"
poll() { "$W/stopmeasure" -url "$MCP" -key "$1" -connect "$CONNECT" > "$2" & POLLPID=$!; sleep 1; }
# admin_hook PATH BODY: a test hook as the bootstrap admin (the hooks are admin-only); signs user09 in again after it
admin_hook() { sign_in local admin@ambit.local "$PW" >/dev/null || fail "admin sign-in"; local c; c=$(api POST "$1" "$2"); sign_in keycloak user09 user09-test-pass >/dev/null || fail "user09 sign-in"; echo "$c"; }
# measure FILE T0: "<first refusal in ms from T0, -1 if none> <accepted calls after T0> <calls with no answer or a server error>".
# A refusal is 401 or 403; no answer and 5xx are counted apart, never as a refusal.
measure() {
  python3 - "$1" "$2" <<'PY'
import sys
t0 = int(sys.argv[2]); first = None; acc = 0; err = 0
for line in open(sys.argv[1]):
    p = line.split()
    if len(p) != 2: continue
    t, st = int(p[0]), int(p[1])
    if t < t0: continue
    if st in (401, 403):
        if first is None: first = t - t0
    elif st == 0 or st >= 500:
        err += 1
    elif st < 400:
        acc += 1
print(first if first is not None else -1, acc, err)
PY
}
await_state() { wait_for "$3" '[[ $(agent_state '"$1"') == '"$2"' ]]'; }

# 10 · the stop list alone refuses a key that is still valid: the gateway's policy is the authoritative check
sign_in keycloak user09 user09-test-pass; new_agent_key 10
[[ $(admin_hook "/api/test/agents/$AGENT/list-only" '{"on":true}') == 204 ]] || fail "10 list-only hook: $(cat "$W/api")"
wait_for 60 '[[ $(init_code "$K") == 403 ]]' || fail "10 a key of a listed agent was not refused by the stop policy: $(cat "$W/init")"
kubectl -n "$NS" get secret "ambit-key-$KID" >/dev/null || fail "10 the key Secret is gone: the refusal came from the key, not from the stop policy"
[[ $(admin_hook "/api/test/agents/$AGENT/list-only" '{"on":false}') == 204 ]] || fail "10 unlist: $(cat "$W/api")"
wait_for 90 '[[ $(init_code "$K") == 200 ]]' || fail "10 the key is not accepted again after the entry was removed"
pass "10 the agent on the stop list: its still-valid key is refused (403) by the gateway policy; off the list it works again"

# 11 · a list of 151 entries is two policies; both are accepted and the policy still refuses
[[ $(admin_hook /api/test/agents/stop-fill '{"n":151}') == 204 ]] || fail "11 fill hook: $(cat "$W/api")"
wait_for 60 'kubectl -n "$NS" get agentgatewaypolicy ambit-agent-stops-0 ambit-agent-stops-1 >/dev/null' || fail "11 two stop policies were not created"
[[ $(admin_hook "/api/test/agents/$AGENT/list-only" '{"on":true}') == 204 ]] || fail "11 list-only hook"
wait_for 90 '[[ $(init_code "$K") == 403 ]]' || fail "11 with 152 entries in 2 policies the listed agent's key is still accepted"
for i in 0 1; do
  [[ $(kubectl -n "$NS" get agentgatewaypolicy "ambit-agent-stops-$i" -o jsonpath='{.status.ancestors[*].conditions[?(@.type=="Accepted")].status}') == *True* ]] || fail "11 policy $i is not accepted"
done
[[ $(admin_hook /api/test/agents/stop-fill '{"n":0}') == 204 && $(admin_hook "/api/test/agents/$AGENT/list-only" '{"on":false}') == 204 ]] || fail "11 clean up"
wait_for 90 '! kubectl -n "$NS" get agentgatewaypolicy ambit-agent-stops-1 >/dev/null 2>&1' || fail "11 the second policy stays after the list got short"
[[ $(ag DELETE "/api/agents/$AGENT") == 204 ]] || fail "11 delete the probe agent"
pass "11 152 entries: two stop policies, both accepted, the listed agent is refused; back to one policy when the list is short"

# 12 · measured: stop request to first refusal, 20 runs with keys on the private Gateway
RUNS=${STOP_RUNS:-20}; : > "$W/runs"
for i in $(seq 1 "$RUNS"); do
  [[ $(admin_hook /api/test/keys/reset-limits '{}') == 204 ]] || fail "12.$i reset-limits"   # 10 keys per person and hour
  new_agent_key "12.$i"; poll "$K" "$W/poll"
  T0=$(now_ms); c=$(ag POST "/api/agents/$AGENT/stop" '{}'); T1=$(now_ms); [[ "$c" == 202 ]] || fail "12.$i stop: HTTP $c $(cat "$W/api")"
  await_state "$AGENT" stopped 120 || fail "12.$i the agent is not reported stopped in 120 s: $(cat "$W/api")"
  T2=$(now_ms); sleep 1; kill "$POLLPID" 2>/dev/null; wait "$POLLPID" 2>/dev/null || true
  read -r first acc errs < <(measure "$W/poll" "$T0")
  [[ "$first" -ge 0 ]] || fail "12.$i the key was never refused after the stop: $(tail -3 "$W/poll")"
  echo "$first $acc $((T2 - T0)) $errs" >> "$W/runs"
  [[ $(ag DELETE "/api/agents/$AGENT") == 204 ]] || fail "12.$i delete: $(cat "$W/api")"
done
python3 - "$W/runs" <<'PY' | tee "$W/summary"
import sys
rows = [list(map(int, l.split())) for l in open(sys.argv[1])]
def pct(v, p):
    v = sorted(v); k = (len(v) - 1) * p / 100; lo = int(k); hi = min(lo + 1, len(v) - 1)
    return v[lo] + (v[hi] - v[lo]) * (k - lo)
f = [r[0] for r in rows]; a = [r[1] for r in rows]; s = [r[2] for r in rows]
e = [r[3] for r in rows]
print("MEASURED stop request to first refusal, keys, private Gateway, %d runs: p50 %.0f ms, p95 %.0f ms, p99 %.0f ms, max %d ms; accepted calls after the request (25 ms polling): median %.0f, max %d; calls with no answer or a server error: %d; request to 'stopped': p50 %.1f s, max %.1f s"
      % (len(rows), pct(f, 50), pct(f, 95), pct(f, 99), max(f), pct(a, 50), max(a), sum(e), pct(s, 50) / 1000, max(s) / 1000))
PY
pass "12 $(cat "$W/summary")"

# 13 · the Ambit controller is down: the key is still deleted and refused, but the stop is not confirmed until it is back
new_agent_key 13; poll "$K" "$W/poll13"
kubectl -n "$NS" scale deploy/ambit-controller --replicas=0 >/dev/null; kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=ambit-controller --timeout=120s >/dev/null
T0=$(now_ms); c=$(ag POST "/api/agents/$AGENT/stop" '{}'); [[ "$c" == 202 ]] || fail "13 stop: HTTP $c"
wait_for 60 '[[ $(init_code "$K") != 200 ]]' || fail "13 the key is still accepted 60 s after the stop with the controller down"
sleep 70
[[ $(agent_state "$AGENT") == stopping ]] || fail "13 the agent is reported $(agent_state "$AGENT") while no Gateway confirmed the stop"
[[ $(field 'bool(d.get("stopNotConfirmedAt"))') == True ]] || fail "13 no report that the stop is not confirmed after 60 s: $(cat "$W/api")"
kill "$POLLPID" 2>/dev/null; wait "$POLLPID" 2>/dev/null || true
read -r f13 a13 e13 < <(measure "$W/poll13" "$T0")
kubectl -n "$NS" scale deploy/ambit-controller --replicas=1 >/dev/null
await_state "$AGENT" stopped 180 || fail "13 the agent is not stopped 180 s after the controller came back"
pass "13 controller down: key refused after $f13 ms ($a13 accepted calls in between), stop not confirmed for 70 s (reported), confirmed after it returned"

# 14 · the Ambit controller restarts while a stop is open: it finishes
new_agent_key 14
T0=$(now_ms); c=$(ag POST "/api/agents/$AGENT/stop" '{}'); [[ "$c" == 202 ]] || fail "14 stop: HTTP $c"
kubectl -n "$NS" rollout restart deploy/ambit-controller >/dev/null
await_state "$AGENT" stopped 240 || fail "14 the agent is not stopped after a controller restart"
[[ $(init_code "$K") != 200 ]] || fail "14 the key works after the stop"
pass "14 a controller restart during a stop: the agent is stopped ($(( $(now_ms) - T0 )) ms after the request)"

# 15 · the agentgateway controller is down: the proxies keep their last configuration, so the key keeps working until it is back
AGW=$(kubectl -n "$NS" get deploy -o name | grep -E 'agentgateway$' | head -1)
if [[ -n "$AGW" ]]; then
  new_agent_key 15; poll "$K" "$W/poll15"
  kubectl -n "$NS" scale "$AGW" --replicas=0 >/dev/null; kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=agentgateway --timeout=120s >/dev/null || true
  T0=$(now_ms); c=$(ag POST "/api/agents/$AGENT/stop" '{}'); [[ "$c" == 202 ]] || fail "15 stop: HTTP $c"
  sleep 20
  [[ $(agent_state "$AGENT") == stopping ]] || fail "15 the agent is reported $(agent_state "$AGENT") while the gateway control plane is down"
  read -r f15 a15 e15 < <(measure "$W/poll15" "$T0")
  kubectl -n "$NS" scale "$AGW" --replicas=1 >/dev/null
  await_state "$AGENT" stopped 300 || fail "15 the agent is not stopped after the control plane came back"
  sleep 1; kill "$POLLPID" 2>/dev/null; wait "$POLLPID" 2>/dev/null || true
  read -r g15 b15 e15b < <(measure "$W/poll15" "$T0")
  [[ "$g15" -ge 0 ]] || fail "15 the key was never refused"
  pass "15 gateway control plane down: in the first 20 s the first refusal was $f15 ms ($a15 calls accepted; -1 = none refused); after it returned the first refusal was $g15 ms after the request ($b15 accepted in all)"
else
  pass "15 skipped: no agentgateway controller Deployment found"
fi
done_step agents
