#!/usr/bin/env bash
# Personal API keys: the gateway accepts a key exactly like its owner's JWT, refuses every invalid credential, never
# forwards the key, records one identity, and only the console writes key objects. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
KC=http://keycloak.localtest.me:8080
NC=system:serviceaccount:kube-system:namespace-controller
body='{"model":"mock/echo","messages":[{"role":"user","content":"hi"}]}'
# code [curl args]: HTTP status of one chat request; the answer is in $W/llm
code() { command curl -s -o "$W/llm" -w '%{http_code}' "$LLM" -H 'Content-Type: application/json' "$@" -d "$body"; }
seen_key() { uv run --no-project python -c 'import json,sys;print(json.load(open(sys.argv[1]))["x_seen"]["ambit_value"])' "$W/llm"; }
field() { uv run --no-project python -c "import json,sys;d=json.load(open(sys.argv[1]));print($1)" "$W/api"; }
scim() { # scim METHOD PATH [JSON]: as the identity provider; body in $W/api
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json')
  [[ $# -ge 3 ]] && a+=(-d "$3")
  command curl "${a[@]}" "$BASE/scim/v2$2"
}
SCIM_ID=""; DEACTIVATED=""; DISABLED=""
PATCH_ON='{"Operations":[{"op":"replace","path":"active","value":true}]}'
PATCH_OFF='{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}'
past() { date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ; }
cleanup() {
  kubectl -n "$NS" delete "accessrules.$G" keys-everyone-mock keys-eng-mock --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "budgets.$G" keys-mira --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete secret forge-test forge-scope --ignore-not-found >/dev/null 2>&1 || true
  [[ -n "$DISABLED" ]] && helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null 2>&1 || true
  # R3-3: reactivation clears the block list and the owner flag (the console answers 503 until it holds the key Lease)
  [[ -n "$DEACTIVATED" ]] && { wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_OFF") == 200 ]]' && wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]]'; } >/dev/null 2>&1 || true
  local pw; pw=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true
  if sign_in local admin@ambit.local "$pw" >/dev/null 2>&1; then
    for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/admin/keys/$id" >/dev/null 2>&1 || true; done
  fi
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

ensure_llm_setup "$NS"
# The Keycloak realm is imported once, at its first start: restart it when the realm lacks this test's users.
if [[ -z "$(token keytest 2>/dev/null || true)" || -z "$(token noemail 2>/dev/null || true)" ]]; then
  kubectl -n "$NS" rollout restart deploy/keycloak >/dev/null && kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
  wait_for 120 '[[ -n "$(token keytest 2>/dev/null)" && -n "$(token noemail 2>/dev/null)" ]]' || fail "0 keycloak realm lacks the users keytest or noemail"
fi
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: keys-everyone-mock }
spec: { allSignedIn: true, models: [mock-echo] }
EOF
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
SCIM_TOKEN=$(kubectl -n "$NS" get secret ambit-console-keys -o jsonpath='{.data.scim-token}' | base64 -d)
# a run that stopped half way may have left keytest offboarded: reactivate it first
if [[ $(scim GET '/Users?filter=userName%20eq%20%22keytest@acme.test%22') == 200 ]]; then
  OLD_ID=$(field 'd["Resources"][0]["id"] if d.get("Resources") else ""')
  # ponytail: a failed deactivation (503) can leave the block set with the user still active, and PATCH active=true on an
  # active user changes nothing; deactivating again, then reactivating, completes and clears it.
  [[ -z "$OLD_ID" ]] || { wait_for 60 '[[ $(scim PATCH "/Users/$OLD_ID" "$PATCH_OFF") == 200 ]]' && wait_for 60 '[[ $(scim PATCH "/Users/$OLD_ID" "$PATCH_ON") == 200 ]]'; } || true
fi
sign_in local admin@ambit.local "$PW"
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "0 reset-limits hook: $(cat "$W/api")"
mkkey llm-key llm; K=$KEY; KID_K=$KID
mkkey mcp-key mcp; M=$KEY; MID=$KID
wait_for 60 '[[ $(code -H "Authorization: Bearer $K") == 200 ]]' || fail "0 a new key is not accepted: $(cat "$W/llm")"

# 1 · allowed in both headers; route filters run after authentication: the model is reached and never sees an ambit_ value
[[ $(code -H "Authorization: Bearer $K") == 200 && $(seen_key) == False ]] || fail "1 bearer key: $(cat "$W/llm")"
[[ $(code -H "x-api-key: $K") == 200 && $(seen_key) == False ]] || fail "1 x-api-key: $(cat "$W/llm")"
pass "1 a key in either header reaches the model; the model received no ambit_ value ($(uv run --no-project python -c 'import json,sys;print(json.load(open(sys.argv[1]))["x_seen"]["headers"])' "$W/llm"))"

# 2 · refused: no credential, wrong key, broken JWT, wrong-scope key, valid JWT + wrong key
TOK=$(token alice)
# First prove a valid JWT works (after a Keycloak restart the gateway holds the old keys until the controller
# refreshes them, every 5 minutes), so the JWT refusals below cannot pass merely because every JWT is rejected.
wait_for 400 '[[ $(code -H "Authorization: Bearer $TOK") == 200 ]]' || fail "2 alice's JWT alone must work: $(cat "$W/llm")"
[[ $(code) =~ ^40[13]$ ]] || fail "2 no credential: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer ambit_wrongwrongwrongwrongwrongwrongwrongwrongwro") == 403 ]] || fail "2 wrong key: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.e30.x") == 403 ]] || fail "2 broken JWT: $(cat "$W/llm")"
[[ $(code -H "x-api-key: $M") == 403 ]] || fail "2 an MCP-scoped key reached the LLM gateway: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer $TOK" -H "x-api-key: ambit_wrong") == 403 ]] || fail "2 JWT + wrong key: $(cat "$W/llm")"
pass "2 no credential, wrong key, broken JWT, wrong-scope key, JWT + wrong key: refused; JWT alone: allowed"

