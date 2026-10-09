#!/usr/bin/env bash
# Step 4a acceptance: Server + AccessRule.servers -> ambit-controller -> ToolHive MCPServer, one multiplexed
# AgentgatewayBackend and a per-tool access policy. Journeys U5 (each group sees only its tools) and
# U3 for tools (a denied tool is refused), plus A1's "nobody reaches a server except through the gateway".
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
CURL_IMAGE=curlimages/curl:8.20.0@sha256:b3f1fb2a51d923260350d21b8654bbc607164a987e2f7c84a0ac199a67df812a

policy_expr() { kubectl -n "$NS" get agentgatewaypolicy ambit-mcp-access -o jsonpath='{.spec.backend.mcp.authorization.policy.matchExpressions[0]}'; }
wait_for() { # wait_for DESCRIPTION CONDITION: evaluates CONDITION every second for 60 s
  for _ in $(seq 1 60); do eval "$2" && return 0; sleep 1; done
  fail "$1"
}

ensure_llm_setup "$NS"   # Keycloak, SSO and gateway values (values-sso.yaml turns MCP on)
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null

# 1 · servers running in ToolHive, rules compiled, generated backend and policy accepted
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null ||
  fail "1 servers not Ready: $(kubectl -n "$NS" get "servers.$G" -o wide)"
kubectl -n "$NS" wait "accessrules.$G" --all --for=condition=Ready --timeout=60s >/dev/null || fail "1 access rules not Ready"
kubectl -n "$NS" wait agentgatewaybackend/ambit-mcp --for=condition=Accepted --timeout=60s >/dev/null || fail "1 backend not accepted"
# Policies report per attachment point (status.ancestors), not at the top level.
wait_for "1 access policy not accepted" '[[ $(kubectl -n "$NS" get agentgatewaypolicy ambit-mcp-access -o jsonpath="{.status.ancestors[*].conditions[?(@.type==\"Accepted\")].status}") == True ]]'
pass "1 2 servers Ready in ToolHive; backend and access policy accepted by agentgateway"

