#!/usr/bin/env bash
# Contract checks: the specific things Ambit relies on in the components it ships, each failing with a
# message that names the dependency. test/upgrade.sh runs them after upgrading one component; they also
# run on their own against the pinned versions.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
attrs() { # attrs SQL-WHERE: the attribute names of the newest matching gateway record, sorted
  ch "SELECT arrayStringConcat(arraySort(mapKeys(LogAttributes)), ' ') FROM otel.otel_logs WHERE $1 ORDER BY Timestamp DESC LIMIT 1"
}

kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null || fail "C0 MCP servers not Ready"

# C1 · ToolHive: every image-based Server runs behind a proxy Service named mcp-<server>-proxy (the gateway's
#      static MCP targets and the controller's tool reader depend on that name).
for s in $(kubectl -n "$NS" get "servers.$G" -o jsonpath='{range .items[?(@.spec.image)]}{.metadata.name}{" "}{end}'); do
  kubectl -n "$NS" get service "mcp-$s-proxy" >/dev/null 2>&1 || fail "C1 ToolHive: no Service mcp-$s-proxy for Server $s"
done
pass "C1 ToolHive: each image Server has its mcp-<name>-proxy Service"

# C2 · agentgateway accepts every object the Ambit controller generates.
bad=$(kubectl -n "$NS" get agentgatewaypolicies,agentgatewaybackends,agentgatewaymodels -l app.kubernetes.io/managed-by=ambit -o json | uv run --no-project python -c '
import json, sys
bad = []
for o in json.load(sys.stdin)["items"]:
    st = o.get("status", {})
    conds = [c for a in st.get("ancestors", []) + st.get("parents", []) for c in a.get("conditions", [])] + st.get("conditions", [])
    if any(c.get("type") == "Accepted" and c.get("status") != "True" for c in conds):
        bad.append(o["kind"] + "/" + o["metadata"]["name"])
print(" ".join(bad))')
[[ -z "$bad" ]] || fail "C2 agentgateway refused generated objects: $bad"
pass "C2 agentgateway accepts every generated policy, backend and model"

# C3 · agentgateway access log → collector → ClickHouse: the attributes the Audit and Usage pages read.
A=$(token alice); M=$(token mira)
# Only records written from now on: the version under test, not records from before an upgrade.
since=$(date -u -v-3S +"%Y-%m-%d %H:%M:%S" 2>/dev/null || date -u -d '3 seconds ago' +"%Y-%m-%d %H:%M:%S")
tools "$A"; call time_get_current_time '{"timezone":"UTC"}' >/dev/null; call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}' >/dev/null
command curl -s -o /dev/null -H "Authorization: Bearer $M" -H 'Content-Type: application/json' \
  -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' http://llm.localtest.me:8080/v1/chat/completions || true
for _ in $(seq 1 30); do
  mcp=$(attrs "Timestamp >= '$since' AND LogAttributes['gen_ai.tool.name'] = 'get_current_time'" 2>/dev/null || true)
  refused=$(attrs "Timestamp >= '$since' AND LogAttributes['gen_ai.tool.name'] = 'convert_time'" 2>/dev/null || true)
  llm=$(attrs "Timestamp >= '$since' AND LogAttributes['protocol'] = 'llm'" 2>/dev/null || true)
  [[ -n "$mcp" && -n "$refused" && -n "$llm" ]] && break; sleep 2