# 3 · expiry is checked by the gateway on every request (the test hook sets until to the past, through the console)
[[ $(api POST "/api/test/keys/$KID_K/expire" "{\"until\":\"$(past)\"}") == 204 ]] || fail "3 expire hook: $(cat "$W/api")"
wait_for 10 '[[ $(code -H "Authorization: Bearer $K") == 403 ]]' || fail "3 an expired key still works: $(cat "$W/llm")"
pass "3 an expired key is refused within 10 s"

# 4 · only the Lease-holding console writes key objects; namespace cleanup can still delete them
kubectl -n "$NS" create secret generic forge-test --from-literal=key=x >/dev/null
if kubectl -n "$NS" label secret forge-test ambitproject.dev/key=true ambitproject.dev/key-llm=true 2>"$W/err"; then
  fail "4a relabelling a Secret into a key was admitted"
fi
grep -q "only the Ambit console may write personal API key objects" "$W/err" || fail "4a wrong refusal: $(cat "$W/err")"
kubectl -n "$NS" delete secret forge-test >/dev/null
! kubectl -n "$NS" delete secret "ambit-key-$MID" 2>"$W/err" || fail "4a an admin's kubectl deleted a key directly"
grep -q "only the Ambit console may write personal API key objects" "$W/err" || fail "4a direct delete refused for the wrong reason: $(cat "$W/err")"
pass "4a a Secret relabelled into a key, and a direct delete of a key, are refused by admission"
# 4b · a Secret with only the scope label (not protected) holding the hash of a known value gives no access
FORGED=ambit_forgedforgedforgedforgedforgedforgedforged1
HASH=$(printf %s "$FORGED" | shasum -a 256 | cut -d' ' -f1)
kubectl -n "$NS" create secret generic forge-scope --from-literal=key="{\"keyHash\":\"sha256:$HASH\",\"metadata\":{\"keyId\":\"forged\",\"email\":\"admin@ambit.local\",\"groups\":[],\"until\":\"2099-01-01T00:00:00Z\"}}" >/dev/null
kubectl -n "$NS" label secret forge-scope ambitproject.dev/key-llm=true >/dev/null
# Positive control: a real key made after the forged Secret is read through the same selectors; once it is accepted,
# the forged Secret has had the time to load too.
mkkey control-key llm
wait_for 90 '[[ $(code -H "Authorization: Bearer $KEY") == 200 ]]' || fail "4b the control key is not accepted: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer $FORGED") == 403 ]] || fail "4b a Secret with only the scope label was accepted as a key: $(cat "$W/llm")"
kubectl -n "$NS" delete secret forge-scope >/dev/null
pass "4b a Secret labelled only key-llm, holding a known hash, gives 403"
# 4c · no Lease: unrelated writes work, and the namespace controller can still delete a key Secret
kubectl -n "$NS" delete lease ambit-key-writer --as="$NC" >/dev/null || fail "4c delete the Lease as the namespace controller"
kubectl -n "$NS" create secret generic "unrelated-$$" --from-literal=a=b >/dev/null || fail "4c an unrelated Secret write failed without the Lease"
kubectl -n "$NS" delete secret "unrelated-$$" >/dev/null
kubectl -n "$NS" delete secret "ambit-key-$MID" --as="$NC" >/dev/null || fail "4c namespace deletion must be able to remove a key while the Lease is absent"
wait_for 30 'kubectl -n "$NS" get lease ambit-key-writer' || fail "4c the console did not re-create the Lease within 30 s"
pass "4c with no Lease: unrelated Secret created; a key Secret deleted as the namespace controller; the Lease came back"