# 2 · no token
code=$(command curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' "$MCP")
[[ "$code" == 401 ]] || fail "2 request without a token returned $code"
pass "2 no token -> 401"

# 3 · U5: alice (eng) sees exactly the one tool her rule names
tools "$(token alice)"
[[ "$TOOLS" == "time_get_current_time" ]] || fail "3 alice sees: $TOOLS"
pass "3 alice (eng) tools/list: $TOOLS"

# 4 · allowed call works; tools outside her rule are refused, on the same server and on another
r=$(call time_get_current_time '{"timezone":"Asia/Seoul"}')
[[ "$r" == ok*Asia/Seoul* ]] || fail "4 allowed call: $r"
r1=$(call time_convert_time '{"source_timezone":"UTC","time":"12:00","target_timezone":"Asia/Seoul"}')
r2=$(call everything_echo '{"message":"hello"}')
[[ "$r1" == error* && "$r2" == error* ]] || fail "4 denied calls went through: $r1 / $r2"
pass "4 alice: get_current_time ok; convert_time and everything_echo refused ($r1)"

# 5 · mira (ml-research) gets every tool on both servers
tools "$(token mira)"
n=$(wc -w <<<"$TOOLS" | tr -d ' ')
[[ "$TOOLS" == *everything_echo* && "$TOOLS" == *time_convert_time* && "$TOOLS" == *time_get_current_time* && $n -ge 15 ]] ||
  fail "5 mira sees: $TOOLS"
r=$(call everything_echo '{"message":"hello from mira"}')
[[ "$r" == ok*"hello from mira"* ]] || fail "5 mira echo: $r"
pass "5 mira (ml-research) sees $n tools and calls everything_echo"

# 6 · deny by default: with no rule for her group, mira sees nothing
kubectl -n "$NS" delete "accessrules.$G" ml-research-tools >/dev/null
wait_for "6 policy still names ml-research after the rule was deleted" '[[ $(policy_expr) != *ml-research* ]]'
wait_for "6 mira still sees tools: $TOOLS" 'try_tools "$(token mira)"; [[ -z "$TOOLS" ]]'
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
wait_for "6 rule not restored" '[[ $(policy_expr) == *ml-research* ]]'
pass "6 no rule for ml-research -> mira's tools/list is empty; rule restored"

# 7 · a rule naming a missing server says which name
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: typo-tools }
spec: { groups: [eng], servers: [{ name: tiem }] }
YAML
wait_for "7 typo rule not reported" '[[ $(kubectl -n "$NS" get "accessrules.$G" typo-tools -o jsonpath="{.status.conditions[?(@.type==\"Ready\")].reason}") == UnknownServer ]]'
msg=$(kubectl -n "$NS" get "accessrules.$G" typo-tools -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
kubectl -n "$NS" delete "accessrules.$G" typo-tools >/dev/null
[[ "$msg" == *tiem* ]] || fail "7 message: $msg"
pass "7 rule with an unknown server -> Ready=False UnknownServer: $msg"

# 8 · a manual edit that opens every tool to everyone is reverted
want=$(policy_expr)
kubectl -n "$NS" patch agentgatewaypolicy ambit-mcp-access --type=json \
  -p '[{"op":"replace","path":"/spec/backend/mcp/authorization/policy/matchExpressions","value":["true"]}]' >/dev/null
wait_for "8 manual edit not reverted" '[[ $(policy_expr) == "$want" ]]'
pass "8 manual edit of the access policy reverted by the controller"

# 9 · GitOps rules: owners set, no Argo CD metadata, Ambit objects' spec never written
for o in mcpserver/time agentgatewaybackend/ambit-mcp agentgatewaypolicy/ambit-mcp-access; do
  kubectl -n "$NS" get "$o" -o json | uv run --no-project python -c '
import json,sys
md=json.load(sys.stdin)["metadata"]; kind=sys.argv[1]
refs=md.get("ownerReferences",[])
assert refs and all(r["kind"]=="Server" for r in refs), refs
if kind.startswith("mcpserver/"): assert len(refs)==1 and refs[0]["name"]=="time" and refs[0].get("controller"), refs
else: assert sorted(r["name"] for r in refs)==["everything","time"] and not any(r.get("controller") for r in refs), refs
assert not [k for k in md.get("annotations",{}) if k.startswith("argocd.argoproj.io/")], md.get("annotations")
assert "app.kubernetes.io/instance" not in md.get("labels",{}), md.get("labels")' "$o" || fail "9 $o breaks a GitOps rule"
done
gens=$(kubectl -n "$NS" get "servers.$G" -o jsonpath='{range .items[*]}{.metadata.generation}{" "}{end}')
[[ -z "$(tr -d '1 ' <<<"$gens")" ]] || fail "9 controller changed a Server's spec (generations: $gens)"
pass "9 MCPServer owned by its Server; shared backend and policy owned by every Server; no Argo CD metadata"

# 10 · only the gateway reaches MCP servers. The same probe with the gateway's label is the control:
# it must get an HTTP answer, so the blocked case is the NetworkPolicy and not DNS or the image.
# probe NAME [LABELS]: HTTP status from a pod calling the ToolHive proxy directly. Runs to completion and
# reads the log: attaching with "kubectl run -i" loses the output when the pod finishes first.
probe() {
  kubectl -n "$NS" delete pod "$1" --ignore-not-found >/dev/null
  kubectl -n "$NS" run "$1" --restart=Never --image="$CURL_IMAGE" ${2:+--labels="$2"} --command -- \
    curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "http://mcp-time-proxy.$NS.svc:8080/mcp" >/dev/null
  # A blocked request times out, curl exits non-zero and the pod ends Failed: wait for either phase.
  for _ in $(seq 1 120); do
    [[ $(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null) =~ ^(Succeeded|Failed)$ ]] && break; sleep 1
  done
  kubectl -n "$NS" logs "$1" 2>/dev/null | head -c 3
  kubectl -n "$NS" delete pod "$1" --wait=false >/dev/null
}
allowed=$(probe mcp-probe-gw gateway.networking.k8s.io/gateway-name=ambit)
blocked=$(probe mcp-probe)
[[ "$allowed" =~ ^[1-5][0-9][0-9]$ ]] || fail "10 control probe with the gateway label got no HTTP answer ($allowed)"
[[ "$blocked" == 000 ]] || fail "10 direct request to the ToolHive proxy was not blocked (HTTP $blocked)"
pass "10 direct request from a non-gateway pod blocked (timeout); same request with the gateway label answered (HTTP $allowed)"

# 11 · digest pinning is enforced by the API server, and deleting a Server removes its pods and tools
if kubectl -n "$NS" apply -f - >/dev/null 2>"$W/err" <<YAML
apiVersion: $G/v1alpha1
kind: Server
metadata: { name: tagged }
spec: { image: "ghcr.io/stacklok/dockyard/uvx/mcp-server-time:2026.8.18" }
YAML
then fail "11 a Server with a tag instead of a digest was accepted"; fi
# While a Server is deleted and created again, the other servers' users must keep working (risk 16: a Service
# backendRef made agentgateway drop the whole shared backend, HTTP 500 for every user). alice may use only time.
# A server joins the list only once it answers, so a booting one must not slow them either (it took ~10 s per
# request: agentgateway introduces every new session to every target). A deleted server's pod may still hold its
# connections for a moment (up to the 2 s connect timeout, twice per probe), hence 8 s, not less.
AL=$(token alice); : > "$W/probes"
( set +e  # one timed-out curl must not end the probe loop (set -e is inherited)
  while :; do
    t0=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')
    c=$(command curl -s -m 30 -D "$W/ph" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $AL" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"r16","version":"1"}}}' "$MCP")
    sid=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/ph" | tr -d '\r'); r=fail
    if [[ "$c" == 200 && -n "$sid" ]] && command curl -s -m 30 -H "Authorization: Bearer $AL" -H "Mcp-Session-Id: $sid" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "$MCP" | grep -q get_current_time; then r=ok; fi
    echo "$c $r $(( $(perl -MTime::HiRes=time -e 'printf "%d", time*1000') - t0 ))" >> "$W/probes"
  done ) & probes=$!
kubectl -n "$NS" delete "servers.$G" everything >/dev/null
wait_for "11 MCPServer everything not garbage-collected" '! kubectl -n "$NS" get mcpserver everything >/dev/null 2>&1'
wait_for "11 mira still sees everything_* tools: $TOOLS" 'try_tools "$(token mira)"; [[ $TOOLS != *everything_* && $TOOLS == *time_get_current_time* ]]'
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null || fail "11 everything not Ready again"
sleep 10; kill "$probes" 2>/dev/null || true; wait "$probes" 2>/dev/null || true
n=$(wc -l <"$W/probes" | tr -d ' '); bad=$(grep -c ' fail ' "$W/probes" || true)
slowest=$(awk '$3>m{m=$3} END{print m+0}' "$W/probes")
[[ "$slowest" -le 8000 ]] || fail "11 another Server's create or delete slowed alice's new sessions: slowest $slowest ms"
[[ "$n" -ge 20 && "$bad" -eq 0 ]] || fail "11 alice's sessions failed while another Server was deleted and created: $bad of $n (codes: $(grep ' fail ' "$W/probes" | cut -d' ' -f1 | sort | uniq -c | tr -s ' \n' ' '))"
pass "11 tag-only image rejected: $(tr -d '\n' <"$W/err" | sed 's/.*spec.image//' | cut -c1-90); deleted Server's tools gone, restored; other users unaffected ($n sessions, 0 failed, slowest $slowest ms)"

# 12 · chart renders identically with MCP on
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml > "$W/r1"
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml > "$W/r2"
cmp -s "$W/r1" "$W/r2" || fail "12 two renders differ"
pass "12 chart renders identically"
done_step 4a
