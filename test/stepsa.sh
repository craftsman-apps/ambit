#!/usr/bin/env bash
# Service accounts (#83, docs/service-accounts.md): an admin creates an account and keys; the gateways serve the
# account as one identity of its group (AccessRules, Budgets, Audit); allSignedIn never grants it; offboarding a
# person leaves it alone except the exposure rule for the admin who issued a key; the reserved domain is refused in
# JWTs, at sign-in and by SCIM; rotation, the emergency switch and deletion work. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
# chat MODEL [curl args]: status of one chat request; the answer is in $W/llm
chat() { local m=$1; shift; command curl -s -o "$W/llm" -w '%{http_code}' --max-time 60 "$LLM" -H 'Content-Type: application/json' "$@" \
  -d "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"; }
field() { uv run --no-project python -c "import json,sys;d=json.load(open(sys.argv[1]));print($1)" "$W/api"; }
scim() { # scim METHOD PATH [JSON]: as the identity provider; body in $W/api
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json')
  [[ $# -ge 3 ]] && a+=(-d "$3")
  command curl "${a[@]}" "$BASE/scim/v2$2"
}
PATCH_ON='{"Operations":[{"op":"replace","path":"active","value":true}]}'
PATCH_OFF='{"Operations":[{"op":"replace","path":"active","value":false}]}'
# scim_user EMAIL: provisions the SCIM user, or finds it (a rerun); sets SCIM_ID
scim_user() {
  local c; c=$(scim POST /Users "{\"schemas\":[\"urn:ietf:params:scim:schemas:core:2.0:User\"],\"userName\":\"$1\",\"active\":true,\"emails\":[{\"value\":\"$1\",\"primary\":true}]}")
  if [[ "$c" == 201 ]]; then SCIM_ID=$(field 'd["id"]'); return; fi
  [[ "$c" == 409 && $(scim GET "/Users?filter=userName%20eq%20%22$1%22") == 200 ]] || fail "scim user $1: $c $(cat "$W/api")"
  SCIM_ID=$(field 'd["Resources"][0]["id"]')
}
# sa METHOD PATH [JSON]: console API as the signed-in user, retried while a console takes over the key Lease (503)
sa() { local c; for _ in $(seq 1 15); do c=$(api "$@"); [[ "$c" == 503 ]] || break; sleep 2; done; echo "$c"; }
# sakey ACCOUNT KEYNAME: a key for the service account, as the signed-in admin; sets KEY and KID
sakey() {
  local c; c=$(sa POST "/api/admin/service-accounts/$1/keys" "{\"name\":\"$2\",\"scope\":\"both\",\"days\":30}")
  [[ "$c" == 201 ]] || fail "key $2 for $1: HTTP $c $(cat "$W/api")"
  KEY=$(field 'd["key"]'); KID=$(field 'd["view"]["id"]')
}
past() { date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ; }
minute() { echo $(( $(date -u +%s) / 60 * 60 )); }
new_minute() { sleep $(( 65 - $(date -u +%s) % 60 )); } # second 5 of the next minute (see step 8, case 20)
flush() { kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true; }
admin() { sign_in local admin@ambit.local "$PW" >/dev/null || fail "bootstrap admin sign-in"; }

ACCOUNTS=(); DEACT=(); DISABLED=""
cleanup() {
  kubectl -n "$NS" delete "accessrules.$G" sa-eng-echo sa-everyone-free sa-user-free --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" sa-eng --ignore-not-found >/dev/null 2>&1 || true
  [[ -n "$DISABLED" ]] && helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null 2>&1 || true
  for id in ${DEACT[@]+"${DEACT[@]}"}; do wait_for 60 '[[ $(scim PATCH "/Users/$id" "$PATCH_ON") == 200 ]]' >/dev/null 2>&1 || true; done
  if [[ -n "${PW:-}" ]] && sign_in local admin@ambit.local "$PW" >/dev/null 2>&1; then
    for n in ${ACCOUNTS[@]+"${ACCOUNTS[@]}"}; do api DELETE "/api/admin/service-accounts/$n" >/dev/null 2>&1 || true; done
    for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/admin/keys/$id" >/dev/null 2>&1 || true; done
  fi
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

ensure_llm_setup "$NS"
# The Keycloak realm is imported once, at its first start: restart it when the realm lacks this test's users.
if [[ -z "$(token svcfake 2>/dev/null || true)" || -z "$(token keytest 2>/dev/null || true)" ]]; then
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
  wait_for 120 '[[ -n "$(token svcfake 2>/dev/null)" && -n "$(token keytest 2>/dev/null)" ]]' || fail "0 keycloak realm lacks the users svcfake or keytest"
fi
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null && mcp_ready "$NS"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: sa-eng-echo }
spec: { groups: [eng], models: [mock-echo] }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: sa-everyone-free }
spec: { allSignedIn: true, models: [mock-free] }
EOF
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
SCIM_TOKEN=$(kubectl -n "$NS" get secret ambit-console-keys -o jsonpath='{.data.scim-token}' | base64 -d)
admin
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "0 reset-limits hook: $(cat "$W/api")"
NAME="ci-test-$(date +%s | tail -c 7)"; ID="$NAME@svc.ambit.internal" # names are never reused: one per run
c=$(sa POST /api/admin/service-accounts "{\"name\":\"$NAME\",\"group\":\"eng\",\"description\":\"test/stepsa.sh\"}")
[[ "$c" == 201 ]] || fail "0 create $NAME: HTTP $c $(cat "$W/api")"
ACCOUNTS+=("$NAME")
sakey "$NAME" first; K1=$KEY; K1ID=$KID

# 1 · the key calls a model and an MCP tool as the account; Audit shows the identity, the type and the key ID
wait_for 90 '[[ $(chat mock/echo -H "Authorization: Bearer $K1") == 200 ]]' || fail "1 model call with the account's key: $(cat "$W/llm")"
wait_for 60 'try_tools "$K1"; [[ " $TOOLS " == *" time_get_current_time "* ]]' || fail "1 the account (group eng) does not see eng's tool: $TOOLS"
tools "$K1" # try_tools opens its session in a subshell; call needs one here
r=$(call time_get_current_time '{"timezone":"UTC"}'); [[ "$r" == ok* ]] || fail "1 MCP tool call: $r"
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
sarecs() {
  [[ $(api GET "/api/audit?email=$ID&from=$day&to=$next") == 200 ]] || return 1
  field 'sum(1 for c in d["calls"] if c.get("identity")=="service account" and c.get("keyId")=="'"$K1ID"'" and c.get("email")=="'"$ID"'")'
}
wait_for 120 '[[ $(sarecs) -ge 2 ]]' || fail "1 no Audit records for $ID with identity 'service account' and key $K1ID: $(head -c 800 "$W/api")"
[[ $(field 'sum(1 for c in d["calls"] if c.get("identity")=="key" or c.get("subject"))') == 0 ]] || fail "1 a record of the account says 'key' or carries a subject"
pass "1 $ID: model call 200, MCP tool time_get_current_time ok; Audit: $(sarecs) records as 'service account' with key $K1ID"

# 2 · allSignedIn does not grant a service account (SA-8 B); a rule naming the identity does; removing it refuses again
wait_for 120 '[[ $(chat mock/free -H "Authorization: Bearer $(token user01)") == 200 ]]' || fail "2 allSignedIn must grant user01: $(cat "$W/llm")"
[[ $(chat mock/free -H "Authorization: Bearer $K1") == 403 ]] || fail "2 allSignedIn granted the service account: $(cat "$W/llm")"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: sa-user-free }
spec: { users: [$ID], models: [mock-free] }
EOF
wait_for 120 '[[ $(chat mock/free -H "Authorization: Bearer $K1") == 200 ]]' || fail "2 users: [$ID] does not grant the account: $(cat "$W/llm")"
kubectl -n "$NS" delete "accessrules.$G" sa-user-free >/dev/null
wait_for 120 '[[ $(chat mock/free -H "Authorization: Bearer $K1") == 403 ]]' || fail "2 without the users rule the account still gets mock-free"
[[ $(chat mock/free -H "Authorization: Bearer $(token user01)") == 200 ]] || fail "2 user01 lost allSignedIn"
pass "2 allSignedIn: user01 200, the account 403; with users: [$ID] 200; without it 403 again"

