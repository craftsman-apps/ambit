#!/usr/bin/env bash
# Audit-only mode (#81) on kind.
# Step 0 · the access-log policy ambit-access-log moves from the chart to the controller, under the same name:
#   install the previous release (evidence on), upgrade to this chart and measure the gap without an access-log
#   policy; the new controller is not ready before it wrote the policy. Roll back with the documented commands
#   (scale the controller to 0, wait until its pods are gone, delete the policy, helm rollback); without the
#   delete, helm rollback fails. After the rollback the controller is back to the old chart's replica count.
#   Upgrade again. Records reach ClickHouse after each move.
# Steps 1-8 · enforce -> audit -> enforce while a loop calls a model and a tool every 0.5 s as alice (group eng: the
#   rules refuse her) and mira (group ml-research: allowed). Each step of each switch must already be in every
#   gateway pod's /config_dump when the controller says it is active. In audit mode alice is served and her records
#   say refuse:model / refuse:tool; every call served in audit mode has a mark; nothing else changes. Then a rules
#   change the API server refuses stops at its step (RulesNotActive). Python parts: test/stepaudit.py.
# Reinstalls Ambit: run under the shared kind lock. Needs this checkout's image in the cluster (make image).
#   AUDIT_STEPS             "0", "1" (steps 1-8) or "0 1" (default)
#   MCP, KEYCLOAK           the gateway's MCP URL and Keycloak as the host reaches them (defaults: port 8080)
#   AMBIT_PREVIOUS_VERSION  the release to upgrade from (default 0.1.0)
#   AMBIT_LLM_URL           the gateway as the host reaches it (default http://llm.localtest.me:8080)
#   AMBIT_EXTRA_VALUES      extra values for this checkout's chart (e.g. another image tag)
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
PREV=${AMBIT_PREVIOUS_VERSION:-0.1.0}
LLM=${AMBIT_LLM_URL:-http://llm.localtest.me:8080}
NEW=(-f charts/ambit/values-kind.yaml --set evidence.enabled=true)
watcher=""
trap 'rc=$?; [[ -n "$watcher" ]] && pkill -P "$watcher" 2>/dev/null; on_exit $rc' EXIT

ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
utc() { date -u +"%Y-%m-%d %H:%M:%S"; }
# records_reach_clickhouse WHAT: a refused gateway request (no token) is recorded too; its record must arrive.
records_reach_clickhouse() {
  local since n=0; since=$(utc); sleep 1
  for _ in $(seq 1 45); do
    curl -s -o /dev/null -H 'Content-Type: application/json' -d '{"model":"none","messages":[]}' "$LLM/v1/chat/completions" || true
    n=$(ch "SELECT count() FROM otel.otel_logs WHERE Timestamp >= '$since'" 2>/dev/null || echo 0)
    (( n > 0 )) && break; sleep 2
  done
  (( n > 0 )) || fail "$1: no gateway record reached ClickHouse within 90 s"
  pass "$1: gateway records reach ClickHouse"
}
policy() { kubectl -n "$NS" get agentgatewaypolicy ambit-access-log -o json 2>/dev/null; }
owner_of_policy() { policy | uv run --no-project python -c 'import sys,json
m=json.load(sys.stdin)["metadata"]; print(m.get("labels",{}).get("app.kubernetes.io/managed-by",""), m.get("annotations",{}).get("meta.helm.sh/release-name",""))'; }
wait_controller_policy() {
  for _ in $(seq 1 60); do [[ $(owner_of_policy 2>/dev/null) == ambit-controller* ]] && return 0; sleep 2; done
  fail "$1: ambit-access-log is not the controller's after 120 s"
}

want() { [[ " ${AUDIT_STEPS:-0 1} " == *" $1 "* ]]; }
if want 0; then
# 0a · the previous release, with its own chart's access-log policy (third-party CRDs and ambit-crds of this
#      checkout: they only add fields).
kubectl apply --server-side --force-conflicts -f deploy/crds/ >/dev/null
command helm upgrade --install ambit-crds charts/ambit-crds -n "$NS" --create-namespace --wait >/dev/null
command helm uninstall ambit -n "$NS" --wait >/dev/null 2>&1 || true
# A failed rollback (0c of an earlier run, Helm 4) leaves objects that the uninstall does not remove.
kubectl -n "$NS" delete agentgatewaypolicy ambit-access-log --ignore-not-found >/dev/null
kubectl -n "$NS" delete configmap ambit-access-log-base ambit-access-mode --ignore-not-found >/dev/null
git show "v$PREV:charts/ambit/values-kind.yaml" > "$W/prev-values.yaml"
command helm install ambit oci://ghcr.io/craftsman-apps/charts/ambit --version "$PREV" -n "$NS" -f "$W/prev-values.yaml" \
  --set evidence.enabled=true --set console.image.repository=ghcr.io/craftsman-apps/ambit-console --set console.image.tag="$PREV" \
  --set console.image.pullPolicy=IfNotPresent --wait --timeout 10m >/dev/null || fail "0a install of release $PREV"
[[ $(owner_of_policy) == *" ambit" ]] || fail "0a release $PREV: ambit-access-log is not Helm's ($(owner_of_policy))"
pass "0a release $PREV installed; its chart owns ambit-access-log"
records_reach_clickhouse "0a release $PREV"
prev_rev=$(command helm history ambit -n "$NS" -o json | uv run --no-project python -c 'import sys,json; print(json.load(sys.stdin)[-1]["revision"])')

# 0b · upgrade (plain helm upgrade, no --wait) while watching the policy: the gap between Helm's delete and the
#      controller's create.
# In a subshell: stopping it stops both commands (the watch alone never ends while the policy stays the same).
(kubectl -n "$NS" get agentgatewaypolicy -w --output-watch-events --field-selector metadata.name=ambit-access-log \
  -o jsonpath='{.type} {.object.metadata.labels.app\.kubernetes\.io/managed-by}{"\n"}' 2>/dev/null |
  perl -MTime::HiRes=time -ne '$|=1; printf "%.3f %s", time, $_' > "$W/watch") &
watcher=$!
sleep 2
helm upgrade ambit charts/ambit -n "$NS" "${NEW[@]}" >/dev/null || fail "0b helm upgrade to this chart"
kubectl -n "$NS" rollout status deploy/ambit-controller --timeout=300s >/dev/null || fail "0b controller rollout"
wait_controller_policy "0b"
sleep 3; { pkill -P "$watcher"; wait "$watcher"; } 2>/dev/null || true; watcher=""
ready=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=ambit-controller -o json | uv run --no-project python -c 'import sys,json
pods=[p for p in json.load(sys.stdin)["items"] if not p["metadata"].get("deletionTimestamp")]
print(max(c["lastTransitionTime"] for p in pods for c in p["status"]["conditions"] if c["type"]=="Ready" and c["status"]=="True"))')
out=$(uv run --no-project python - "$W/watch" "$ready" <<'PY'
import sys, datetime
lines = [l.split() for l in open(sys.argv[1]) if l.strip()]
ready = datetime.datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00")).timestamp()
deleted = next((float(l[0]) for l in lines if l[1] == "DELETED"), None)
if deleted is None:
    sys.exit("FAIL no DELETED event: Helm did not delete the chart's ambit-access-log\n" + open(sys.argv[1]).read())
added = next((float(l[0]) for l in lines if float(l[0]) > deleted and l[1] in ("ADDED", "MODIFIED") and l[2:] == ["ambit-controller"]), None)
if added is None:
    sys.exit("FAIL the controller did not create ambit-access-log after Helm deleted it\n" + open(sys.argv[1]).read())
first = next(float(l[0]) for l in lines if l[2:] == ["ambit-controller"])
# Ready's time has whole seconds; 1 s for the clock difference.
if int(ready) < int(first) - 1:
    sys.exit(f"FAIL the controller was ready at {sys.argv[2]}, before it wrote ambit-access-log ({first:.3f})")
print(f"{added - deleted:.1f}")
PY
) || fail "0b ${out}"
gap=$out
awk -v g="$gap" 'BEGIN{exit !(g < 10)}' || fail "0b upgrade gap without an access-log policy: $gap s (target under 10 s)"
pass "0b upgrade: ambit-access-log was missing for $gap s (target under 10 s); the controller was ready only after writing it"
records_reach_clickhouse "0b this chart"

# 0c · rollback. Without the delete, Helm refuses the controller's object (the documented step is needed): Helm 3
#      with "invalid ownership metadata"; Helm 4 (server-side apply) adopts it and then fails on the fields the
#      controller manages. Helm 4 has by then rolled back some objects (the controller among them).
if err=$(command helm rollback ambit "$prev_rev" -n "$NS" 2>&1); then fail "0c helm rollback without deleting ambit-access-log succeeded"; fi
grep -q -e "invalid ownership metadata" -e 'conflicts with "ambit-controller"' <<<"$err" || fail "0c helm rollback failed, but not on ambit-access-log: $err"
pass "0c helm rollback without the delete fails on ambit-access-log ($(grep -o -m1 -e 'invalid ownership metadata' -e 'conflicts with "ambit-controller"' <<<"$err"))"
# The documented steps, with the wait between the scale-down and the delete: a controller pod that is still stopping
# writes the policy again within a second of the delete, and the rollback then fails as above (seen in a run).
kubectl -n "$NS" scale deployment ambit-controller --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=ambit-controller --timeout=120s >/dev/null
kubectl -n "$NS" delete agentgatewaypolicy ambit-access-log >/dev/null
command helm rollback ambit "$prev_rev" -n "$NS" --wait --timeout 6m >/dev/null || fail "0c helm rollback with the documented steps"
[[ $(owner_of_policy) == *" ambit" ]] || fail "0c after the rollback, ambit-access-log is not Helm's ($(owner_of_policy))"
pass "0c rollback with the documented steps: ambit-access-log is Helm's again"
# The rollback must also undo the scale-down: helm rollback --wait counts a Deployment with 0 replicas as ready, and
# the gateway writes records without a controller, so neither check above would notice a missing controller.
want=$(git show "v$PREV:charts/ambit/templates/controller/deployment.yaml" | awk '$1=="replicas:"{print $2; exit}')
got=$(kubectl -n "$NS" get deploy ambit-controller -o jsonpath='{.spec.replicas}')
[[ "$got" == "$want" ]] || fail "0c after the rollback, ambit-controller has $got replicas, want $want (release $PREV's chart)"
kubectl -n "$NS" rollout status deploy/ambit-controller --timeout=180s >/dev/null || fail "0c after the rollback, ambit-controller is not ready"
ready=$(kubectl -n "$NS" get deploy ambit-controller -o jsonpath='{.status.readyReplicas}')
[[ "$ready" == "$want" ]] || fail "0c after the rollback, $ready of $want ambit-controller pods are ready"
pass "0c rollback with the documented steps: ambit-controller is back to $want replica(s), $ready ready"
records_reach_clickhouse "0c rolled back to $PREV"

# 0d · upgrade again, for the steps that follow.
helm upgrade ambit charts/ambit -n "$NS" "${NEW[@]}" --wait --timeout 6m >/dev/null || fail "0d helm upgrade again"
wait_controller_policy "0d"
records_reach_clickhouse "0d this chart again"
fi
if want 1; then
MCP=${MCP:-http://mcp.localtest.me:8080/mcp}
SSO=(-f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml ${AMBIT_EXTRA_VALUES:+-f "$AMBIT_EXTRA_VALUES"})
U=alice V=mira   # alice: group eng, the rules refuse her; mira: group ml-research, allowed
PY=(uv run --no-project python test/stepaudit.py)
ch_json() { ch "$1 FORMAT JSONEachRow"; }
status_of() { command curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"${3:-hello}\"}]}" "$LLM/v1/chat/completions"; }
published() { kubectl -n "$NS" get configmap ambit-access-mode -o "jsonpath={.data.$1}" 2>/dev/null; }
ready_reason() { kubectl -n "$NS" get accessrule audit-ml-time -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'; }
loop="" forwards=()
trap 'rc=$?; [[ -n "$loop" ]] && kill "$loop" 2>/dev/null; for p in "${forwards[@]}"; do kill "$p" 2>/dev/null; done
  kubectl -n "$NS" delete accessrule audit-too-long --ignore-not-found >/dev/null 2>&1; on_exit $rc' EXIT

# 1 · enforce mode: alice may not use mock-echo (test/llm/mock.yaml: ml-research only) or the time server; mira may
#     use both, the whole server.
ensure_llm_setup "$NS"
command helm upgrade ambit charts/ambit -n "$NS" "${SSO[@]}" --set access.mode=enforce --wait --timeout 5m >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<'YAML'
apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata: { name: time }
spec:
  displayName: Time
  image: ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949
  transport: stdio
---
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: audit-ml-time }
spec:
  groups: [ml-research]
  servers: [{ name: time }]
YAML
kubectl -n "$NS" apply -f test/llm/router.yaml >/dev/null
mcp_ready "$NS"
wait_for 400 'try_tools "$(token $V)"; [[ "$TOOLS" == "time_convert_time time_get_current_time" ]]' || fail "1 $V does not see the two time tools ($TOOLS)"
[[ $(status_of "$(token $U)" mock/echo) == 403 ]] || fail "1 $U's call to mock/echo was not refused"
tools "$(token $U)"; [[ -z "$TOOLS" ]] || fail "1 $U sees tools: $TOOLS"
r=$(call time_get_current_time '{"timezone":"UTC"}'); [[ $r == error* ]] || fail "1 $U's tool call was not refused: $r"
[[ $(status_of "$(token $V)" mock/echo) == 200 ]] || fail "1 $V's call to mock/echo was refused"
v1=$(published rulesVersion)
[[ $(published mode) == enforce && -n "$v1" ]] || fail "1 published mode '$(published mode)', rules version '$v1'"
pass "1 enforce: $U is refused the model and the tool and sees no tools; $V sees both tools (rules version $v1)"

# 2 · the call loop, every 0.5 s, until step 5 is done.
"${PY[@]}" loop "$W/loop.jsonl" "$LLM" "$MCP" "${KEYCLOAK:-http://keycloak.localtest.me:8080}" "$U" "$V" &
loop=$!
sleep 10
[[ -s "$W/loop.jsonl" ]] || fail "2 the call loop wrote nothing"
pass "2 the call loop runs"
# Port-forwards to every gateway pod's admin port (localhost:15000 inside the pod) for /config_dump.
gw_pods=($(kubectl -n "$NS" get pod -l gateway.networking.k8s.io/gateway-name=ambit --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'))
(( ${#gw_pods[@]} > 0 )) || fail "2 no gateway pods"
dumps=()
for i in "${!gw_pods[@]}"; do
  kubectl -n "$NS" port-forward "pod/${gw_pods[$i]}" "$((25000 + i)):15000" >/dev/null 2>&1 &
  forwards+=($!) dumps+=("http://127.0.0.1:$((25000 + i))/config_dump")
done
dumps_answer() { for d in "${dumps[@]}"; do command curl -sf -o /dev/null "$d" || return 1; done; }
wait_for 30 dumps_answer || fail "2 no /config_dump from the gateway pods (${dumps[*]})"

# switch MODE STEP: helm upgrade while the loop runs; each step must be in every gateway pod's /config_dump when the
# controller says it is active; then the mode is published.
switch() {
  local t0; t0=$(date +%s)
  command helm upgrade ambit charts/ambit -n "$NS" "${SSO[@]}" --set access.mode="$1" >/dev/null || fail "$2 helm upgrade to $1"
  "${PY[@]}" switch "$NS" "$1" "$t0" "$W/switch-$1.json" "${dumps[@]}" > "$W/switch-$1.out" || fail "$2 switch to $1: $(cat "$W/switch-$1.out")"
  pass "$2 switch to $1, each step in every gateway pod's /config_dump when active: $(cat "$W/switch-$1.out")"
}

# 3 · enforce -> audit while the loop runs.
switch audit 3
wait_for 60 '[[ $(status_of "$(token $U)" mock/echo) == 200 ]]' || fail "3 $U is still refused mock/echo in audit mode"
mcp_open "$(token $U)"; r=$(call time_get_current_time '{"timezone":"UTC"}'); [[ $r == ok* ]] || fail "3 $U's tool call in audit mode: $r"
tools "$(token $U)"; [[ "$TOOLS" == "time_convert_time time_get_current_time" ]] || fail "3 $U's tools/list in audit mode: '$TOOLS' (want both, unfiltered)"
[[ $(published rulesVersion) == "$v1" ]] || fail "3 the rules version changed with the mode ($v1 -> $(published rulesVersion))"
c=$(kubectl -n "$NS" get accessrule audit-ml-time -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
[[ $c == *"audit-only mode: recorded, not enforced" ]] || fail "3 AccessRule message: $c"
pass "3 audit: $U's model and tool calls are served, her tools/list is unfiltered, the AccessRules say recorded, not enforced"

# 4 · the records say what the rules in force would have done. First routed calls (router auto: "python" goes to
#     mock-echo, the default is mock-unpriced): the record names the model that answered (spec open point 1).
since=$(utc); sleep 1
[[ $(status_of "$(token $V)" auto "a python traceback") == 200 && $(status_of "$(token $U)" auto "a python traceback") == 200 ]] || fail "4 routed calls"
q="SELECT LogAttributes['jwt.email'] AS who, LogAttributes['mcp.method.name'] AS method, LogAttributes['gen_ai.request.model'] AS model,
  LogAttributes['ambit.wouldRefuse'] AS mark, LogAttributes['ambit.wouldRefuseTool'] AS toolmark FROM otel.otel_logs"
wait_for 120 'ch_json "$q WHERE Timestamp >= '"'"'$since'"'"' AND LogAttributes['"'"'ambit.router'"'"'] = '"'"'auto'"'"'" > "$W/routed.json"; "${PY[@]}" routed "$W/routed.json"' ||
  fail "4 routed records: $(cat "$W/routed.json")"
pass "4 routed records: gen_ai.request.model is the answering model (mock/echo); marks allow ($V) and other-model ($U)"
since=$(utc); sleep 1
tools "$(token $U)"
wait_for 120 'ch_json "$q WHERE Timestamp >= '"'"'$since'"'"' AND LogAttributes['"'"'ambit.accessMode'"'"'] = '"'"'audit'"'"'" > "$W/r4.json"; "${PY[@]}" audit4 "$W/r4.json"' ||
  fail "4 audit records within 2 minutes: $(head -c 3000 "$W/r4.json")"
pass "4 audit records: $U refuse:model and refuse:tool, $V allow, tools/list n/a"

# 5 · audit -> enforce while the loop runs.
switch enforce 5
wait_for 60 '[[ $(status_of "$(token $U)" mock/echo) == 403 ]]' || fail "5 $U is not refused again"
[[ $(status_of "$(token $V)" mock/echo) == 200 ]] || fail "5 $V is refused after the switch back"
pass "5 enforce again: $U is refused, $V is served"
sleep 5; kill "$loop"; wait "$loop" 2>/dev/null || true; loop=""

# 6, 7 · the loop against the records: every call served in audit mode has a mark; alice is served only between the
#        switches and always with refuse:*; mira is always served; no 5xx, no connection error.
IFS='|' read -r first last < <(uv run --no-project python -c 'import json,sys,datetime
t=[json.loads(l)["start"] for l in open(sys.argv[1])]; f=lambda x: datetime.datetime.fromtimestamp(x, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
print(f(min(t)-1) + "|" + f(max(t)+2))' "$W/loop.jsonl")
wait_for 120 '[[ $(ch "SELECT count() FROM otel.otel_logs WHERE Timestamp > '"'"'$last'"'"'") -gt 0 ]] || { status_of x mock/echo >/dev/null; false; }' || true
ch_json "SELECT toUnixTimestamp64Nano(Timestamp)/1e9 AS t, LogAttributes['jwt.email'] AS who, LogAttributes['mcp.method.name'] AS method,
  LogAttributes['gen_ai.request.model'] AS model, LogAttributes['ambit.accessMode'] AS mode, LogAttributes['ambit.wouldRefuse'] AS mark,
  LogAttributes['ambit.wouldRefuseTool'] AS toolmark, LogAttributes['http.response.status_code'] AS status FROM otel.otel_logs
  WHERE Timestamp BETWEEN '$first' AND '$last' AND (
    (LogAttributes['jwt.email'] IN ('alice@acme.test', 'mira@acme.test')
      AND (LogAttributes['gen_ai.request.model'] = 'mock/echo' OR LogAttributes['mcp.method.name'] = 'tools/call'))
    -- agentgateway answers a model refusal before it takes the request snapshot: no identity, no model in the record
    OR (LogAttributes['protocol'] = 'llm' AND LogAttributes['jwt.email'] = '' AND LogAttributes['http.response.status_code'] = '403'))
  ORDER BY t" > "$W/records.json"
"${PY[@]}" analyze "$W/loop.jsonl" "$W/records.json" "$W/switch-audit.json" "$W/switch-enforce.json" > "$W/analyze.out" ||
  fail "6/7 the loop and the records disagree: $(cat "$W/analyze.out")"
pass "6/7 $(cat "$W/analyze.out" | tr '\n' ' ')"

# 8 · a rules change the API server refuses (an AccessRule with 700 groups on mock-echo: its CEL is above
#     agentgateway's 16384-character limit) stops at the authorization step: RulesNotActive, the old version kept.
groups=$(uv run --no-project python -c 'print(", ".join(f"g{i:03d}-a-long-group-name" for i in range(700)))')
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: audit-too-long }
spec:
  groups: [$groups]
  models: [mock-echo]
YAML
wait_for 120 '[[ $(ready_reason) == RulesNotActive ]]' || fail "8 no RulesNotActive: $(kubectl -n "$NS" get accessrule audit-ml-time -o jsonpath='{.status.conditions}')"
[[ $(published rulesVersion) == "$v1" ]] || fail "8 the refused change was published ($(published rulesVersion), want $v1)"
[[ $(status_of "$(token $U)" mock/echo) == 403 && $(status_of "$(token $V)" mock/echo) == 200 ]] || fail "8 the results changed"
msg=$(kubectl -n "$NS" get accessrule audit-ml-time -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
pass "8 a refused rules change: RulesNotActive (${msg:0:200}), the old rules version $v1 kept, results unchanged"
kubectl -n "$NS" delete accessrule audit-too-long >/dev/null
wait_for 120 '[[ $(ready_reason) == Compiled ]]' || fail "8 the rules did not come back after the refused rule was deleted"
fi
done_step audit