# 5 · audit: one identity per record, the key id, never a JWT subject on key records
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
keyrecs() { # keyrecs EMAIL KEYID: records of calls made with that key
  [[ $(api GET "/api/audit?email=$1&from=$day&to=$next") == 200 ]] || return 1
  field 'sum(1 for c in d["calls"] if c.get("identity")=="key" and c.get("keyId")=="'"$2"'")'
}
wait_for 90 '[[ $(keyrecs admin@ambit.local "$KID_K") -ge 1 ]]' || fail "5 no key record: $(head -c 600 "$W/api")"
[[ $(field 'sum(1 for c in d["calls"] if c.get("identity")=="key" and c.get("subject"))') == 0 ]] || fail "5 a key record carries a subject"
[[ $(field 'sum(1 for c in d["calls"] if c.get("identity")=="key" and c.get("email")!="admin@ambit.local")') == 0 ]] || fail "5 a key record has another identity"
pass "5 audit records the key's requests as the owner, with the key id and no subject"

# 11 · the settings endpoint reports the chart values
[[ $(api GET /api/settings/keys) == 200 && "$(field '__import__("json").dumps(d,sort_keys=True)')" == '{"enabled": true, "inactivityDays": 30, "maxDays": 365}' ]] || fail "11 settings: $(cat "$W/api")"
pass "11 GET /api/settings/keys: $(cat "$W/api")"

# 6 · offboarding: keytest's key is refused at once (block list) and deleted (SCIM, as in step 6)
sign_in keycloak keytest keytest-test-pass
mkkey keytest-laptop llm; AK=$KEY; AKID=$KID
wait_for 60 '[[ $(code -H "Authorization: Bearer $AK") == 200 ]]' || fail "6 keytest's key is not accepted: $(cat "$W/llm")"
c=$(scim POST /Users '{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"keytest@acme.test","active":true,"emails":[{"value":"keytest@acme.test","primary":true}]}')
if [[ "$c" == 409 ]]; then  # a rerun: the SCIM user stays after the test
  [[ $(scim GET '/Users?filter=userName%20eq%20%22keytest@acme.test%22') == 200 ]] || fail "6 scim lookup"
  SCIM_ID=$(field 'd["Resources"][0]["id"]')
else
  [[ "$c" == 201 ]] || fail "6 scim create: $c $(cat "$W/api")"
  SCIM_ID=$(field 'd["id"]')
fi
DEACTIVATED=1
[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_OFF") == 200 ]] || fail "6 deactivate: $(cat "$W/api")"
wait_for 10 '[[ $(code -H "Authorization: Bearer $AK") == 403 ]]' || fail "6 an offboarded person's key still works"
[[ $(code -H "Authorization: Bearer $(token keytest)") == 403 ]] || fail "6 the offboarded person's JWT still works"
! kubectl -n "$NS" get secret "ambit-key-$AKID" >/dev/null 2>&1 || fail "6 the key Secret survived offboarding"
[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]] || fail "6 reactivate: $(cat "$W/api")"
DEACTIVATED=""
wait_for 30 '[[ $(code -H "Authorization: Bearer $(token keytest)") == 200 ]]' || fail "6 reactivation did not lift the block"
sign_in keycloak keytest keytest-test-pass; mkkey keytest-again llm   # the owner flag is cleared: new keys are allowed
pass "6 offboarding refuses the key and the JWT at once and deletes the key; reactivation allows both again"

