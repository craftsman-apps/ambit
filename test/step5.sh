#!/usr/bin/env bash
# Step 5 acceptance: per-user credentials. Users connect a SaaS app with their own account in the console;
# the gateway sends each user's own token upstream (journey U1: "User01's calls use User01's token, User02's
# use User02's" — the test IBM's gateway fails); a broken grant shows as "reconnect" before an agent fails (U4).
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
connect_saas() { connect_app saas "$@"; }
call_whoami() { # call_whoami TOKEN: prints the whoami tool's answer, or "not listed"
  tools "$1"
  [[ " $TOOLS " == *" whoami_whoami "* ]] || { echo "not listed"; return; }
  mcp_send '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami_whoami","arguments":{}}}' |
    uv run --no-project python -c 'import json,sys;d=json.loads(sys.stdin.read());print(d["result"]["content"][0]["text"] if "result" in d else "error "+d["error"]["message"])'
}
call_hash() { # call_hash TOKEN: the tokenhash image server's answer (a hash of the bearer token it received), or "not listed"
  tools "$1"
  [[ " $TOOLS " == *" tokenhash_token_hash "* ]] || { echo "not listed"; return; }
  mcp_send '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tokenhash_token_hash","arguments":{}}}' |
    uv run --no-project python -c 'import json,sys;d=json.loads(sys.stdin.read());print(d["result"]["content"][0]["text"] if "result" in d else "error "+d["error"]["message"])'
}
kc_admin() {
  local tok; tok=$(command curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  command curl -s -X "$1" -H "Authorization: Bearer $tok" "${@:3}" "$KC/admin/realms/saas$2"
}

cleanup_preset() {
  kubectl -n "$NS" delete "accessrules.$G" whoami-p-users --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "servers.$G,connections.$G" whoami-p --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=whoami-p --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret ambit-conn-whoami-p --ignore-not-found >/dev/null 2>&1 || true
}
cleanup() {
  kubectl -n "$NS" delete "accessrules.$G" tokenhash-users --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "servers.$G" tokenhash --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete -f test/connections/objects.yaml --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$GRANTS" delete secret -l ambitproject.dev/connection=saas --ignore-not-found >/dev/null 2>&1 || true
  cleanup_preset
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

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

# 1 · U1: user09 has no grant -> the server's tools are withheld, and her connections page offers the link
sign_in keycloak user09 user09-test-pass
[[ $(state_of) == not-connected ]] || fail "1 user09 state: $(cat "$W/api")"
link=$(field '[c["connectURL"] for c in d if c["name"]=="saas"][0]')
[[ $(call_whoami "$(token user09)") == "not listed" ]] || fail "1 user09 reached whoami without a grant"
pass "1 user09 without a grant: whoami withheld; My connections offers $link"

# 2 · user01 connects with her own SaaS account; her calls carry her token
sign_in keycloak user01 user01-test-pass
connect_saas user01 user01-saas-pass
[[ $(state_of) == connected ]] || fail "2 user01 state: $(cat "$W/api")"
for _ in $(seq 1 20); do got=$(call_whoami "$(token user01)"); [[ "$got" != "not listed" ]] && break; sleep 3; done  # right after a gateway restart
[[ "$got" == "user01 @ $KC/realms/saas" ]] || fail "2 user01's call reached whoami as: $got"
pass "2 user01 connected; her call reached the server as: $got"

# 3 · user09 connects too; each call carries its own user's token, never the other's
sign_in keycloak user09 user09-test-pass
connect_saas user09 user09-saas-pass
m=$(call_whoami "$(token user09)"); a=$(call_whoami "$(token user01)")
[[ "$m" == "user09 @ $KC/realms/saas" && "$a" == "user01 @ $KC/realms/saas" ]] || fail "3 user09=$m user01=$a"
pass "3 user09 -> '$m'; user01 -> '$a' (no cross-user token)"

# 3b · An image that talks HTTP also gets each person's own token, through ToolHive's proxy. The image
#      (test/tokenhash) is loaded on the node by `make image`; a Server names an image by digest, so the
#      node's digest for it is read and given a name that the digest reference finds.
node=${KIND_NODE:-${KIND_CLUSTER:-ambit}-control-plane}
dg=$(docker exec "$node" crictl images --digests -o json | uv run --no-project python -c 'import json,sys
for i in json.load(sys.stdin)["images"]:
    if any(t.endswith("ambit-tokenhash:dev") for t in i["repoTags"]) and i["repoDigests"]:
        print(i["repoDigests"][0].split("@")[1]); break')
[[ "$dg" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "3b the node has no digest for ambit-tokenhash:dev (run make image): '$dg'"
docker exec "$node" ctr -n k8s.io images tag --force docker.io/library/ambit-tokenhash:dev "docker.io/library/ambit-tokenhash@$dg" >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Server
metadata: { name: tokenhash }
spec:
  displayName: Token hash (test image)
  image: docker.io/library/ambit-tokenhash@$dg
  transport: streamable-http
  mcpPort: 8080
  connection: saas
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: tokenhash-users }
spec:
  groups: [eng, ml-research]
  servers: [{ name: tokenhash }]
YAML
kubectl -n "$NS" wait "servers.$G" tokenhash --for=condition=Ready --timeout=240s >/dev/null || fail "3b tokenhash image server not Ready: $(kubectl -n "$NS" get "servers.$G" tokenhash -o jsonpath='{.status.conditions}')"
for _ in $(seq 1 30); do ha=$(call_hash "$(token alice)"); [[ "$ha" != "not listed" ]] && break; sleep 3; done
hm=$(call_hash "$(token mira)"); ha2=$(call_hash "$(token alice)")
[[ "$ha" == sha256:*" user=alice" && "$hm" == sha256:*" user=mira" ]] || fail "3b the image server received alice='$ha' mira='$hm'"
[[ "${ha%% *}" != "${hm%% *}" && "$ha" == "$ha2" ]] || fail "3b hashes: alice='$ha' mira='$hm' alice again='$ha2'"
hp=$(call_hash "$(token priya)")
[[ "$hp" == "not listed" ]] || fail "3b priya has no saas grant, but the image server was not withheld from her: '$hp'"
pass "3b image server over HTTP: alice -> '$ha'; mira -> '$hm'; no grant -> withheld (each person's own token reaches the container)"

# 4 · the broker is reachable only from the gateway; grants are not readable in the Secrets
probe() {
  kubectl -n "$NS" delete pod "$1" --ignore-not-found >/dev/null
  kubectl -n "$NS" run "$1" --restart=Never --image="$CURL_IMAGE" ${2:+--labels="$2"} --command -- \
    curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://ambit-console.$NS.svc:8082/v1/connection/saas" >/dev/null
  # A blocked request times out, curl exits non-zero and the pod ends Failed: wait for either phase.
  for _ in $(seq 1 120); do
    [[ $(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null) =~ ^(Succeeded|Failed)$ ]] && break; sleep 1
  done
  kubectl -n "$NS" logs "$1" 2>/dev/null | head -c 3
  kubectl -n "$NS" delete pod "$1" --wait=false >/dev/null
}
allowed=$(probe broker-probe-gw gateway.networking.k8s.io/gateway-name=ambit); blocked=$(probe broker-probe)
[[ "$allowed" == 200 && "$blocked" == 000 ]] || fail "4 broker reachability: gateway-labelled pod $allowed (want 200), other pod $blocked (want 000)"
kubectl -n "$GRANTS" get secret -l ambitproject.dev/connection=saas -o json | grep -q -e 'user01@acme' -e 'user09@acme' && fail "4 an email is readable in a grant Secret"
pass "4 broker: only gateway pods reach it (others time out); grant Secrets hold no readable email or token"

# 5 · U4: user01's SaaS session ends at the provider -> the check marks her grant broken before her agent fails
uid=$(kc_admin GET "/users?username=user01&exact=true" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
kc_admin POST "/users/$uid/logout" >/dev/null
sign_in keycloak user10 user10-test-pass
[[ $(api POST /api/connections/check '{}') == 200 ]] || fail "5 check: $(cat "$W/api")"
sign_in keycloak user01 user01-test-pass
[[ $(state_of) == broken ]] || fail "5 user01 state after logout: $(cat "$W/api")"
why=$(field '[c.get("error","") for c in d if c["name"]=="saas"][0]')
for _ in $(seq 1 15); do [[ $(call_whoami "$(token user01)") == "not listed" ]] && break; sleep 5; done  # gateway caches tokens up to 60 s
[[ $(call_whoami "$(token user01)") == "not listed" ]] || fail "5 user01 still reaches whoami with a broken grant"
pass "5 user01's grant shows 'Reconnect needed' ($why); her agent no longer reaches the server"

# 6 · disconnect revokes at the provider
sign_in keycloak user09 user09-test-pass
[[ $(api DELETE /api/connections/saas) == 200 && $(field 'd["revokedAtProvider"]') == True ]] || fail "6 disconnect: $(cat "$W/api")"
[[ $(state_of) == not-connected ]] || fail "6 user09 still connected"
pass "6 user09 disconnected; the provider confirmed the revocation (RFC 7009)"

# 7 · Presets: user10 adds "Who am I (test preset)" from the preset list, reads its tools with her own
#     SaaS account, grants one tool to eng.
helm upgrade ambit charts/ambit -n "$NS" --reuse-values --set console.testPresets=true --wait --timeout 5m >/dev/null  # the test preset is off by default
cleanup_preset  # leftovers of an interrupted run
sign_in keycloak user10 user10-test-pass
CODE=$(api POST /api/apps/from-preset "{\"operation\":\"s5p$(date +%s)\",\"preset\":\"whoami-test\",\"version\":1,\"name\":\"whoami-p\",\"displayName\":\"Who am I (preset)\",\"clientID\":\"ambit-connect\",\"clientSecret\":\"saas-test-client-secret\",\"values\":{}}")
[[ $CODE == 201 ]] || fail "7 add from preset: HTTP $CODE $(cat "$W/api")"
kubectl -n "$NS" wait "servers.$G" whoami-p --for=condition=Ready --timeout=120s >/dev/null || fail "7 whoami-p not Ready"
connect_app whoami-p user01 user01-saas-pass
CODE=$(api PUT /api/servers/whoami-p/tool-reader '{}')
[[ $CODE == 204 ]] || fail "7 tool reader: HTTP $CODE $(cat "$W/api")"
wait_for 90 '(( $(kubectl -n "$NS" get "servers.$G" whoami-p -o jsonpath="{.status.tools[*].name}" | wc -w) == 2 ))' || fail "7 tool list not read with user10's account"
[[ $(api POST /api/access-rules '{"name":"whoami-p-users","groups":["eng"],"allSignedIn":false,"servers":[{"name":"whoami-p","tools":["whoami"]}]}') == 201 ]] || fail "7 access rule: $(cat "$W/api")"
sign_in keycloak user01 user01-test-pass
connect_app whoami-p user01 user01-saas-pass
wait_for 90 'try_tools "$(token user01)"; [[ " $TOOLS " == *" whoami-p_whoami "* ]]' || true
[[ " $TOOLS " == *" whoami-p_whoami "* ]] || fail "7 granted tool not listed: $TOOLS"
[[ " $TOOLS " != *" whoami-p_whoami_admin "* ]] || fail "7 ungranted tool listed: $TOOLS"
pass "7 presets: added, tools read with user10's account, one tool granted to eng"

# 8 · Binding: changing the server URL after users connected removes the gateway's token path.
pol() { kubectl -n "$NS" get agentgatewaypolicy ambit-conn-whoami-p >/dev/null 2>&1; }
pol || fail "8 connection policy missing before the URL change"
kubectl -n "$NS" patch "servers.$G" whoami-p --type merge -p '{"spec":{"url":"http://whoami-other.ambit-system.svc:8080/mcp"}}' >/dev/null
kubectl -n "$NS" wait "servers.$G" whoami-p --for=jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'=ConnectionBound --timeout=60s >/dev/null || fail "8 no ConnectionBound"
wait_for 60 '! pol' || fail "8 connection policy kept after rebinding"
kubectl -n "$NS" patch "servers.$G" whoami-p --type merge -p '{"spec":{"url":"http://whoami.ambit-system.svc:8080/mcp"}}' >/dev/null
kubectl -n "$NS" wait "servers.$G" whoami-p --for=condition=Ready --timeout=60s >/dev/null || fail "8 not Ready after restoring the URL"
wait_for 60 pol || fail "8 connection policy not restored"
pass "8 binding: a server URL change removes the token path; changing it back restores it"

# 9 · whoami refuses a token issued for another audience.
other=$(command curl -s -d grant_type=client_credentials -d client_id=other-app -d client_secret=other-app-secret "$KC/realms/saas/protocol/openid-connect/token" |
  uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
mcp_list() { MCPARGS=(-H "Authorization: Bearer $1" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'); }
# Reachable: 401 without a token. Retried: in CI the first probe once failed within 3 s (kubectl or DNS, not a timeout).
wait_for 30 '[[ $(pod_curl "app=probe" "http://whoami.$NS.svc:8080/mcp") == 401 ]]' ||
  fail "9 whoami not reachable from a probe pod: $(pod_curl "app=probe" "http://whoami.$NS.svc:8080/mcp") ($(tr '\n' ' ' < "$W/pod_curl.err" | cut -c1-300))"
good=$(command curl -s -d grant_type=password -d client_id=ambit-connect -d client_secret=saas-test-client-secret -d username=user01 -d password=user01-saas-pass "$KC/realms/saas/protocol/openid-connect/token" |
  uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
mcp_list "$good"; code=$(pod_curl "app=probe" "http://whoami.$NS.svc:8080/mcp" "${MCPARGS[@]}")  # positive control: the right audience is accepted
[[ $code == 200 ]] || fail "9 token with the whoami audience refused: $code"
mcp_list "$other"; code=$(pod_curl "app=probe" "http://whoami.$NS.svc:8080/mcp" "${MCPARGS[@]}")
[[ $code == 401 ]] || fail "9 wrong-audience token not refused: $code"
pass "9 whoami verifies the token audience"

# 10 · NetworkPolicy: only the controller reaches the tools port; the controller cannot reach the broker.
ctl="app.kubernetes.io/name=ambit-controller"
gw="gateway.networking.k8s.io/gateway-name=ambit"
[[ $(pod_curl "$ctl" "http://ambit-console.$NS.svc:8083/v1/tools/whoami-p") == 200 ]] || fail "10 controller cannot reach the tools port (positive control)"
[[ $(pod_curl "$gw" "http://ambit-console.$NS.svc:8082/healthz") == 200 ]] || fail "10 gateway cannot reach the broker (positive control)"
[[ $(pod_curl "$gw" "http://ambit-console.$NS.svc:8083/v1/tools/whoami-p") == 000 ]] || fail "10 gateway reached the tools port"
[[ $(pod_curl "$ctl" "http://ambit-console.$NS.svc:8082/healthz") == 000 ]] || fail "10 controller reached the broker"
pass "10 network policy separates broker and tools ports"
done_step 5
