#!/usr/bin/env bash
# Connection consent (docs/agents.md, experimental): an agent gets its owner's connection token only for the apps the owner
# ticked for it, checked on every request by the token broker from the agent the gateway names; a client's own
# x-ambit-agent header has no effect. The SaaS side is the test realm of step 5 (users user09 and user01). Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
GRANTS=$NS-grants
KC=http://keycloak.localtest.me:8080
field() { uv run --no-project python -c "import json,sys;d=json.load(open(sys.argv[1]));print($1)" "$W/api"; }
ag() { local c; for _ in $(seq 1 15); do c=$(api "$@"); [[ "$c" == 503 ]] || break; sleep 2; done; echo "$c"; }
call_whoami() { # call_whoami TOKEN: prints the whoami tool's answer, or "not listed"
  tools "$1"
  [[ " $TOOLS " == *" whoami_whoami "* ]] || { echo "not listed"; return; }
  mcp_send '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami_whoami","arguments":{}}}' |
    uv run --no-project python -c 'import json,sys;d=json.loads(sys.stdin.read());print(d["result"]["content"][0]["text"] if "result" in d else "error "+d["error"]["message"])'
}
kc_admin() {
  local tok; tok=$(command curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  command curl -s -X "$1" -H "Authorization: Bearer $tok" "${@:3}" "$KC/admin/realms/saas$2"
}
# who_as TOKEN [EXTRA-HEADER]: the whoami tool's answer for a session opened with TOKEN (a key or a token) and one more
# request header on every call, or "not listed". The extra header is what a client could add on its own.
who_as() {
  local tok=$1 extra=${2:-}
  local -a h=(-H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream'); [[ -n "$extra" ]] && h+=(-H "$extra")
  command curl -s -D "$W/mh" -o /dev/null "${h[@]}" -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"consent","version":"1"}}}' "$MCP"
  local sid; sid=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/mh" | tr -d '\r'); [[ -n "$sid" ]] || { echo "no session"; return; }
  h+=(-H "Mcp-Session-Id: $sid")
  command curl -s -o /dev/null "${h[@]}" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$MCP"
  local names; names=$(command curl -s "${h[@]}" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "$MCP" | awk '/^data: /{print substr($0, 7); next} /^\{/{print}' |
    uv run --no-project python -c '
import json,sys
try: print(" ".join(t["name"] for t in json.loads(sys.stdin.read())["result"]["tools"]))
except Exception: print("")')
  [[ " $names " == *" whoami_whoami "* ]] || { echo "not listed"; return; }
  command curl -s "${h[@]}" -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami_whoami","arguments":{}}}' "$MCP" | awk '/^data: /{print substr($0, 7); next} /^\{/{print}' |
    uv run --no-project python -c '
import json,sys
try:
    d=json.loads(sys.stdin.read()); print(d["result"]["content"][0]["text"] if "result" in d else "error "+d["error"]["message"])
except Exception as e: print("error no answer")'
}
AGENTS=(); KEYIDS=()
cleanup() {
  kubectl -n "$NS" delete "accessrules.$G" whoami-agents --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete -f test/connections/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=saas --ignore-not-found >/dev/null 2>&1 || true
  local pw; pw=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true
  if sign_in local admin@ambit.local "$pw" >/dev/null 2>&1; then
    for n in ${AGENTS[@]+"${AGENTS[@]}"}; do api DELETE "/api/agents/$n" >/dev/null 2>&1 || true; done
  fi
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

ensure_llm_setup "$NS"
# The saas realm is imported at Keycloak start; restart once if this cluster's Keycloak predates it.
if [[ $(command curl -s -o /dev/null -w '%{http_code}' "$KC/realms/saas/.well-known/openid-configuration") != 200 ]]; then
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
fi
# Realms are imported only when missing: a realm from before the other-app client or the password grant (step 5, presets) is deleted and re-imported.
if [[ $(command curl -s -o /dev/null -w '%{http_code}' -d grant_type=client_credentials -d client_id=other-app -d client_secret=other-app-secret "$KC/realms/saas/protocol/openid-connect/token") != 200 ||
      $(command curl -s -o /dev/null -w '%{http_code}' -d grant_type=password -d client_id=ambit-connect -d client_secret=saas-test-client-secret -d username=user01 -d password=user01-saas-pass "$KC/realms/saas/protocol/openid-connect/token") != 200 ]]; then
  [[ $(kc_admin DELETE "" -o /dev/null -w '%{http_code}') == 204 ]] || fail "could not delete the old saas realm"
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
  wait_for 120 "command curl -sf $KC/realms/saas/.well-known/openid-configuration" || fail "saas realm not re-imported"
fi
# Keycloak makes new signing keys on every restart; the gateway reloads them within a few minutes.
wait_for 400 'try_tools "$(token user09)"; [[ "$TOOLS" != "<error>" ]]' || fail "0 the gateway does not accept Keycloak tokens"
kubectl -n "$GRANTS" create secret generic ambit-conn-saas --from-literal=clientSecret=saas-test-client-secret --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=saas --ignore-not-found >/dev/null
kubectl -n "$NS" apply -f test/connections/objects.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/whoami --timeout=120s >/dev/null
kubectl -n "$NS" wait "servers.$G" whoami --for=condition=Ready --timeout=120s >/dev/null || fail "0 whoami server not Ready"
for _ in $(seq 1 60); do
  [[ $(kubectl -n "$NS" get agentgatewaypolicy ambit-conn-whoami -o jsonpath='{.status.ancestors[*].conditions[?(@.type=="Accepted")].status}' 2>/dev/null) == True ]] && break; sleep 1
done

# 1 · user09 connects the app with her own account; her own token reaches whoami as herself
sign_in keycloak user09 user09-test-pass
connect_app saas user09 user09-saas-pass
for _ in $(seq 1 20); do got=$(who_as "$(token user09)"); [[ "$got" != "not listed" ]] && break; sleep 3; done
[[ "$got" == *user09* ]] || fail "1 user09's own token does not reach whoami as her: $got"
pass "1 user09's own token: whoami answers for user09 ($got)"

# 2 · an agent of user09 with the server in its scope but no consent: the server is withheld
c=$(ag POST /api/agents '{"displayName":"consent test","platform":"grok","scope":["whoami"]}'); [[ "$c" == 201 ]] || fail "2 create agent: HTTP $c $(cat "$W/api")"
A=$(field 'd["name"]'); AGENTS+=("$A")
c=$(ag POST "/api/agents/$A/keys" '{"name":"k","days":30}'); [[ "$c" == 201 ]] || fail "2 key: HTTP $c $(cat "$W/api")"
K=$(field 'd["key"]')
wait_for 90 '[[ $(who_as "$K") != "no session" ]]' || fail "2 the agent key is not accepted"
[[ $(who_as "$K") == "not listed" ]] || fail "2 an agent without consent reaches whoami: $(who_as "$K")"
pass "2 the agent has whoami in its scope but no consent: whoami is not listed for it"

# 3 · a client's own x-ambit-agent header changes nothing, in either direction
[[ $(who_as "$K" 'x-ambit-agent: -') == "not listed" ]] || fail "3 a client header '-' made the agent a person"
[[ $(who_as "$K" "x-ambit-agent: $A") == "not listed" ]] || fail "3 a client header with the agent's name changed the answer"
[[ $(who_as "$(token user09)" "x-ambit-agent: $A") == *user09* ]] || fail "3 a person's token with a forged agent header lost its connection"
[[ $(who_as "$(token user09)" 'x-ambit-agent: nobody') == *user09* ]] || fail "3 a person's token with another agent header lost its connection"
pass "3 x-ambit-agent sent by the client (as '-', as the agent's name, as another name) has no effect: the gateway names the agent from the key"

# 4 · the owner ticks the connection: the agent reaches whoami as user09; she unticks it: gone again
[[ $(ag PATCH "/api/agents/$A" '{"connectionConsent":["saas"]}') == 200 ]] || fail "4 consent: $(cat "$W/api")"
for _ in $(seq 1 20); do got=$(who_as "$K"); [[ "$got" != "not listed" ]] && break; sleep 3; done
[[ "$got" == *user09* ]] || fail "4 with consent the agent does not reach whoami as user09: $got"
[[ $(ag PATCH "/api/agents/$A" '{"connectionConsent":[]}') == 200 ]] || fail "4 remove consent"
for _ in $(seq 1 20); do [[ $(who_as "$K") == "not listed" ]] && break; sleep 3; done
[[ $(who_as "$K") == "not listed" ]] || fail "4 the agent still reaches whoami after the consent was removed"
# 4b · a session that already called whoami loses the connection when the consent goes (the broker is asked on every call)
[[ $(ag PATCH "/api/agents/$A" '{"connectionConsent":["saas"]}') == 200 ]] || fail "4b consent again"
for _ in $(seq 1 20); do got=$(who_as "$K"); [[ "$got" != "not listed" ]] && break; sleep 3; done
mcp_open "$K"
r=rror no answer; [[ "$r" == ok*user09* ]] || fail "4b the open session cannot call whoami with consent: $r"
[[ $(ag PATCH "/api/agents/$A" '{"connectionConsent":[]}') == 200 ]] || fail "4b remove consent"
for _ in $(seq 1 30); do r=rror no answer; [[ "$r" != ok*user09* ]] && break; sleep 2; done
[[ "$r" != ok*user09* ]] || fail "4b the open session still reaches whoami as user09 after the consent was removed: $r"
pass "4b the session that had called whoami is refused after the consent was removed ($r)"
pass "4 consent ticked: whoami answers as user09 ($got); consent removed: not listed again (checked on every request, no cache of the grant decision beyond the gateway's 60 s)"

# 5 · an agent whose owner did not connect gets nothing even with consent
sign_in keycloak user01 user01-test-pass
c=$(ag POST /api/agents '{"displayName":"consent test two","scope":["whoami"],"connectionConsent":["saas"]}'); [[ "$c" == 201 ]] || fail "5 create agent: HTTP $c $(cat "$W/api")"
B=$(field 'd["name"]'); AGENTS+=("$B")
c=$(ag POST "/api/agents/$B/keys" '{"name":"k","days":30}'); K2=$(field 'd["key"]')
wait_for 90 '[[ $(who_as "$K2") != "no session" ]]' || fail "5 the key is not accepted"
[[ $(who_as "$K2") == "not listed" ]] || fail "5 consent without the owner's own connection gave access: $(who_as "$K2")"
pass "5 consent alone gives nothing: the owner (user01) has not connected the app, so the agent gets no token"
done_step agentconn