done
[[ -n "$mcp" ]] || fail "C3 no access-log record of an MCP tool call reached ClickHouse (attribute gen_ai.tool.name missing?)"
[[ -n "$llm" ]] || fail "C3 no access-log record of an LLM request reached ClickHouse (attribute protocol=llm missing?)"
missing=()
for a in jwt.email jwt.sub jwt.groups route protocol mcp.method.name mcp.target gen_ai.tool.name; do [[ " $mcp " == *" $a "* ]] || missing+=("MCP:$a"); done
for a in jwt.email jwt.groups route protocol; do [[ " $llm " == *" $a "* ]] || missing+=("LLM:$a"); done
[[ " $refused " == *" error "* ]] || missing+=("refused MCP:error")
[[ " $mcp " == *" http.status "* || " $mcp " == *" http.response.status_code "* ]] || missing+=("MCP:http.status|http.response.status_code")
(( ${#missing[@]} == 0 )) || fail "C3 agentgateway access log lacks attributes Ambit reads: ${missing[*]}"
status_name=$([[ " $mcp " == *" http.response.status_code "* ]] && echo http.response.status_code || echo http.status)
tokens=$([[ " $llm " == *" gen_ai.usage.input_tokens "* ]] && echo "present" || echo "absent (risk 13)")
pass "C3 access log has every attribute Ambit reads; status as $status_name; LLM token counts $tokens"

# C4 · ClickHouse keeps audit records for the retention the chart sets (one year by default).
ttl=$(ch "SELECT engine_full FROM system.tables WHERE database='otel' AND name='otel_logs'")
[[ "$ttl" == *"toIntervalDay(365)"* ]] || fail "C4 ClickHouse otel_logs TTL is not 365 days: $ttl"
pass "C4 ClickHouse otel_logs keeps records 365 days"

# C5 · Dex serves the issuer the console signs in with.
iss=$(command curl -s http://console.localtest.me:8080/dex/.well-known/openid-configuration | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["issuer"])')
[[ "$iss" == "http://console.localtest.me:8080/dex" ]] || fail "C5 Dex issuer is '$iss'"
pass "C5 Dex issuer $iss"

# C6 · envoy ratelimit is up and loaded Ambit's domain config when budgets exist.
kubectl -n "$NS" rollout status deploy/ambit-ratelimit --timeout=120s >/dev/null || fail "C6 ratelimit not ready"
pass "C6 envoy ratelimit ready"
# C7 · every NetworkPolicy still selects running pods: a component that renames its pod labels (e.g. the
#      gateway's gateway.networking.k8s.io/gateway-name) would otherwise be cut off without an error.
orphans=$(kubectl -n "$NS" get networkpolicies -o json | uv run --no-project python -c '
import json, subprocess, sys
out = []
for np in json.load(sys.stdin)["items"]:
    sels = [np["spec"].get("podSelector", {}).get("matchLabels", {})]
    for rule in np["spec"].get("ingress", []):
        for f in rule.get("from", []):
            if "podSelector" in f and "namespaceSelector" not in f:
                sels.append(f["podSelector"].get("matchLabels", {}))
    for sel in sels:
        if not sel:
            continue
        q = ",".join(f"{k}={v}" for k, v in sel.items())
        pods = subprocess.run(["kubectl", "-n", sys.argv[1], "get", "pods", "-l", q, "--field-selector=status.phase=Running", "-o", "name"], capture_output=True, text=True).stdout.split()
        if not pods:
            out.append(np["metadata"]["name"] + ":" + q)
print(" ".join(sorted(set(out))))' "$NS")
[[ -z "$orphans" ]] || fail "C7 NetworkPolicy selectors match no running pod (labels changed?): $orphans"
pass "C7 every NetworkPolicy selector matches running pods"

# C8 · MCP tool names stay <server>_<tool>, and a tool a person may not use is answered "Unknown tool".
tools "$A"
[[ " $TOOLS " == *" time_get_current_time "* ]] || fail "C8 alice's tools/list lost the <server>_<tool> names: $TOOLS"
r=$(call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"UTC"}')
[[ "$r" == *"Unknown tool"* ]] || fail "C8 a denied tool is no longer answered 'Unknown tool': $r"
pass "C8 tool names <server>_<tool>; denied tools answered 'Unknown tool'"

# C9 · a model a person may not use is refused with 403 (per-model rules on AgentgatewayModel).
c=$(command curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $A" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' http://llm.localtest.me:8080/v1/chat/completions)
[[ "$c" == 403 ]] || fail "C9 alice (eng) was not refused the ml-research model: HTTP $c"
pass "C9 a disallowed model is refused with 403"

# C10 · envoy ratelimit's counter keys in Valkey keep the format the console's Budgets page reads:
#       ambit-llm_group_<group>_user_<email>_<dayStartUnix>.
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: contract-eng }
spec: { group: eng, userRequestsPerDay: 100000 }
YAML
day=$(( $(date -u +%s) / 86400 * 86400 ))
for _ in $(seq 1 60); do
  command curl -s -o /dev/null -H "Authorization: Bearer $A" -H 'Content-Type: application/json' \
    -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' http://llm.localtest.me:8080/v1/chat/completions || true
  key=$(kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli --scan --pattern "ambit-llm_group_eng_user_alice@acme.test_$day" 2>/dev/null || true)
  [[ -n "$key" ]] && break; sleep 3
done
kubectl -n "$NS" delete "budgets.$G" contract-eng --ignore-not-found >/dev/null
[[ -n "$key" ]] || fail "C10 no counter ambit-llm_group_eng_user_alice@acme.test_$day in Valkey (key format changed? keys: $(kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli --scan --pattern 'ambit-llm*' 2>/dev/null | head -3 | tr '\n' ' '))"
pass "C10 ratelimit counter key format unchanged ($key)"

# C11 · ClickHouse loaded Ambit's config (an unknown setting would stop it starting) and otel_logs has the
#       columns Ambit reads.
mark=$(ch "SELECT value FROM system.server_settings WHERE name = 'mark_cache_size'")
[[ "$mark" == 268435456 ]] || fail "C11 ClickHouse did not apply Ambit's low-memory config (mark_cache_size=$mark)"
cols=$(ch "SELECT arrayStringConcat(groupArray(name), ' ') FROM system.columns WHERE database='otel' AND table='otel_logs' AND name IN ('Timestamp','LogAttributes')")
[[ "$cols" == *Timestamp* && "$cols" == *LogAttributes* ]] || fail "C11 otel_logs lacks Timestamp or LogAttributes: $cols"
pass "C11 ClickHouse applied Ambit's config; otel_logs has Timestamp and LogAttributes"

# C12 · the identity provider's groups reach the console through Dex (roles and "What you can use").
sign_in keycloak priya priya-test-pass
[[ $(api GET /api/me) == 200 ]] || fail "C12 /api/me failed after SSO sign-in"
groups=$(uv run --no-project python -c "import json;print(json.load(open('$W/api')).get('groups'))")
[[ "$groups" == *ambit-admins* ]] || fail "C12 the groups claim did not reach the console: $groups"
pass "C12 groups claim reaches the console through Dex ($groups)"

# C13 · with Argo CD installed: Ambit's health check is still registered for Ambit objects.
if kubectl get namespace argocd >/dev/null 2>&1; then
  cm=$(kubectl -n argocd get configmap argocd-cm -o json | uv run --no-project python -c 'import json,sys;print(" ".join(k for k in json.load(sys.stdin)["data"] if "ambitproject.dev" in k))')
  [[ "$cm" == *Server* && "$cm" == *AccessRule* ]] || fail "C13 Argo CD has no Ambit health check: '$cm'"
  pass "C13 Argo CD health check registered ($cm)"
fi
done_step contracts
