#!/usr/bin/env bash
# Step platforms: the request shape of each hosted platform that connects with a key header (docs/guides/agents/*.md),
# sent to the public front door of the test cluster. This is a replay of the request each guide describes, not a test with the
# platform: "request shape tested; not yet tested with a live account". For each platform: the header that the guide tells you
# to enter, an agent of the mode and platform the guide names, an MCP session (initialize, initialized, tools/list, one call),
# a wrong key, a key in the address (never accepted), and the audit record of the agent (agent, mode, declared platform, ingress).
# Needs the agents of docs/agents.md and the front door of docs/front-door.md. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
source test/frontdoor/lib.sh
G=ambitproject.dev
PUB=mcp-public.localtest.me
CH_POD=ambit-clickhouse-0
ch() { kubectl -n "$NS" exec "$CH_POD" -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
field() { "${PY[@]}" -c "import json,sys;d=json.load(open(sys.argv[1]));print($1)" "$W/api"; }
ag() { local c; for _ in $(seq 1 15); do c=$(api "$@"); [[ "$c" == 503 ]] || break; sleep 2; done; echo "$c"; }
AGENTS=()
cleanup() {
  for p in ${PIDS[@]+"${PIDS[@]}"}; do kill "$p" 2>/dev/null || true; done
  kubectl -n "$NS" patch "servers.$G" time --type json -p '[{"op":"remove","path":"/spec/frontDoor"}]' >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" fp-time fp-ops --ignore-not-found >/dev/null 2>&1 || true
  local pw; pw=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true
  if sign_in local admin@ambit.local "$pw" >/dev/null 2>&1; then
    for n in ${AGENTS[@]+"${AGENTS[@]}"}; do api DELETE "/api/agents/$n" >/dev/null 2>&1 || true; done
    for id in ${KEYIDS[@]+"${KEYIDS[@]}"}; do api DELETE "/api/admin/keys/$id" >/dev/null 2>&1 || true; done
  fi
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT

# --- 0 · setup: the front door on, one server (time) on it, rules for the admin (person agents) and for group ops (itself agents)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$PUB" -addext "subjectAltName=DNS:$PUB" -keyout "$W/tls.key" -out "$W/ca.pem" >/dev/null 2>&1
kubectl -n "$NS" create secret tls ambit-public-tls --cert="$W/ca.pem" --key="$W/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$W/tls.key"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/frontdoor/values.yaml --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-public --timeout=300s >/dev/null
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: fp-time }
spec: { allSignedIn: true, servers: [{ name: time }] }
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: fp-ops }
spec: { groups: [ops], servers: [{ name: time }] }
EOF
kubectl -n "$NS" wait "servers.$G" time --for=condition=Ready --timeout=300s >/dev/null || fail "0 server time not Ready"
kubectl -n "$NS" patch "servers.$G" time --type merge -p '{"spec":{"frontDoor":{"enabled":true,"acceptUnmaskedServerMessages":true}}}' >/dev/null
wait_for 120 "kubectl -n '$NS' get agentgatewaybackend ambit-mcp-public >/dev/null" || fail "0 no public backend"
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW" || fail "0 admin sign-in"
[[ $(api POST /api/test/keys/reset-limits '{}') == 204 ]] || fail "0 reset-limits hook: $(cat "$W/api")"
PODS=($(kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit-public --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'))
PA=$(forward "${PODS[0]}"); PB=$PA
SUFFIX=$(date +%s | tail -c 7)

# agent MODE PLATFORM NAME [RANGES-JSON]: makes an agent (scope: the time server's one tool) and a key; sets AGENT, KEY, KID
agent() {
  local mode=$1 platform=$2 name=$3 ranges=${4:-[]} body c
  if [[ $mode == person ]]; then
    body="{\"displayName\":\"$name $SUFFIX\",\"platform\":\"$platform\",\"scope\":[\"time/get_current_time\"]}"
  else
    body="{\"mode\":\"itself\",\"name\":\"$name-$SUFFIX\",\"displayName\":\"$name\",\"group\":\"ops\",\"owner\":\"admin@ambit.local\",\"platform\":\"$platform\",\"scope\":[\"time/get_current_time\"]}"
  fi
  c=$(ag POST /api/agents "$body"); [[ "$c" == 201 ]] || fail "agent $name ($mode, $platform): HTTP $c $(cat "$W/api")"
  AGENT=$(field 'd["name"]'); AGENTS+=("$AGENT")
  c=$(ag POST "/api/agents/$AGENT/keys" "{\"name\":\"$platform\",\"days\":30,\"sourceRanges\":$ranges}"); [[ "$c" == 201 ]] || fail "key of $AGENT: HTTP $c $(cat "$W/api")"
  KEY=$(field 'd["key"]'); KID=$(field 'd["view"]["id"]')
}

# replay HEADER-NAME VALUE-PREFIX PROTOCOL: the platform's request shape with the agent's key in the header it is told to use.
# MCP: initialize, notifications/initialized, tools/list (one tool: the agent's scope), a call that the scope allows, a call that it
# does not, then the session is closed (DELETE). A wrong key, and the key in the address, are refused.
replay() {
  local hname=$1 prefix=$2 proto=$3 label=$4 KH=(-H "$hname: $prefix$KEY") c
  local init="{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"$proto\",\"capabilities\":{},\"clientInfo\":{\"name\":\"$label\",\"version\":\"1\"}}}"
  wait_for 90 'c=$(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$init" "${KH[@]}"); [[ "$c" == 200 ]]' || fail "$label: initialize with a new key: HTTP $c $(head -c 200 "$W/pub")"
  PSID=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/pubh" | tr -d '\r'); [[ -n "$PSID" ]] || fail "$label: no session id"
  c=$(psend "$PA" '{"jsonrpc":"2.0","method":"notifications/initialized"}' "${KH[@]}" -H "MCP-Protocol-Version: $proto"); [[ "$c" == 202 || "$c" == 200 ]] || fail "$label: initialized: $c"
  c=$(psend "$PA" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "${KH[@]}" -H "MCP-Protocol-Version: $proto"); [[ "$c" == 200 ]] || fail "$label: tools/list: $c"
  local tools; tools=$("${PY[@]}" -c 'import json,sys;print(" ".join(sorted(t["name"] for t in json.load(open(sys.argv[1]))["result"]["tools"])))' "$W/pubj")
  [[ "$tools" == time_get_current_time ]] || fail "$label: tools/list shows '$tools', want only the tool in the agent's scope"
  c=$(psend "$PA" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"time_get_current_time","arguments":{"timezone":"UTC"}}}' "${KH[@]}" -H "MCP-Protocol-Version: $proto")
  [[ "$c" == 200 ]] && grep -q '"result"' "$W/pubj" && ! grep -q '"isError":true' "$W/pubj" || fail "$label: the allowed call: $c $(head -c 300 "$W/pub")"
  c=$(psend "$PA" '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"time_convert_time","arguments":{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}}}' "${KH[@]}" -H "MCP-Protocol-Version: $proto")
  ! grep -q '"result"' "$W/pubj" || grep -q '"isError":true' "$W/pubj" || fail "$label: a tool outside the scope returned a result: $(head -c 300 "$W/pub")"
  c=$(pub "$PA" /mcp -X DELETE -H "Mcp-Session-Id: $PSID" "${KH[@]}"); [[ "$c" =~ ^(200|202|204|405)$ ]] || fail "$label: closing the session: $c"
  [[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$init" -H "$hname: ${prefix}ambit_wrongwrongwrongwrongwrongwrongwrongwrongwro") == 403 ]] || fail "$label: a wrong key was not refused"
  [[ $(pub "$PA" "/mcp?api_key=$KEY" -X POST "${MCPH[@]}" -d "$init") == 403 ]] || fail "$label: a key in the address was accepted"
  [[ $(pub "$PA" "/mcp?api_key=$KEY" -X POST "${MCPH[@]}" -d "$init" "${KH[@]}") == 200 ]] || fail "$label: an extra query parameter broke a valid request"
}
# audited AGENT MODE PLATFORM: the access log names the agent, its mode, the declared platform and the public ingress
audited() {
  wait_for 120 '[[ $(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['"'"'ambit.agent'"'"'] = '"'"'$1'"'"' AND LogAttributes['"'"'ambit.agent_mode'"'"'] = '"'"'$2'"'"' AND LogAttributes['"'"'ambit.platform_declared'"'"'] = '"'"'$3'"'"' AND LogAttributes['"'"'ambit.ingress'"'"'] = '"'"'public'"'"'") -ge 3 ]]' ||
    fail "audit has no public records of agent $1 ($2, $3)"
}

# --- 1 · Grok Bot: a person's own agent, Authorization: Bearer
agent person grok "Grok Bot"; replay Authorization "Bearer " 2025-03-26 "grok"; audited "$AGENT" person grok
pass "1 Grok Bot (docs/guides/agents/grok-bot.md): person agent $AGENT, Authorization: Bearer, protocol 2025-03-26: session, scoped tools, refused call, wrong key, key in the address; audited as agent/person/grok/public"
# --- 2 · Copilot Studio: a person's own agent, the key alone in x-api-key
agent person copilot-studio "Copilot Studio"; replay x-api-key "" 2025-06-18 "copilot-studio"; audited "$AGENT" person copilot-studio
pass "2 Copilot Studio (docs/guides/agents/copilot-studio.md): person agent $AGENT, x-api-key with the key alone, protocol 2025-06-18; audited as copilot-studio"
# --- 3 · AWS AgentCore: an itself-mode agent (a service account), Authorization: Bearer
agent itself agentcore "AgentCore gateway"; replay Authorization "Bearer " 2025-06-18 "agentcore"; audited "$AGENT" itself agentcore
pass "3 AWS AgentCore (docs/guides/agents/agentcore.md): itself-mode agent $AGENT, Authorization: Bearer, protocol 2025-06-18; audited as agent/itself/agentcore"
# --- 4 · Cloudflare Agents: an itself-mode agent, Authorization: Bearer, MCP-Protocol-Version and Mcp-Session-Id on every request
agent itself cloudflare "Cloudflare worker"; replay Authorization "Bearer " 2025-11-25 "cloudflare"; audited "$AGENT" itself cloudflare
pass "4 Cloudflare Agents (docs/guides/agents/cloudflare.md): itself-mode agent $AGENT, Authorization: Bearer, protocol 2025-11-25 headers on every request, session closed with DELETE; audited as cloudflare"
# --- 5 · Claude.ai shared team setup (lower assurance): one itself-mode agent; source ranges decide who may use the key
agent itself claude-ai "Claude.ai team" '["192.0.2.0/24"]'
wait_for 60 '[[ $(pub "$PA" /mcp -X POST "${MCPH[@]}" -d "$INIT" -H "Authorization: Bearer $KEY") == 403 ]]' || fail "5 a key with other source ranges worked from this address: $(head -c 200 "$W/pub")"
KEY_OTHER=$KEY
agent itself claude-ai "Claude.ai team any" '["0.0.0.0/0","::/0"]'; replay Authorization "Bearer " 2025-11-25 "claude-ai"; audited "$AGENT" itself claude-ai
who=$(ch "SELECT any(LogAttributes['jwt.email']) FROM otel.otel_logs WHERE LogAttributes['ambit.agent'] = '$AGENT'")
[[ "$who" == *"@svc.ambit.internal" ]] || fail "5 the shared agent is not a service account in Audit: '$who'"
pass "5 Claude.ai shared team setup (docs/guides/agents/claude-ai-shared-team.md): itself-mode agent; a key with other source ranges is refused (403) from this address, one that allows it works; Audit shows the service account $who, never a person"

done_step platforms