# 3 · Budgets: userRequestsPerMinute counts the account as one person of eng, with its own counter
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: sa-eng }
spec: { group: eng, userRequestsPerMinute: 2 }
EOF
wait_rl "$NS" "ambit-llm.rpm_group_eng.rpm_user: unit=MINUTE requests_per_unit=2,"
flush; A=$(token user01)
new_minute; m=$(minute); codes=""
for _ in 1 2 3; do codes+="$(chat mock/echo -H "Authorization: Bearer $K1") "; done
p=$(chat mock/echo -H "Authorization: Bearer $A")
used=$(kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli GET "ambit-llm_rpm_group_eng_rpm_user_${ID}_$m" 2>/dev/null | tr -d '\r')
[[ "$codes" == "200 200 429 " && "$p" == 200 && "$used" == 3 ]] || fail "3 per minute 2: account $codes(want 200 200 429), user01 $p (want 200), counter $used (want 3)"
kubectl -n "$NS" delete "budgets.$G" sa-eng >/dev/null; flush
pass "3 userRequestsPerMinute 2: the account $codes; user01 in eng still $p; the account's own counter: $used"

# 4 · offboarding a person of the account's group deletes their key and leaves the account's key working
sign_in keycloak keytest keytest-test-pass || fail "4 keytest sign-in"
mkkey sa-keytest llm; KT=$KEY
wait_for 60 '[[ $(chat mock/echo -H "Authorization: Bearer $KT") == 200 ]]' || fail "4 keytest's key: $(cat "$W/llm")"
admin
scim_user keytest@example.com; KTSCIM=$SCIM_ID; DEACT+=("$KTSCIM")
wait_for 60 '[[ $(scim PATCH "/Users/$KTSCIM" "$PATCH_OFF") == 200 ]]' || fail "4 deactivate keytest: $(cat "$W/api")"
wait_for 10 '[[ $(chat mock/echo -H "Authorization: Bearer $KT") == 403 ]]' || fail "4 keytest's key still works"
sleep 2
[[ $(chat mock/echo -H "Authorization: Bearer $K1") == 200 ]] || fail "4 the account's key stopped when keytest was offboarded: $(cat "$W/llm")"
[[ $(sa POST /api/test/keys/repair '{}') == 204 ]] || fail "4 repair hook: $(cat "$W/api")"
[[ $(chat mock/echo -H "Authorization: Bearer $K1") == 200 ]] || fail "4 the repair stopped the account's key: $(cat "$W/llm")"
wait_for 60 '[[ $(scim PATCH "/Users/$KTSCIM" "$PATCH_ON") == 200 ]]' || fail "4 reactivate keytest"
DEACT=()
pass "4 keytest (eng) offboarded: their key 403; the account's key 200 after 2 s and after a repair"

