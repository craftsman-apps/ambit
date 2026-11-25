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
[[ $(field 'sum(1 for c in d["calls"] if c.get("agent")=="'"$A1"'" and c["agentMode"]=="person" and c["platformDeclared"]=="grok" and c["agentOwnerAtIssue"]=="user09@example.com" and c["ingress"]=="private" and c["keyId"]=="'"$K1ID"'" and c["identity"]=="key" and c["agentOwnerNow"]=="user09@example.com")') -ge 3 ]] || fail "7 agent fields: $(head -c 1200 "$W/api")"
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
       and r[i["agent_owner_now"]] == "user09@example.com" and r[i["platform_declared"]] == "grok" and r[i["ingress"]] == "private"]
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
done_step agents