# 7 · requireEmail: a token without an email claim is refused by default (Keycloak user "noemail")
NT=$(command curl -s -d grant_type=password -d client_id=ambit-cli-test -d username=noemail -d password=noemail-test-pass \
  "$KC/realms/acme/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])') || fail "7 no token for noemail"
[[ $(code -H "Authorization: Bearer $NT") == 403 ]] || fail "7 a JWT without email was not refused: $(cat "$W/llm")"
pass "7 a JWT without an email is refused (requireEmail default)"

# 8 · MCP: a key sees exactly the tools its owner's JWT sees (same groups)
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null && mcp_ready "$NS"
sign_in keycloak mira mira-test-pass
mkkey mira-mcp mcp; MK=$KEY
tools "$(token mira)"; JWT_TOOLS=$TOOLS
wait_for 60 'try_tools "$MK"; [[ "$TOOLS" == "$JWT_TOOLS" ]]' || fail "8 key tools '$TOOLS' differ from JWT tools '$JWT_TOOLS'"
[[ -n "$JWT_TOOLS" ]] || fail "8 mira sees no tools"
pass "8 MCP: a key gets its owner's tools ($JWT_TOOLS)"

# 10 · mixed credentials: mira's key in x-api-key plus alice's JWT in Authorization is served as mira
sign_in keycloak mira mira-test-pass
mkkey mira-llm llm; ML=$KEY; ML_ID=$KID
wait_for 60 '[[ $(code -H "x-api-key: $ML" -H "Authorization: Bearer $(token alice)") == 200 ]]' || fail "10 mixed credentials: $(cat "$W/llm")"
sign_in local admin@ambit.local "$PW"
wait_for 90 '[[ $(keyrecs mira@acme.test "$ML_ID") -ge 1 ]]' || fail "10 no audit record for mira's key: $(head -c 600 "$W/api")"
[[ $(field 'sum(1 for c in d["calls"] if c.get("keyId")=="'"$ML_ID"'" and c.get("subject"))') == 0 ]] || fail "10 the record carries alice's subject"
[[ $(field 'sum(1 for c in d["calls"] if c.get("email")=="alice@acme.test" and c.get("keyId")=="'"$ML_ID"'")') == 0 ]] || fail "10 the record names alice"
pass "10 key + another person's JWT: served as mira; the record has mira's email, the key id and no subject"

# 9 · budgets: one counter per person, whatever the credential
flush() { kubectl -n "$NS" exec deploy/ambit-valkey -- sh -c "valkey-cli --scan --pattern 'ambit-llm*' | xargs -r valkey-cli del" >/dev/null 2>&1 || true; }
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: keys-mira }
spec: { group: ml-research, userRequestsPerDay: 1 }
EOF
kubectl -n "$NS" wait "budgets.$G" keys-mira --for=condition=Ready --timeout=60s >/dev/null || fail "9 budget not Ready"
wait_rl "$NS" "group_ml-research.user: unit=DAY requests_per_unit=1,"
flush
b1=$(code -H "x-api-key: $ML"); b2=$(code -H "x-api-key: $ML"); b3=$(code -H "Authorization: Bearer $(token mira)")
[[ "$b1" == 200 && "$b2" == 429 && "$b3" == 429 ]] || fail "9 mira's key then key then JWT: $b1 $b2 $b3 (want 200 429 429): $(cat "$W/llm")"
kubectl -n "$NS" delete "budgets.$G" keys-mira >/dev/null
flush
pass "9 mira's key got one 200 then 429; her JWT then got 429 too (same counter)"

# 12 · apiKeys.enabled=false: the gateway still decides JWTs (group rule, offboarding); keys are refused
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: keys-eng-mock }
spec: { groups: [eng], models: [mock-echo] }
EOF
kubectl -n "$NS" delete "accessrules.$G" keys-everyone-mock >/dev/null
wait_for 120 '[[ $(code -H "Authorization: Bearer $(token alice)") == 200 && $(code -H "Authorization: Bearer $(token priya)") == 403 ]]' || fail "12 setup: only the eng group rule should allow mock-echo"
sign_in keycloak keytest keytest-test-pass
mkkey keytest-before llm; KT=$KEY
wait_for 60 '[[ $(code -H "x-api-key: $KT") == 200 ]]' || fail "12 setup: keytest's key (group eng) is not accepted: $(cat "$W/llm")"
DISABLED=1
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set apiKeys.enabled=false --wait --timeout 5m >/dev/null
wait_for 90 '[[ $(code -H "x-api-key: $KT") =~ ^40[13]$ ]]' || fail "12 with keys off a key is still accepted: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer $KT") =~ ^40[13]$ ]] || fail "12 with keys off a bearer key is accepted: $(cat "$W/llm")"
[[ $(code -H "Authorization: Bearer $(token keytest)") == 200 ]] || fail "12 with keys off a group-rule JWT user is refused: $(cat "$W/llm")"
DEACTIVATED=1
# the new console pod answers 503 ("retry") until it holds the key Lease (up to the 15 s lease time)
wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_OFF") == 200 ]]' || fail "12 deactivate: $(cat "$W/api")"
wait_for 30 '[[ $(code -H "Authorization: Bearer $(token keytest)") == 403 ]]' || fail "12 with keys off an offboarded JWT user is not refused: $(cat "$W/llm")"
wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]]' || fail "12 reactivate: $(cat "$W/api")"
DEACTIVATED=""
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
DISABLED=""
wait_for 60 '[[ $(code -H "Authorization: Bearer $(token keytest)") == 200 ]]' || fail "12 after restore the JWT user is refused"
sign_in keycloak keytest keytest-test-pass; mkkey keytest-after llm
wait_for 90 '[[ $(code -H "x-api-key: $KEY") == 200 ]]' || fail "12 after restore a new key is not accepted: $(cat "$W/llm")"
pass "12 apiKeys.enabled=false: group-rule JWT 200, keys refused, offboarded JWT 403; enabled again: keys work"
done_step keys
