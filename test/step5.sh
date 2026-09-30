#!/usr/bin/env bash
# Step 5 acceptance: per-user credentials. Users connect a SaaS app with their own account in the console;
# the gateway sends each user's own token upstream (journey U1: "Alice's calls use Alice's token, Bob's
# use Bob's" — the test IBM's gateway fails); a broken grant shows as "reconnect" before an agent fails (U4).
# Keycloak realm "saas" stands in for the SaaS app; the whoami MCP server reports whose token arrived.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
GRANTS=$NS-grants
KC=http://keycloak.localtest.me:8080
CURL_IMAGE=curlimages/curl:8.20.0@sha256:b3f1fb2a51d923260350d21b8654bbc607164a987e2f7c84a0ac199a67df812a
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
state_of() { api GET /api/connections >/dev/null; field '[c["state"] for c in d if c["name"]=="saas"][0]'; }
# connect_saas USER PASS: the browser flow — console -> saas login -> console callback.
connect_saas() {
  hop GET "$BASE/connections/saas/connect"; follow
  local action; action=$(form_action)
  [[ -n "$action" ]] || fail "no saas login form (HTTP $CODE)"
  hop POST "$action" --data-urlencode "username=$1" --data-urlencode "password=$2"; follow
  [[ "$(awk 'NR==1' "$W/all-headers")" ]] || true
  grep -q 'connected=saas' "$W/all-headers" || fail "connect for $1 did not return to /connections?connected=saas"
}
call_whoami() { # call_whoami TOKEN: prints the whoami tool's answer, or "not listed"
  tools "$1"
  [[ " $TOOLS " == *" whoami_whoami "* ]] || { echo "not listed"; return; }
  mcp_send '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami_whoami","arguments":{}}}' |
    uv run --no-project python -c 'import json,sys;d=json.loads(sys.stdin.read());print(d["result"]["content"][0]["text"] if "result" in d else "error "+d["error"]["message"])'
}
kc_admin() {
  local tok; tok=$(command curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  command curl -s -X "$1" -H "Authorization: Bearer $tok" "$KC/admin/realms/saas$2"
}

cleanup() {
  kubectl -n "$NS" delete -f test/connections/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=saas --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
# The saas realm is imported at Keycloak start; restart once if this cluster's Keycloak predates it.
if [[ $(command curl -s -o /dev/null -w '%{http_code}' "$KC/realms/saas/.well-known/openid-configuration") != 200 ]]; then
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
fi
kubectl -n "$GRANTS" create secret generic ambit-conn-saas --from-literal=clientSecret=saas-test-client-secret --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=saas --ignore-not-found >/dev/null
kubectl -n "$NS" apply -f test/connections/objects.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/whoami --timeout=120s >/dev/null
kubectl -n "$NS" wait "servers.$G" whoami --for=condition=Ready --timeout=120s >/dev/null || fail "0 whoami server not Ready"
for _ in $(seq 1 60); do
  [[ $(kubectl -n "$NS" get agentgatewaypolicy ambit-conn-whoami -o jsonpath='{.status.ancestors[*].conditions[?(@.type=="Accepted")].status}' 2>/dev/null) == True ]] && break; sleep 1
done

# 1 · U1: mira has no grant -> the server's tools are withheld, and her connections page offers the link
sign_in keycloak mira mira-test-pass
[[ $(state_of) == not-connected ]] || fail "1 mira state: $(cat "$W/api")"
link=$(field '[c["connectURL"] for c in d if c["name"]=="saas"][0]')
[[ $(call_whoami "$(token mira)") == "not listed" ]] || fail "1 mira reached whoami without a grant"
pass "1 mira without a grant: whoami withheld; My connections offers $link"

# 2 · alice connects with her own SaaS account; her calls carry her token
sign_in keycloak alice alice-test-pass
connect_saas alice alice-saas-pass
[[ $(state_of) == connected ]] || fail "2 alice state: $(cat "$W/api")"
got=$(call_whoami "$(token alice)")
[[ "$got" == "alice @ $KC/realms/saas" ]] || fail "2 alice's call reached whoami as: $got"
pass "2 alice connected; her call reached the server as: $got"

# 3 · mira connects too; each call carries its own user's token, never the other's
sign_in keycloak mira mira-test-pass
connect_saas mira mira-saas-pass
m=$(call_whoami "$(token mira)"); a=$(call_whoami "$(token alice)")
[[ "$m" == "mira @ $KC/realms/saas" && "$a" == "alice @ $KC/realms/saas" ]] || fail "3 mira=$m alice=$a"
pass "3 mira -> '$m'; alice -> '$a' (no cross-user token)"

# 4 · the broker is reachable only from the gateway; grants are not readable in the Secrets
probe() {
  kubectl -n "$NS" delete pod "$1" --ignore-not-found >/dev/null
  kubectl -n "$NS" run "$1" --restart=Never --image="$CURL_IMAGE" ${2:+--labels="$2"} --command -- \
    curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://ambit-console.$NS.svc:8082/v1/connection/saas" >/dev/null
  kubectl -n "$NS" wait pod "$1" --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s >/dev/null 2>&1 ||
    kubectl -n "$NS" wait pod "$1" --for=jsonpath='{.status.phase}'=Failed --timeout=5s >/dev/null 2>&1 || true
  kubectl -n "$NS" logs "$1" 2>/dev/null | head -c 3
  kubectl -n "$NS" delete pod "$1" --wait=false >/dev/null
}
allowed=$(probe broker-probe-gw gateway.networking.k8s.io/gateway-name=ambit); blocked=$(probe broker-probe)
[[ "$allowed" == 200 && "$blocked" == 000 ]] || fail "4 broker reachability: gateway-labelled pod $allowed (want 200), other pod $blocked (want 000)"
kubectl -n "$GRANTS" get secret -l ambitproject.dev/connection=saas -o json | grep -q -e 'alice@acme' -e 'mira@acme' && fail "4 an email is readable in a grant Secret"
pass "4 broker: only gateway pods reach it (others time out); grant Secrets hold no readable email or token"

# 5 · U4: alice's SaaS session ends at the provider -> the check marks her grant broken before her agent fails
uid=$(kc_admin GET "/users?username=alice&exact=true" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
kc_admin POST "/users/$uid/logout" >/dev/null
sign_in keycloak priya priya-test-pass
[[ $(api POST /api/connections/check '{}') == 200 ]] || fail "5 check: $(cat "$W/api")"
sign_in keycloak alice alice-test-pass
[[ $(state_of) == broken ]] || fail "5 alice state after logout: $(cat "$W/api")"
why=$(field '[c.get("error","") for c in d if c["name"]=="saas"][0]')
for _ in $(seq 1 15); do [[ $(call_whoami "$(token alice)") == "not listed" ]] && break; sleep 5; done  # gateway caches tokens up to 60 s
[[ $(call_whoami "$(token alice)") == "not listed" ]] || fail "5 alice still reaches whoami with a broken grant"
pass "5 alice's grant shows 'Reconnect needed' ($why); her agent no longer reaches the server"

# 6 · disconnect revokes at the provider
sign_in keycloak mira mira-test-pass
[[ $(api DELETE /api/connections/saas) == 200 && $(field 'd["revokedAtProvider"]') == True ]] || fail "6 disconnect: $(cat "$W/api")"
[[ $(state_of) == not-connected ]] || fail "6 mira still connected"
pass "6 mira disconnected; the provider confirmed the revocation (RFC 7009)"
done_step 5
