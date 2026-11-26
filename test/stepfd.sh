#!/usr/bin/env bash
# Step fd: the public front door (frontDoor.*, docs/front-door.md), key-only. A second Gateway, ambit-public, with HTTPS
# only, one host and one route. This step checks that nothing private is reachable on it, that only a valid MCP key passes,
# that the limits count what they should, and that the Gateway keeps working when a pod restarts or the limiter is down.
# Nothing is exposed: the test reaches the pods through kubectl port-forward on random local ports.
# Server `time` is on the front door (acknowledged); server `everything` is private. Both are allowed to the key's owner by an
# AccessRule, so only the front door's own backend keeps the second one out. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
source test/frontdoor/lib.sh
G=ambitproject.dev
PUB=mcp-public.localtest.me
CH_POD=ambit-clickhouse-0
ch() { kubectl -n "$NS" exec "$CH_POD" -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
# values used by every helm upgrade of this step; the limits are high so the other checks never reach them (check 7 lowers them)
fdset() { helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/frontdoor/values.yaml \
  --set frontDoor.rateLimit.perCredentialPerMinute=600 --set frontDoor.rateLimit.perSourcePerMinute=100000 --set frontDoor.rateLimit.perPodPerSecond=200 \
  "$@" --wait --timeout 5m >/dev/null; }
cleanup() {
  stop_forwards
  kubectl -n "$NS" delete configmap ambit-offboarded --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" patch "servers.$G" time --type json -p '[{"op":"remove","path":"/spec/frontDoor"}]' >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "servers.$G" fd-unack fd-conn --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" fd-time --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete httproute fd-evil fd-other --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=1 >/dev/null 2>&1 || true
  kubectl -n "$NS" scale deploy/ambit-valkey --replicas=1 >/dev/null 2>&1 || true
  for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/keys/$id" >/dev/null 2>&1 || true; done
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

# --- 0 · setup: certificate, install, test servers (one on the front door), keys
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$PUB" -addext "subjectAltName=DNS:$PUB" -keyout "$W/tls.key" -out "$W/ca.pem" >/dev/null 2>&1
kubectl -n "$NS" create secret tls ambit-public-tls --cert="$W/ca.pem" --key="$W/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$W/tls.key"
# Dex issues the gateway tokens (mcp.jwt.dex in test/frontdoor/values.yaml): no Keycloak is needed for this step.
fdset
kubectl -n "$NS" rollout status deploy/ambit-public --timeout=300s >/dev/null
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: fd-time }
spec: { allSignedIn: true, servers: [{ name: time }, { name: everything }] }
EOF
kubectl -n "$NS" rollout status deploy/ambit-guard --timeout=300s >/dev/null || fail "0 ambit-guard is not running"
kubectl -n "$NS" wait "servers.$G" time everything --for=condition=Ready --timeout=300s >/dev/null || fail "0 servers not Ready"
# the guard hides and refuses a shared server's tools until they are read and pinned
kubectl -n "$NS" wait "servers.$G" time everything --for=condition=ToolsPinned --timeout=300s >/dev/null || fail "0 tools not pinned: $(kubectl -n "$NS" get "servers.$G" time everything -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[*].reason} {end}')"
# time is on the front door, with the acknowledgment; everything is not
kubectl -n "$NS" patch "servers.$G" time --type merge -p '{"spec":{"frontDoor":{"enabled":true,"acceptUnmaskedServerMessages":true}}}' >/dev/null
wait_for 120 "kubectl -n '$NS' get agentgatewaybackend ambit-mcp-public >/dev/null && kubectl -n '$NS' get agentgatewaypolicy ambit-mcp-public-access >/dev/null" || fail "0 no public backend for the front-door server"
kubectl -n "$NS" wait agentgatewaybackend/ambit-mcp-public --for=condition=Accepted --timeout=120s >/dev/null || fail "0 public backend not accepted"
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW"
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "0 reset-limits hook: $(cat "$W/api")"
mkkey fd-mcp mcp; K=$KEY; KID_K=$KID
mkkey fd-llm llm; LK=$KEY
mkkey fd-mcp-2 mcp; K2=$KEY; KID_K2=$KID
public_pods
[[ ${#PODS[@]} -eq 2 ]] || fail "0 want 2 public gateway pods, have: ${PODS[*]}"
PA=$(forward "${PODS[0]}"); PB=$(forward "${PODS[1]}")
KH=(-H "x-api-key: $K")
# the private route lists the time server's tool for this key before the public route is asked (the policy copy is then settled)
privtools() { # the tool names the key sees on the private route
  local sid; command curl -s -D "$W/ph" -o /dev/null -X POST "$MCP" "${MCPH[@]}" -H "x-api-key: $K" -d "$INIT"
  sid=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/ph" | tr -d '\r')
  command curl -s -o /dev/null -X POST "$MCP" "${MCPH[@]}" -H "x-api-key: $K" -H "Mcp-Session-Id: $sid" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  command curl -s -X POST "$MCP" "${MCPH[@]}" -H "x-api-key: $K" -H "Mcp-Session-Id: $sid" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | awk '/^data: /{print substr($0, 7); next} /^\{/{print}' |
    "${PY[@]}" -c 'import json,sys;print(" ".join(sorted(t["name"] for t in json.loads(sys.stdin.read())["result"]["tools"])))'
}
wait_for 120 '[[ "$(privtools)" == *time_get_current_time* ]]' || fail "0 the private route does not list the time server's tool for the key: '$(privtools 2>&1)'"
# a new key is accepted within seconds; the public Gateway needs the same time
wait_for 120 '[[ $(ptools $PA "${KH[@]}") == *time_get_current_time* ]]' || { pinit "$PA" "${KH[@]}"; fail "0 a valid key does not reach the public backend: status '$PCODE', port '$PA', curl: $(tail -c 300 "$W/pub.err"); a tool list: $(ptools "$PA" "${KH[@]}" 2>&1 | head -c 300) $(head -c 300 "$W/pubj")"; }
pass "0 installed; two public gateway pods (${PODS[*]}); a new key lists the front-door server's tools through the public Gateway"

# --- 1 · what is not served: every private path and host is 404 (or no answer) on the public port
not_served() { # not_served PORT PATH [curl args]
  local c; c=$(pub "$1" "$2" "${@:3}")
  [[ "$c" == 404 ]] || fail "1 $2 ${*:3}: HTTP $c, want 404: $(head -c 200 "$W/pub")"
}
for p in / /api/me /api/keys /api/agents /dex/.well-known/openid-configuration /dex/auth /auth/login /scim/v2/Users /connections /.well-known/oauth-protected-resource/mcp /mcp/ /mcp/x /v1/models /v1/chat/completions; do
  not_served "$PA" "$p"
  not_served "$PA" "$p" "${KH[@]}"          # a valid key does not open a private path
done
not_served "$PA" /mcp/anything -X POST "${KH[@]}"
# Another host name on the public port: the listener is bound to one host. The answer is 404, or the TLS handshake is
# refused; never a page of the console, Dex or the LLM host.
HOSTSEEN=""
for h in console.localtest.me llm.localtest.me mcp.localtest.me other.example.com; do
  c=$(command curl -sk -o "$W/pub" -w '%{http_code}' --max-time 15 --resolve "$h:$PA:127.0.0.1" "https://$h:$PA/" || true)
  [[ "$c" == 404 || "$c" == 000 ]] || fail "1 host $h on the public port: HTTP $c: $(head -c 200 "$W/pub")"
  c=$(command curl -sk -o "$W/pub" -w '%{http_code}' --max-time 15 --resolve "$h:$PA:127.0.0.1" -H "x-api-key: $K" "https://$h:$PA/mcp" -X POST -d "$INIT" -H 'Content-Type: application/json' || true)
  [[ "$c" == 404 || "$c" == 000 ]] || fail "1 /mcp with a key under host $h on the public port: HTTP $c: $(head -c 200 "$W/pub")"
  HOSTSEEN="$HOSTSEEN $h=$c"
done
# The Host header alone (valid TLS name, other Host header): the route's host name does not match
c=$(pub "$PA" /mcp -X POST -H 'Host: console.localtest.me' "${MCPH[@]}" -d "$INIT" "${KH[@]}")
[[ "$c" == 404 ]] || fail "1 /mcp with Host console.localtest.me under the public name: HTTP $c: $(head -c 200 "$W/pub")"
# Plain HTTP on the HTTPS port gets no MCP answer
c=$(command curl -s -o "$W/pub" -w '%{http_code}' --max-time 10 "http://127.0.0.1:$PA/mcp" -X POST -d "$INIT" "${KH[@]}" || true)
[[ "$c" != 200 ]] || fail "1 plain HTTP reached MCP: $(head -c 200 "$W/pub")"
pass "1 on the public port: 14 private or unknown paths are 404 with and without a key; other host names:${HOSTSEEN:-none}; plain HTTP: HTTP $c"

# --- 2 · who passes: only a valid MCP key
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT") == 403 ]] || fail "2 no key: $(head -c 200 "$W/pub")"
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H 'x-api-key: ambit_wrongwrongwrongwrongwrongwrongwrongwrongwro') == 403 ]] || fail "2 wrong key: $(head -c 200 "$W/pub")"
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H 'Authorization: Bearer ambit_wrongwrongwrongwrongwrongwrongwrongwrongwro') == 403 ]] || fail "2 wrong bearer key: $(head -c 200 "$W/pub")"
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $LK") == 403 ]] || fail "2 an LLM-scoped key: $(head -c 200 "$W/pub")"
[[ $(ptools "$PA" -H "Authorization: Bearer $K") == *time_get_current_time* ]] || fail "2 a valid key in the Authorization header"
[[ $(ptools "$PA" -H "x-api-key: $K") == *time_get_current_time* ]] || fail "2 a valid key in x-api-key"
TOK=$(command curl -s "$BASE/dex/token" -d grant_type=password -d client_id=ambit-cli -d scope="openid email profile" \
  --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" | "${PY[@]}" -c 'import json,sys;print(json.load(sys.stdin)["access_token"])') || fail "2 no token from Dex"
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "Authorization: Bearer $TOK") == 403 ]] || fail "2 a company token is accepted on the public Gateway: $(head -c 200 "$W/pub")"
# the same token and the same key on the private route: no regression
[[ $(command curl -s -o /dev/null -w '%{http_code}' -X POST "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Authorization: Bearer $TOK" -d "$INIT") == 200 ]] || fail "2 the company token no longer works on the private route"
[[ $(command curl -s -o /dev/null -w '%{http_code}' -X POST "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "x-api-key: $K" -d "$INIT") == 200 ]] || fail "2 the key no longer works on the private route"
pass "2 no key, wrong keys, an LLM-scoped key and a company token: 403; a valid MCP key in either header: tools listed; the private route still takes both"

# --- 3 · the key expires and the owner is offboarded: refused on the public Gateway like on the private one
[[ $(api POST "/api/test/keys/$KID_K/expire" "{\"until\":\"$(past)\"}") == 204 ]] || fail "3 expire hook: $(cat "$W/api")"
wait_for 20 '[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" "${KH[@]}") == 403 ]]' || fail "3 an expired key still works on the public Gateway"
pass "3 an expired key is refused (403) on the public Gateway"
kubectl -n "$NS" create configmap ambit-offboarded --from-literal=emails='["admin@ambit.local"]' >/dev/null
wait_for 60 '[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K2") == 403 ]]' || fail "3 an offboarded owner's key still works on the public Gateway"
[[ $(command curl -s -o /dev/null -w '%{http_code}' -X POST "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "x-api-key: $K2" -d "$INIT") == 403 ]] || fail "3 the private route does not refuse the offboarded owner"
kubectl -n "$NS" delete configmap ambit-offboarded >/dev/null
wait_for 60 '[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K2") == 200 ]]' || fail "3 the key does not work again after reactivation"
pass "3 an offboarded owner's key is refused on both Gateways, and works again when the person is reactivated"

# --- 4 · only the named route may be attached to the public Gateway (admission, server-side dry run)
route() { # route NAME GATEWAY HOST: a route to dry-run
  cat <<EOF2
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: $1 }
spec:
  parentRefs: [{ name: $2 }]
  hostnames: [$3]
  rules: [{ matches: [{ path: { type: PathPrefix, value: / } }], backendRefs: [{ name: ambit-console, port: 8080 }] }]
EOF2
}
out=$(route fd-evil ambit-public "$PUB" | kubectl -n "$NS" apply --dry-run=server -f - 2>&1) && fail "4 a route of another name was admitted on the public Gateway"
grep -q 'only the route ambit-mcp-public' <<<"$out" || fail "4 wrong refusal: $out"
out=$(route ambit-mcp-public ambit-public other.example.com | kubectl -n "$NS" apply --dry-run=server -f - 2>&1) && fail "4 the public route for another host was admitted"
grep -q 'only the route ambit-mcp-public' <<<"$out" || fail "4 wrong refusal (host): $out"
route fd-other ambit console.localtest.me | kubectl -n "$NS" apply --dry-run=server -f - >/dev/null 2>&1 || fail "4 a route of the private Gateway was refused"
# a change of the real route: another path, another backend, another Gateway next to the public one
edit() { # edit PYTHON-STATEMENT: the real route with one change, dry-run
  kubectl -n "$NS" get httproute ambit-mcp-public -o json | "${PY[@]}" -c "
import json, sys
o = json.load(sys.stdin)
for k in ('status',): o.pop(k, None)
for k in ('resourceVersion', 'uid', 'creationTimestamp', 'generation', 'managedFields'): o['metadata'].pop(k, None)
$1
print(json.dumps(o))" | kubectl -n "$NS" apply --dry-run=server -f - 2>&1
}
for change in "o['spec']['rules'][0]['matches'][0]['path']['value']='/mcp/x'" \
  "o['spec']['rules'][0]['backendRefs']=[{'name':'ambit-console','port':8080}]" \
  "o['spec']['rules'][0]['matches'][0]['path']['type']='PathPrefix'" \
  "o['spec']['parentRefs'].append({'name':'ambit'})"; do
  out=$(edit "$change") && fail "4 a change of the public route was admitted: $change"
  grep -q 'only the route ambit-mcp-public' <<<"$out" || fail "4 wrong refusal for $change: $out"
done
edit "pass" >/dev/null || fail "4 the unchanged public route was refused"
pass "4 admission: a route of another name, or for another host, is refused on ambit-public, and so is a change of the public route (path, backend, extra Gateway); routes of the private Gateway are not affected"

# --- 5 · the audit record says public, with the caller; a private call says private
cnt() { ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['ambit.ingress'] = '$1' AND LogAttributes['ambit.key_id'] = '$KID_K2' AND Timestamp >= now() - INTERVAL 10 MINUTE"; }
wait_for 120 '[[ $(cnt public) -ge 1 && $(cnt private) -ge 1 ]]' || fail "5 audit has no public and private record of key $KID_K2 (public $(cnt public), private $(cnt private))"
who=$(ch "SELECT any(LogAttributes['jwt.email']) FROM otel.otel_logs WHERE LogAttributes['ambit.ingress'] = 'public' AND LogAttributes['ambit.key_id'] = '$KID_K2'")
[[ "$who" == admin@ambit.local ]] || fail "5 the public record names '$who'"
bad=$(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['ambit.ingress'] = 'public' AND LogAttributes['ambit.accessMode'] != 'enforce'")
[[ "$bad" == 0 ]] || fail "5 $bad public records are not in enforce mode"
pass "5 audit: the key's public calls say ambit.ingress=public and name the owner; its private calls say private ($(cnt public) public, $(cnt private) private)"

# --- 6 · an MCP session survives a restart of the public pods, and any pod answers it (shared session key)
kubectl -n "$NS" get secret -o name | grep -i 'ambit-public' >/dev/null && SESSKEY=$(kubectl -n "$NS" get secret -o name | grep -i 'ambit-public' | tr '\n' ' ') || SESSKEY="(none named ambit-public)"
pinit "$PA" -H "x-api-key: $K2"; S1=$PSID
[[ "$PCODE" == 200 && -n "$S1" ]] || fail "6 no session on pod A: $PCODE"
psend "$PA" '{"jsonrpc":"2.0","method":"notifications/initialized"}' -H "x-api-key: $K2" >/dev/null
PSID=$S1; c=$(psend "$PB" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' -H "x-api-key: $K2")
[[ "$c" == 200 ]] && grep -q time_get_current_time "$W/pubj" || fail "6 pod B does not know the session of pod A: $c $(head -c 200 "$W/pub")"
kubectl -n "$NS" rollout restart deploy/ambit-public >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-public --timeout=300s >/dev/null
reforward
PSID=$S1; wait_for 60 'c=$(psend "$PA" "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/list\"}" -H "x-api-key: $K2"); [[ "$c" == 200 ]]' || fail "6 the session did not survive the restart of the public pods: $c $(head -c 200 "$W/pub")"
pass "6 an MCP session opened on one public pod is answered by the other and by the new pods after a rollout restart (session secrets: $SESSKEY)"

# --- 7 · limits: per key, per source, per pod, and the limiter down
next_window() { local s; s=$(date +%S); s=$((10#$s)); (( s < 35 )) || sleep $((62 - s)); } # leave 25 s or more in this minute
burst() { # burst N PORT [curl args]: N MCP initialize requests in a row; prints "<ok> <limited> <refused> <other>"
  local n=$1 port=$2; shift 2; local ok=0 lim=0 den=0 other=0 r last=-
  for _ in $(seq 1 "$n"); do
    r=$(pubk "$port" /mcp -X POST "${MCPH[@]}" -d "$INIT" "$@")
    case $r in ok) ok=$((ok+1));; limited) lim=$((lim+1));; refused) den=$((den+1));; *) other=$((other+1)); last=$r;; esac
  done
  echo "$ok $lim $den $other $last"
}
new_window() { local s; s=$(date +%S); s=$((10#$s)); sleep $((62 - s)); } # the start of the next minute (the limits count in fixed minutes)
mkkey fd-mcp-burst mcp; K4=$KEY
mkkey fd-mcp-3 mcp; K3=$KEY
for k in "$K4" "$K3"; do wait_for 60 '[[ $(ptools "$PA" -H "x-api-key: $k") == *time_get_current_time* ]]' || fail "7 a key of this check does not work"; done
fdset --set frontDoor.rateLimit.perCredentialPerMinute=20 >/dev/null
wait_for 150 'rlconfig "$NS" | grep -q "fd_credential.*requests_per_unit=20"' || fail "7 the ratelimit service did not load the key limit 20: $(rlconfig "$NS" | grep ambit-public)"
next_window
read -r ok lim den other LASTOTHER <<<"$(burst 30 "$PA" -H "x-api-key: $K4")"
(( lim >= 1 && ok >= 1 && ok <= 20 && other == 0 && den == 0 )) || fail "7 per key (limit 20 a minute): $ok ok, $lim limited, $den refused, $other other (${LASTOTHER:-}; curl: $(tail -c 200 "$W/pub.err" 2>/dev/null))"
KEY1_OK=$ok; KEY1_LIM=$lim
[[ $(pubk "$PB" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3") == ok ]] || fail "7 a second key was limited by the first key's counter"
[[ $(pubk "$PB" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K4") == limited ]] || fail "7 the limited key passed on the other pod (the counter is shared)"
pass "7 per key: of 30 requests with one key, $KEY1_OK passed and $KEY1_LIM were limited in one minute (limit 20); a second key passed; the first key was limited on the other pod too (a shared counter)"
fdset >/dev/null
wait_for 150 'rlconfig "$NS" | grep -q "fd_credential.*requests_per_unit=600"' || fail "7 the key limit was not restored"

# per source: counts EVERY request of an address before authentication and before the route is matched (#390): no key, a wrong
# key, an unknown path and a valid key all use the same counter. Over the limit the answer is 429.
fdset --set frontDoor.rateLimit.perSourcePerMinute=30 >/dev/null
wait_for 150 'rlconfig "$NS" | grep -q "fd_source.*requests_per_unit=30"' || fail "7 the ratelimit service did not load the source limit 30: $(rlconfig "$NS" | grep ambit-public)"
reforward  # a port-forward that sat idle may be gone
new_window  # the source counter of this minute holds every request of the checks before: start in a new minute
read -r nok nlim nden nother LASTOTHER <<<"$(burst 40 "$PA")"       # no key: 403 until the source is over its limit, then 429
(( nden >= 1 && nden <= 30 && nlim >= 10 && nok == 0 && nother == 0 )) || fail "7 requests without a key (source limit 30): $nok ok, $nlim limited, $nden refused, $nother other (${LASTOTHER:-}; want some 403, then 429, from the 31st request on)"
SRC_DEN=$nden; SRC_LIM=$nlim
[[ $(pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3") == limited ]] || fail "7 a valid key from the source that is over its limit was not limited"
[[ $(pubk "$PB" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K4") == limited ]] || fail "7 another key from the limited source was not limited on the other pod (the source counter is shared by all keys and pods)"
pass "7 per source: of 40 requests without a key, $SRC_DEN were refused (403) and $SRC_LIM were limited (429, limit 30 a minute); then a valid key from the same source, on either pod, was limited too"
new_window
n404=0; n429=0; nx=0
for _ in $(seq 1 40); do c=$(pub "$PA" /nothing); case $c in 404) n404=$((n404+1));; 429) n429=$((n429+1));; *) nx=$((nx+1));; esac; done
(( n404 >= 1 && n404 <= 30 && n429 >= 1 && nx == 0 )) || fail "7 unknown path (source limit 30): $n404 not found, $n429 limited, $nx other (want 404 until the limit, then 429)"
pass "7 per source: of 40 requests for an unknown path, $n404 got 404 and $n429 were limited (429): requests the route does not match are counted too"
fdset >/dev/null
wait_for 150 'rlconfig "$NS" | grep -q "fd_source.*requests_per_unit=100000"' || fail "7 the source limit was not restored"

# per pod, and the ratelimit service down: FailOpen keeps serving and the per-pod limit still applies; FailClosed refuses
fdset --set frontDoor.rateLimit.perPodPerSecond=3 >/dev/null
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=ambit-ratelimit --timeout=60s >/dev/null
sleep 3
[[ $(pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3") == ok ]] || fail "7 FailOpen: with the ratelimit service down a single request did not pass"
rm -f "$W"/burst.*
for i in $(seq 1 40); do ( pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3" > "$W/burst.$i" ) & done; wait
pp=$(cat "$W"/burst.* | sort | uniq -c | tr '\n' ' ')
grep -q limited "$W"/burst.* || fail "7 the per-pod limit (3 a second) did not apply with the ratelimit service down: $pp"
pass "7 per pod: with the ratelimit service down (FailOpen) a single request passes and 40 at once are limited by the pod: $pp"
# helm upgrade brings the ratelimit Deployment back to 1 replica: change the setting first, then stop the service again
fdset --set frontDoor.rateLimit.perPodPerSecond=200 --set frontDoor.rateLimit.failureMode=FailClosed >/dev/null
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=ambit-ratelimit --timeout=60s >/dev/null
wait_for 90 '[[ $(pub "$PA" /nothing) == 403 ]]' ||
  fail "7 FailClosed: with the ratelimit service down a request still passed (policy failureMode: $(kubectl -n "$NS" get agentgatewaypolicy ambit-public-limits -o jsonpath='{.spec.traffic.rateLimit.global.failureMode}'))"
FC=$(pub "$PA" /nothing)   # without the source check this path is 404: a 403 comes from the check that cannot be reached
[[ "$FC" == 403 ]] || fail "7 FailClosed: with the ratelimit service down a request for an unknown path got '$FC', want 403 from the source check"
kubectl -n "$NS" scale deploy/ambit-ratelimit --replicas=1 >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-ratelimit --timeout=120s >/dev/null
fdset >/dev/null
wait_for 90 '[[ $(pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3") == ok ]]' || fail "7 the front door does not recover when the ratelimit service is back"
pass "7 FailClosed with the ratelimit service down: an unknown path got HTTP $FC (it is 404 when the check works; here the source check cannot be reached, so the request is refused before the route is matched); back to normal when the service returns"

# Valkey down while the ratelimit service runs: the service reports its error as HTTP 400 on /json, so the source check
# refuses every request, whatever frontDoor.rateLimit.failureMode says (it applies when the service cannot be reached).
kubectl -n "$NS" scale deploy/ambit-valkey --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=ambit-valkey --timeout=60s >/dev/null
wait_for 60 '[[ $(pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3") == other:400 ]]' || fail "7 Valkey down (FailOpen): want HTTP 400 from the source check, got $(pubk "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K3")"
kubectl -n "$NS" scale deploy/ambit-valkey --replicas=1 >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-valkey --timeout=120s >/dev/null
# The ratelimit service reconnects to Valkey one connection at a time: right after one good answer another request can still
# get 400 (seen in the merge queue, 2026-11-26). Ask for 15 good answers in a row, on both pods.
valkey_back() { local p i; for p in "$PA" "$PB"; do for i in $(seq 1 15); do [[ $(pub "$p" /nothing) == 404 ]] || return 1; done; done; }
wait_for 120 'valkey_back' || fail "7 the front door does not recover when Valkey is back (an unknown path: $(pub "$PA" /nothing))"
pass "7 Valkey down with the ratelimit service up: every request is refused with HTTP 400 (documented); back to normal when Valkey returns"

# --- 8 · the client address: direct (this test: the connection) and the PROXY protocol
REQ=(uv run --no-project python -I test/frontdoor/request.py)
c=$("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /nothing); [[ "$c" == 404 ]] || fail "8 direct: no PROXY header, GET /nothing: $c"
fdset --set frontDoor.clientAddress=proxyProtocol --set frontDoor.rateLimit.perSourcePerMinute=30 >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-public --timeout=300s >/dev/null
reforward
wait_for 60 '[[ $("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /nothing --proxy 203.0.113.7) == 404 ]]' || fail "8 a request with a PROXY header is not answered: $("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /nothing --proxy 203.0.113.7)"
wait_for 90 'rlconfig "$NS" | grep -q "fd_source.*requests_per_unit=30"' || fail "8 source limit 30 not loaded"
next_window
a=0
# 203.0.113.7 floods the door with a WRONG key (403, never authenticated): the source limit must still count it (#390)
for _ in $(seq 1 40); do c=$("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /mcp --proxy 203.0.113.7 --header "x-api-key: ambit_wrongwrongwrongwrongwrongwrongwrongwrongwro" --post "$INIT"); [[ "$c" == 429 ]] && a=$((a+1)); done
c8=$("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /mcp --proxy 203.0.113.8 --header "x-api-key: $K3" --post "$INIT")
(( a >= 1 )) || fail "8 203.0.113.7 was never limited with a wrong key ($a of 40; the last answer of 203.0.113.8: $c8)"
[[ "$c8" == 200 ]] || fail "8 203.0.113.8 got '$c8' while 203.0.113.7 was limited: the limit does not use the address in the PROXY header"
wait_for 120 '[[ $(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['"'"'ambit.ingress'"'"'] = '"'"'public'"'"' AND Timestamp >= now() - INTERVAL 5 MINUTE AND LogAttributes['"'"'client.address'"'"'] = '"'"'203.0.113.7'"'"'") -ge 1 ]]' || fail "8 the audit record does not carry the proxied client address"
# last: a connection that the gateway refuses can end kubectl's port-forward
[[ $("${REQ[@]}" "$PA" "$W/ca.pem" "$PUB" /nothing) == refused ]] || fail "8 a connection without a PROXY header was served (a caller could choose its own address)"
pass "8 PROXY protocol: no header: refused; 203.0.113.7 was limited ($a of 40 requests with a wrong key, limit 30) while a valid key from 203.0.113.8 was not (HTTP $c8); the audit record (client.address) has 203.0.113.7"
fdset >/dev/null

# --- 9 · public/private separation: every MCP method on the public route (a front-door server `time`, a private server
# `everything`; the owner may use both)
priv() { # priv JSON [curl args]: one request in the private session $PRSID; the answer's JSON is in $W/privj
  command curl -s -o "$W/priv" -w '%{http_code}' -X POST "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "x-api-key: $K5" ${PRSID:+-H "Mcp-Session-Id: $PRSID"} -d "$1" "${@:2}"
  awk '/^data: /{print substr($0, 7); next} /^\{/{print}' "$W/priv" > "$W/privj"
}
mkkey fd-mcp-4 mcp; K5=$KEY
wait_for 60 '[[ $(ptools "$PA" -H "x-api-key: $K5") == *time_get_current_time* ]]' || fail "9 the key of this step does not work"
HK=(-H "x-api-key: $K5")
pinit "$PA" "${HK[@]}"; [[ "$PCODE" == 200 && -n "$PSID" ]] || fail "9 initialize: $PCODE"
pm() { psend "$PA" "$1" "${HK[@]}" "${@:2}"; }   # a request in the public session; prints the status; JSON in $W/pubj
c=$(pm '{"jsonrpc":"2.0","method":"notifications/initialized"}'); [[ "$c" == 202 || "$c" == 200 ]] || fail "9 notifications/initialized: $c"
c=$(pm '{"jsonrpc":"2.0","id":2,"method":"ping"}'); [[ "$c" == 200 ]] || fail "9 ping: $c"
# a private session of the same key, for comparison
command curl -s -D "$W/privh" -o /dev/null -X POST "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "x-api-key: $K5" -d "$INIT"
PRSID=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/privh" | tr -d '\r'); [[ -n "$PRSID" ]] || fail "9 no private session"
priv '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
names() { "${PY[@]}" -c 'import json,sys;print(" ".join(sorted(t["name"] for t in json.load(open(sys.argv[1]))["result"]["tools"])))' "$1"; }
c=$(pm '{"jsonrpc":"2.0","id":3,"method":"tools/list"}'); PUBTOOLS=$(names "$W/pubj")
priv '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' >/dev/null; PRIVTOOLS=$(names "$W/privj")
[[ "$PUBTOOLS" == *time_get_current_time* && "$PUBTOOLS" != *everything_* ]] || fail "9 tools/list on the public route: $PUBTOOLS"
[[ "$PRIVTOOLS" == *time_get_current_time* && "$PRIVTOOLS" == *everything_* ]] || fail "9 tools/list on the private route (no regression): $PRIVTOOLS"
ET=$(tr ' ' '\n' <<<"$PRIVTOOLS" | grep '^everything_echo' | head -1)   # a tool of the private server, with its prefix
[[ -n "$ET" ]] || fail "9 the private server has no echo tool: $PRIVTOOLS"
leak() { grep -qi 'everything\|resource 1\|simple_prompt\|"echo' "$W/pub"; }
refused_not_leaking() { # refused_not_leaking WHAT JSON: a request the public route must not serve from the private server
  local c; c=$(pm "$2")
  [[ "$c" != 000 ]] || fail "9 $1: no answer (a transport failure proves nothing)"
  if leak; then fail "9 $1: the private server answered through the public route: $c $(head -c 300 "$W/pub")"; fi
  echo "$1=$c"
}
R=""
R="$R $(refused_not_leaking 'tools/call everything_echo' "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/call\",\"params\":{\"name\":\"$ET\",\"arguments\":{\"message\":\"hi\"}}}")"
R="$R $(refused_not_leaking 'tools/call echo' '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hi"}}}')"
R="$R $(refused_not_leaking 'resources/list' '{"jsonrpc":"2.0","id":12,"method":"resources/list"}')"
R="$R $(refused_not_leaking 'resources/templates/list' '{"jsonrpc":"2.0","id":13,"method":"resources/templates/list"}')"
R="$R $(refused_not_leaking 'prompts/list' '{"jsonrpc":"2.0","id":14,"method":"prompts/list"}')"
R="$R $(refused_not_leaking 'resources/read' '{"jsonrpc":"2.0","id":15,"method":"resources/read","params":{"uri":"test://static/resource/1"}}')"
R="$R $(refused_not_leaking 'prompts/get' '{"jsonrpc":"2.0","id":16,"method":"prompts/get","params":{"name":"simple_prompt"}}')"
R="$R $(refused_not_leaking 'prompts/get-prefixed' '{"jsonrpc":"2.0","id":17,"method":"prompts/get","params":{"name":"everything_simple_prompt"}}')"
R="$R $(refused_not_leaking 'completion/complete' '{"jsonrpc":"2.0","id":18,"method":"completion/complete","params":{"ref":{"type":"ref/prompt","name":"everything_simple_prompt"},"argument":{"name":"x","value":""}}}')"
R="$R $(refused_not_leaking 'logging/setLevel' '{"jsonrpc":"2.0","id":19,"method":"logging/setLevel","params":{"level":"debug"}}')"
R="$R $(refused_not_leaking 'resources/subscribe' '{"jsonrpc":"2.0","id":20,"method":"resources/subscribe","params":{"uri":"test://static/resource/1"}}')"
R="$R $(refused_not_leaking 'unknown-method' '{"jsonrpc":"2.0","id":21,"method":"nope/nothing"}')"
R="$R $(refused_not_leaking 'batch' '[{"jsonrpc":"2.0","id":22,"method":"tools/list"},{"jsonrpc":"2.0","id":23,"method":"resources/list"}]')"
# the front-door server's own tool works; a call of a private tool is not a target (an error, not a result)
c=$(pm '{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"time_get_current_time","arguments":{"timezone":"UTC"}}}')
[[ "$c" == 200 ]] && grep -q '"result"' "$W/pubj" && ! grep -q '"isError":true' "$W/pubj" || fail "9 the front-door server's tool: $c $(head -c 300 "$W/pub")"
c=$(pm "{\"jsonrpc\":\"2.0\",\"id\":31,\"method\":\"tools/call\",\"params\":{\"name\":\"$ET\",\"arguments\":{\"message\":\"hi\"}}}")
! grep -q '"result"' "$W/pubj" || grep -q '"isError":true' "$W/pubj" || fail "9 a private server's tool returned a result on the public route: $(head -c 300 "$W/pub")"
# forged headers have no effect, and a private session id is unknown on the public Gateway
c=$(psend "$PA" '{"jsonrpc":"2.0","id":40,"method":"tools/list"}' "${HK[@]}" -H 'x-ambit-ingress: private' -H 'X-Ambit-User: other@example.com' -H 'x-ambit-agent: other')
[[ "$c" == 200 && "$(names "$W/pubj")" == "$PUBTOOLS" ]] || fail "9 forged headers changed the answer: $c $(head -c 200 "$W/pub")"
SAVE=$PSID; PSID=$PRSID
c=$(psend "$PA" '{"jsonrpc":"2.0","id":41,"method":"tools/list"}' "${HK[@]}")
[[ "$c" != 200 ]] || fail "9 a private session id was accepted on the public Gateway: $(head -c 200 "$W/pub")"
PSID=$SAVE
# the private route still has both servers' tools after all of this
priv '{"jsonrpc":"2.0","id":50,"method":"tools/list"}' >/dev/null; [[ "$(names "$W/privj")" == "$PRIVTOOLS" ]] || fail "9 the private route lost tools"
pass "9 public route: tools/list shows only the front-door server ($PUBTOOLS); every other method refused or without a private server's data:$R; forged headers and a private session id had no effect; the private route keeps both servers"

# --- 10 · Ready says why a server is marked but not served; only front-door pods accept the public Gateway
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Server
metadata: { name: fd-unack }
spec: { url: "https://fd-unack.invalid/mcp", transport: streamable-http, frontDoor: { enabled: true } }
---
apiVersion: $G/v1alpha1
kind: Server
metadata: { name: fd-conn }
spec: { url: "https://fd-conn.invalid/mcp", transport: streamable-http, connection: fd-no-such-connection, frontDoor: { enabled: true, acceptUnmaskedServerMessages: true } }
EOF
ready() { kubectl -n "$NS" get "servers.$G" "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].reason}'; }
wait_for 60 '[[ "$(ready fd-unack)" == "False FrontDoorNotAcknowledged" && "$(ready fd-conn)" == "False FrontDoorConnection" ]]' || fail "10 Ready: fd-unack '$(ready fd-unack)', fd-conn '$(ready fd-conn)'"
PRIV_TARGETS=$(kubectl -n "$NS" get agentgatewaybackend ambit-mcp -o jsonpath='{.spec.mcp.targets[*].name}') || fail "10 cannot read the private backend"
PUB_TARGETS=$(kubectl -n "$NS" get agentgatewaybackend ambit-mcp-public -o jsonpath='{.spec.mcp.targets[*].name}') || fail "10 cannot read the public backend"
[[ " $PRIV_TARGETS " == *" time "* && " $PUB_TARGETS " == *" time "* ]] || fail "10 the servers are missing from a backend (private: $PRIV_TARGETS, public: $PUB_TARGETS)"
for n in fd-unack fd-conn; do
  [[ " $PRIV_TARGETS " != *" $n "* ]] || fail "10 $n is a target of the private backend"
  [[ " $PUB_TARGETS " != *" $n "* ]] || fail "10 $n is a target of the public backend"
done
kubectl -n "$NS" delete "servers.$G" fd-unack fd-conn >/dev/null
CURL_IMAGE=curlimages/curl:8.20.0@sha256:b3f1fb2a51d923260350d21b8654bbc607164a987e2f7c84a0ac199a67df812a
PUBPOD=gateway.networking.k8s.io/gateway-name=ambit-public
kubectl -n "$NS" get pod -l ambitproject.dev/front-door=true -o name | grep -q . || fail "10 no pod has the label ambitproject.dev/front-door"
a=$(pod_curl "$PUBPOD" "http://mcp-time-proxy.$NS.svc:8080/mcp" -X POST -H 'Content-Type: application/json' -d "$INIT")
b=$(pod_curl "$PUBPOD" "http://mcp-everything-proxy.$NS.svc:8080/mcp" -X POST -H 'Content-Type: application/json' -d "$INIT")
d=$(pod_curl "$PUBPOD" "http://ambit-console.$NS.svc:8082/v1/connection/x")
g=$(pod_curl "gateway.networking.k8s.io/gateway-name=ambit" "http://mcp-everything-proxy.$NS.svc:8080/mcp" -X POST -H 'Content-Type: application/json' -d "$INIT")
[[ "$a" != 000 ]] || fail "10 a pod of the public Gateway cannot reach the front-door server"
[[ "$b" == 000 ]] || fail "10 a pod of the public Gateway reached the private server (HTTP $b)"
[[ "$d" == 000 ]] || fail "10 a pod of the public Gateway reached the token broker (HTTP $d)"
[[ "$g" != 000 ]] || fail "10 the private Gateway's own access to the private server was lost"
pass "10 Ready is False with the reason (FrontDoorNotAcknowledged, FrontDoorConnection) and the server is a target of neither backend; a pod labelled as the public Gateway reaches the front-door server (HTTP $a) but not the private server or the token broker (000); the private Gateway still reaches it (HTTP $g)"

# --- 11 · an agent key's source ranges apply on the public Gateway too (the Require rule of the private route is repeated there)
agent_key() { # agent_key NAME RANGES-JSON: an agent for the signed-in admin and one key; sets AKEY
  local c; c=$(api POST /api/agents "{\"displayName\":\"$1 $$\",\"platform\":\"other\",\"scope\":[\"time\"]}"); [[ "$c" == 201 ]] || fail "11 agent: HTTP $c $(cat "$W/api")"
  AGENT=$("${PY[@]}" -c 'import json,sys;print(json.load(open(sys.argv[1]))["name"])' "$W/api"); AGENTS+=("$AGENT")
  c=$(api POST "/api/agents/$AGENT/keys" "{\"name\":\"k\",\"days\":30,\"sourceRanges\":$2}"); [[ "$c" == 201 ]] || fail "11 agent key: HTTP $c $(cat "$W/api")"
  AKEY=$("${PY[@]}" -c 'import json,sys;print(json.load(open(sys.argv[1]))["key"])' "$W/api")
}
AGENTS=()
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "11 reset-limits hook"
agent_key elsewhere '["192.0.2.0/24"]'; K_ELSE=$AKEY
agent_key anywhere '["0.0.0.0/0","::/0"]'; K_ANY=$AKEY
wait_for 60 '[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K_ANY") == 200 ]]' || fail "11 an agent key that allows every address does not work: $(head -c 200 "$W/pub")"
[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "x-api-key: $K_ELSE") == 403 ]] || fail "11 an agent key for other addresses worked on the public Gateway: $(head -c 200 "$W/pub")"
[[ $(command curl -s -o /dev/null -w '%{http_code}' -X POST "$MCP" "${MCPH[@]}" -H "x-api-key: $K_ELSE" -d "$INIT") == 403 ]] || fail "11 the same key worked on the private route"
for a in "${AGENTS[@]}"; do api DELETE "/api/agents/$a" >/dev/null || true; done
pass "11 an agent key with source ranges for other addresses is refused (403) on the public and the private Gateway; one that allows every address works"

done_step fd
