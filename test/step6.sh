#!/usr/bin/env bash
# Step 6 acceptance: offboarding (journey A4). The identity provider deactivates alice over SCIM; by the
# time the request returns, every SaaS grant she authorised is revoked at the provider (confirmed by the
# provider's own session list), her console sessions are over, her still-valid gateway token is refused on
# the MCP and LLM routes, mira is untouched, and a record proves each step. Reactivation lifts the block.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
GRANTS=$NS-grants
KC=http://keycloak.localtest.me:8080
LLM=http://llm.localtest.me:8080/v1/chat/completions
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
connect_saas() {
  hop GET "$BASE/connections/saas/connect"; follow
  local action; action=$(form_action)
  [[ -n "$action" ]] || fail "no saas login form (HTTP $CODE)"
  hop POST "$action" --data-urlencode "username=$1" --data-urlencode "password=$2"; follow
  grep -q 'connected=saas' "$W/all-headers" || fail "connect for $1 did not complete"
}
kc_admin() {
  local tok; tok=$(command curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  command curl -s -X "$1" -H "Authorization: Bearer $tok" "$KC/admin/realms/saas$2"
}
saas_sessions() { # saas_sessions USER: how many sessions the provider still holds for the user
  local uid; uid=$(kc_admin GET "/users?username=$1&exact=true" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
  kc_admin GET "/users/$uid/sessions" | uv run --no-project python -c 'import json,sys;print(len(json.load(sys.stdin)))'
}
scim() { # scim METHOD PATH [JSON]: as the identity provider; body in $W/api
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json')
  [[ $# -ge 3 ]] && a+=(-d "$3")
  command curl "${a[@]}" "$BASE/scim/v2$2"
}
mcp_code() { # HTTP status of an MCP initialize with TOKEN
  command curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"step6","version":"1"}}}' "$MCP"
}
llm_code() { command curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' "$LLM"; }
cleanup() {
  kubectl -n "$NS" delete -f test/connections/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l 'ambitproject.dev/connection' --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l 'ambitproject.dev/scim-user' --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap ambit-offboarded --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
if [[ $(command curl -s -o /dev/null -w '%{http_code}' "$KC/realms/saas/.well-known/openid-configuration") != 200 ]]; then
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
fi
cleanup
kubectl -n "$GRANTS" create secret generic ambit-conn-saas --from-literal=clientSecret=saas-test-client-secret --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" apply -f test/connections/objects.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/whoami --timeout=120s >/dev/null
SCIM_TOKEN=$(kubectl -n "$NS" get secret ambit-console-keys -o jsonpath='{.data.scim-token}' | base64 -d)

# Start from no provider sessions, so "0 after offboarding" proves Ambit's revocation.
for u in alice mira; do
  uid=$(kc_admin GET "/users?username=$u&exact=true" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
  kc_admin POST "/users/$uid/logout" >/dev/null
done

# 0 · alice and mira connect the app; alice's gateway token works before offboarding
sign_in keycloak mira mira-test-pass; connect_saas mira mira-saas-pass
sign_in keycloak alice alice-test-pass; connect_saas alice alice-saas-pass
cp "$W/jar" "$W/alice-jar"
ALICE=$(token alice)
[[ $(mcp_code "$ALICE") == 200 && $(saas_sessions alice) -ge 1 ]] || fail "0 setup: alice MCP $(mcp_code "$ALICE"), provider sessions $(saas_sessions alice)"
pass "0 alice and mira connected; alice's gateway token works; the provider holds $(saas_sessions alice) session(s) for alice"

# 1 · the SCIM endpoint needs its token; the identity provider provisions alice
[[ $(command curl -s -o /dev/null -w '%{http_code}' "$BASE/scim/v2/Users") == 401 ]] || fail "1 SCIM without token"
[[ $(scim POST /Users '{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"alice@acme.test","active":true,"emails":[{"value":"alice@acme.test","primary":true}]}') == 201 ]] ||
  fail "1 provision: $(cat "$W/api")"
ID=$(field 'd["id"]')
pass "1 SCIM needs the bearer token; alice provisioned (id $ID)"

# 2 · A4: deactivation (Entra ID style) offboards everything before the request returns
[[ $(scim PATCH "/Users/$ID" '{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}') == 200 ]] ||
  fail "2 deactivate: $(cat "$W/api")"
[[ $(saas_sessions alice) == 0 ]] || fail "2 the provider still holds $(saas_sessions alice) session(s) for alice"
[[ $(kubectl -n "$GRANTS" get secret -l ambitproject.dev/connection=saas --no-headers | wc -l | tr -d ' ') == 1 ]] || fail "2 grant count"
[[ $(saas_sessions mira) -ge 1 ]] || fail "2 mira's grant was revoked too"
cp "$W/alice-jar" "$W/jar"
[[ $(me_status) == 401 ]] || fail "2 alice's console session still works"
pass "2 deactivation: the provider holds 0 sessions for alice (revoked there), mira's grant intact, alice's console session ended"

# 3 · her still-valid gateway token is refused on both routes (not when it expires)
for _ in $(seq 1 30); do [[ $(mcp_code "$ALICE") == 403 ]] && break; sleep 1; done
m=$(mcp_code "$ALICE"); l=$(llm_code "$ALICE")
[[ "$m" == 403 && "$l" == 403 ]] || fail "3 alice's token: MCP $m, LLM $l (want 403 both)"
[[ $(mcp_code "$(token mira)") == 200 ]] || fail "3 mira blocked too"
pass "3 alice's unexpired gateway token: MCP 403, LLM 403; mira unaffected"

# 4 · the record proves it, and the log has one line per grant
sign_in keycloak priya priya-test-pass
[[ $(api GET /api/offboarding) == 200 ]] || fail "4 records: $(cat "$W/api")"
rec=$(field '[(r["trigger"], r["completeSuccess"], [(g["connection"], g["revokedAtProvider"]) for g in r["grants"]], r["gatewayBlocked"]) for r in d["records"] if r["email"]=="alice@acme.test"][0]')
[[ "$rec" == "('scim PATCH active=false', True, [('saas', True)], True)" ]] || fail "4 record: $rec"
kubectl -n "$NS" logs deploy/ambit-console --since=5m | grep -q 'offboarding: grant.*alice@acme.test.*connection=saas revokedAtProvider=true' || fail "4 no log line for the revoked grant"
pass "4 record: $rec; log line per grant present"

# 5 · reactivation lifts the gateway block
[[ $(scim PATCH "/Users/$ID" '{"Operations":[{"op":"replace","path":"active","value":true}]}') == 200 ]] || fail "5 reactivate"
for _ in $(seq 1 30); do [[ $(mcp_code "$(token alice)") == 200 ]] && break; sleep 1; done
[[ $(mcp_code "$(token alice)") == 200 ]] || fail "5 alice still blocked after reactivation"
pass "5 reactivated: alice's gateway access is back (her app grants stay revoked until she reconnects)"
done_step 6