# 5 · SCIM cannot create a user in the service account domain; the block list is unchanged
before=$(kubectl -n "$NS" get configmap ambit-offboarded -o jsonpath='{.data.emails}' 2>/dev/null || true)
c=$(scim POST /Users "{\"schemas\":[\"urn:ietf:params:scim:schemas:core:2.0:User\"],\"userName\":\"$ID\",\"active\":false,\"emails\":[{\"value\":\"$ID\",\"primary\":true}]}")
[[ "$c" == 400 ]] || fail "5 SCIM create of $ID: $c $(cat "$W/api")"
after=$(kubectl -n "$NS" get configmap ambit-offboarded -o jsonpath='{.data.emails}' 2>/dev/null || true)
[[ "$before" == "$after" ]] || fail "5 the block list changed: $before -> $after"
[[ $(chat mock/echo -H "Authorization: Bearer $K1") == 200 ]] || fail "5 the account's key stopped"
pass "5 SCIM POST /Users $ID (active=false): 400; block list unchanged; the key works"

# 6 · a token whose email is in the reserved domain: the gateway refuses it, and so does the console sign-in
T=$(token svcfake) || fail "6 no token for svcfake"
[[ $(chat mock/echo -H "Authorization: Bearer $T") == 403 ]] || fail "6 a JWT with svcfake@svc.ambit.internal was accepted: $(cat "$W/llm")"
[[ $(chat mock/echo -H "Authorization: Bearer $(token user01)") == 200 ]] || fail "6 control: user01's JWT (same group eng) is refused"
sign_in keycloak svcfake svcfake-test-pass || true
[[ "$CODE" == 403 ]] && grep -q "reserved for service accounts" "$W/body" || fail "6 console sign-in of svcfake: HTTP $CODE $(head -c 300 "$W/body")"
admin
pass "6 svcfake@svc.ambit.internal (group eng): JWT 403 at the gateway (user01 200); console sign-in 403"

