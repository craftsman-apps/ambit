#!/usr/bin/env bash
# Step 4b acceptance: a user requests an MCP server in the console, an approver approves the exact image
# digest, ToolHive starts it, the controller reads its tools, and an admin grants one tool in the rules form.
# Journeys A1 (add a server with approval), U3 (see what you may use) and U5 (tool sets per group).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
IMAGE=ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
cleanup() {
  kubectl -n "$NS" delete "servers.$G" time-b --ignore-not-found >/dev/null
  kubectl -n "$NS" delete "serverrequests.$G" --all >/dev/null
  kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
cleanup
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null || fail "0 test servers not Ready"

# 1 · the controller reads each running server's tools into its status (through the NetworkPolicy)
for _ in $(seq 1 60); do
  n=$(kubectl -n "$NS" get "servers.$G" time -o jsonpath='{.status.tools[*].name}'); [[ "$n" == "convert_time get_current_time" ]] && break; sleep 1
done
[[ "$n" == "convert_time get_current_time" ]] || fail "1 time tools in status: '$n'"
pass "1 Server time lists its tools in status: $n"

# 2 · alice (viewer) asks for a server; nothing runs yet
sign_in keycloak alice alice-test-pass
code=$(api POST /api/server-requests "{\"serverName\":\"time-b\",\"displayName\":\"Time, second copy\",\"image\":\"$IMAGE\",\"reason\":\"step 4b test\"}")
[[ "$code" == 201 ]] || fail "2 request: $code $(cat "$W/api")"
REQ=$(field 'd["name"]')
[[ $(field 'd["requestedBy"]') == alice@acme.test ]] || fail "2 requestedBy: $(cat "$W/api")"
kubectl -n "$NS" get "servers.$G" time-b >/dev/null 2>&1 && fail "2 server exists before approval"
code=$(api POST "/api/server-requests/$REQ/approve" '{}')
[[ "$code" == 403 ]] || fail "2 viewer approved: $code"
code=$(api POST /api/server-requests "{\"serverName\":\"time-c\",\"image\":\"ghcr.io/stacklok/dockyard/uvx/mcp-server-time:2026.8.18\"}")
[[ "$code" == 400 && "$(cat "$W/api")" == *digest* ]] || fail "2 tag accepted: $code $(cat "$W/api")"
pass "2 alice requested time-b ($REQ); no Server yet; she cannot approve; a tag-only image is refused"

# 3 · separation of duties: an approver cannot approve their own request
sign_in keycloak sam sam-test-pass
code=$(api POST /api/server-requests "{\"serverName\":\"time-c\",\"image\":\"$IMAGE\"}")
[[ "$code" == 201 ]] || fail "3 sam request: $code"
OWN=$(field 'd["name"]')
code=$(api POST "/api/server-requests/$OWN/approve" '{}')
[[ "$code" == 403 && "$(cat "$W/api")" == *"another approver"* ]] || fail "3 own approval: $code $(cat "$W/api")"
pass "3 sam (approver) cannot approve his own request: $(field 'd["error"]')"

# 4 · sam approves alice's request: the Server is created with the record, starts, and lists its tools
code=$(api POST "/api/server-requests/$REQ/approve" '{"comment":"digest matches the catalog"}')
[[ "$code" == 200 ]] || fail "4 approve: $code $(cat "$W/api")"
[[ $(kubectl -n "$NS" get "servers.$G" time-b -o jsonpath='{.metadata.annotations.ambitproject\.dev/approved-by} {.metadata.annotations.ambitproject\.dev/requested-by} {.spec.image}') == "sam@acme.test alice@acme.test $IMAGE" ]] ||
  fail "4 server record: $(kubectl -n "$NS" get "servers.$G" time-b -o yaml)"
[[ $(kubectl -n "$NS" get "serverrequests.$G" "$REQ" -o jsonpath='{.status.decision} {.status.decidedBy}') == "Approved sam@acme.test" ]] || fail "4 request status"
kubectl -n "$NS" wait "servers.$G" time-b --for=condition=Ready --timeout=300s >/dev/null || fail "4 time-b not Ready"
for _ in $(seq 1 60); do n=$(kubectl -n "$NS" get "servers.$G" time-b -o jsonpath='{.status.tools[*].name}'); [[ -n "$n" ]] && break; sleep 1; done
[[ "$n" == "convert_time get_current_time" ]] || fail "4 time-b tools: '$n'"
code=$(api POST "/api/server-requests/$REQ/deny" '{}')
[[ "$code" == 409 ]] || fail "4 second decision: $code"
pass "4 sam approved; time-b created with requested-by alice, approved-by sam, exact digest; Running with tools: $n"

# 5 · approval alone grants nothing: alice cannot see time-b's tools yet
tools "$(token alice)"
[[ "$TOOLS" != *time-b_* ]] || fail "5 alice sees time-b tools before any rule: $TOOLS"
pass "5 approved server is still closed until a rule names it (alice: $TOOLS)"

# 6 · priya (admin) grants eng one tool of time-b in the rules form; alice sees it, and only it
sign_in keycloak priya priya-test-pass
code=$(api PUT /api/access-rules/eng-time '{"groups":["eng"],"allSignedIn":false,"models":[],"servers":[{"name":"time","tools":["get_current_time"]},{"name":"time-b","tools":["convert_time"]}]}')
[[ "$code" == 200 ]] || fail "6 rule update: $code $(cat "$W/api")"
code=$(api GET /api/access-rules/eng-time)
cel=$(field '[e["cel"] for e in d["effectiveServers"] if e["server"] == "time-b"][0]')
[[ "$cel" == 'mcp.tool.target == "time-b" && mcp.tool.name == "convert_time" && jwt.groups.exists(g, g in ["eng"])' ]] ||
  fail "6 rule detail shows: $cel"
for _ in $(seq 1 30); do tools "$(token alice)"; [[ "$TOOLS" == *time-b_convert_time* ]] && break; sleep 1; done
[[ "$TOOLS" == "time-b_convert_time time_get_current_time" ]] || fail "6 alice sees: $TOOLS"
pass "6 rule edited in the console -> alice sees exactly: $TOOLS"

# 7 · U3: alice can see what she may use and which rule grants it
sign_in keycloak alice alice-test-pass
code=$(api GET /api/me/access)
got=$(field '" ".join(s["server"]+":"+",".join(s["tools"])+"@"+",".join(s["rules"]) for s in d["servers"])')
[[ "$got" == "time:get_current_time@eng-time time-b:convert_time@eng-time" ]] || fail "7 my access: $got"
pass "7 alice's own access page: $got"
done_step 4b