# 7 · rotation: a second key, then the first is revoked; the old key is refused, the new one works
sakey "$NAME" second; K2=$KEY; K2ID=$KID
wait_for 60 '[[ $(chat mock/echo -H "Authorization: Bearer $K2") == 200 ]]' || fail "7 the new key: $(cat "$W/llm")"
[[ $(sa POST "/api/admin/service-accounts/$NAME/keys" '{"name":"third","scope":"llm","days":30}') == 429 ]] || fail "7 a third key: $(cat "$W/api")"
[[ $(sa DELETE "/api/admin/keys/$K1ID") == 204 ]] || fail "7 revoke the first key: $(cat "$W/api")"
wait_for 10 '[[ $(chat mock/echo -H "Authorization: Bearer $K1") == 403 ]]' || fail "7 the revoked key still works"
[[ $(chat mock/echo -H "Authorization: Bearer $K2") == 200 ]] || fail "7 the new key stopped"
pass "7 rotation: second key 200, a third refused (429), the first revoked and refused"

# 8 · exposure: the admin who issued a key is offboarded; that key stops within exposureDays (7); the key another
#     admin issued is unchanged. The test hook then moves the exposed key's end to the past.
sign_in keycloak user10 user10-test-pass || fail "8 user10 sign-in"
sakey "$NAME" by-user10; K3=$KEY; K3ID=$KID
wait_for 60 '[[ $(chat mock/echo -H "Authorization: Bearer $K3") == 200 ]]' || fail "8 user10's key for the account: $(cat "$W/llm")"
admin
scim_user user10@example.com; U10=$SCIM_ID; DEACT+=("$U10")
wait_for 60 '[[ $(scim PATCH "/Users/$U10" "$PATCH_OFF") == 200 ]]' || fail "8 deactivate user10: $(cat "$W/api")"
[[ $(sa GET /api/admin/service-accounts) == 200 ]] || fail "8 list: $(cat "$W/api")"
left=$(field '[(__import__("datetime").datetime.fromisoformat(k["until"].replace("Z","+00:00"))-__import__("datetime").datetime.now(__import__("datetime").timezone.utc)).total_seconds() for a in d["accounts"] if a["name"]=="'"$NAME"'" for k in a["keys"] if k["id"]=="'"$K3ID"'"][0]')
warn=$(field '" | ".join(w for a in d["accounts"] if a["name"]=="'"$NAME"'" for w in a["warnings"])')
k2until=$(field '[k["until"] for a in d["accounts"] if a["name"]=="'"$NAME"'" for k in a["keys"] if k["id"]=="'"$K2ID"'"][0]')
(( ${left%.*} > 7*86400 - 600 && ${left%.*} <= 7*86400 )) || fail "8 the exposed key stops in $left s, want about 7 days"
[[ "$warn" == *"was issued by a person who was offboarded. It stops on"* ]] || fail "8 the account shows no exposure warning: $warn"
[[ $(chat mock/echo -H "Authorization: Bearer $K3") == 200 ]] || fail "8 the exposed key stopped at once (exposureDays is 7)"
[[ $(sa POST "/api/test/keys/$K3ID/expire" "{\"until\":\"$(past)\"}") == 204 ]] || fail "8 expire hook: $(cat "$W/api")"
wait_for 10 '[[ $(chat mock/echo -H "Authorization: Bearer $K3") == 403 ]]' || fail "8 the exposed key still works after its end"
[[ $(chat mock/echo -H "Authorization: Bearer $K2") == 200 ]] || fail "8 the key the bootstrap admin issued stopped: $(cat "$W/llm")"
wait_for 60 '[[ $(scim PATCH "/Users/$U10" "$PATCH_ON") == 200 ]]' || fail "8 reactivate user10"
DEACT=()
pass "8 user10 offboarded: their key stops in $(( ${left%.*} / 3600 )) h (warning: ${warn:0:90}...); at its end 403; the other admin's key ($k2until) 200"

# 9 · serviceAccounts.enabled=false: service account keys refused (kept), creation 409, a personal key still works
mkkey sa-control llm; PK=$KEY
wait_for 60 '[[ $(chat mock/free -H "Authorization: Bearer $PK") == 200 ]]' || fail "9 the admin's personal key: $(cat "$W/llm")"
DISABLED=1
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set serviceAccounts.enabled=false --wait --timeout 5m >/dev/null
sso_settled "$NS"; admin
wait_for 90 '[[ $(chat mock/echo -H "Authorization: Bearer $K2") =~ ^40[13]$ ]]' || fail "9 with service accounts off the key is accepted: $(cat "$W/llm")"
[[ $(chat mock/free -H "Authorization: Bearer $PK") == 200 ]] || fail "9 with service accounts off a personal key is refused: $(cat "$W/llm")"
[[ $(sa POST /api/admin/service-accounts '{"name":"off-test","group":"eng","description":""}') == 409 ]] || fail "9 create while off: $(cat "$W/api")"
kubectl -n "$NS" get secret "ambit-key-$K2ID" >/dev/null || fail "9 the key Secret was deleted"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
DISABLED=""
sso_settled "$NS"; admin
wait_for 90 '[[ $(chat mock/echo -H "Authorization: Bearer $K2") == 200 ]]' || fail "9 switched on again, the key is refused: $(cat "$W/llm")"
pass "9 serviceAccounts.enabled=false: the account's key refused, kept; creation 409; a personal key 200; on again: 200"

# 10 · delete: every key is refused within seconds; the name is never reused
t0=$SECONDS
[[ $(sa DELETE "/api/admin/service-accounts/$NAME") == 204 ]] || fail "10 delete: $(cat "$W/api")"
wait_for 10 '[[ $(chat mock/echo -H "Authorization: Bearer $K2") == 403 ]]' || fail "10 the deleted account's key still works"
took=$(( SECONDS - t0 ))
[[ $(sa POST /api/admin/service-accounts "{\"name\":\"$NAME\",\"group\":\"eng\",\"description\":\"\"}") == 409 ]] || fail "10 the name was reused: $(cat "$W/api")"
[[ $(sa GET /api/admin/key-events) == 200 && $(field 'sum(1 for e in d["events"] if e["email"]=="'"$ID"'" and e["action"] in ("sa-create","sa-delete","sa-delete-key","exposed"))') -ge 4 ]] ||
  fail "10 History lacks sa-create, sa-delete, sa-delete-key or exposed for $ID"
pass "10 deleted: the key refused within $took s; creating $NAME again: 409; History has the account's events"
done_step sa
