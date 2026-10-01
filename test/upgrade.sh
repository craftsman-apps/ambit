#!/usr/bin/env bash
# Upgrade verification: does Ambit still work with a new version of one component it ships?
#   test/upgrade.sh COMPONENT VERSION [STEPS]
#   COMPONENT  agentgateway | toolhive (VERSION = chart version, CRDs included)
#              clickhouse | collector | ratelimit | valkey (VERSION = image, pinned by digest if you can)
#              dex (VERSION = image tag)
#   STEPS      acceptance steps to run on the candidate; default: every step and the console UI tests
#              (4c only when the GitHub App key exists)
# Order: baseline → state created on the pinned versions → upgrade with 2 gateway replicas under traffic →
# that state still works → contracts → steps and UI → roll back → contracts again → report.
# Changes the kind cluster for the run and puts the pinned versions back at the end, also when interrupted.
# Exit code 0 only when nothing failed.
set -euo pipefail
COMP=${1:?component}; VER=${2:?version}
ALL="0 1 2 3 3b 3c 4a 4b 4c 4d 5 6 7 8 9 10 ext ui"  # add new steps here too
no4c=""
[[ -s "$HOME/.config/ambit/github-app.pem" ]] || { ALL=${ALL/ 4c / }; no4c=1; }
STEPS=${3:-$ALL}
NS=ambit-system
cd "$(dirname "$0")/.."
source test/lib.sh
CAND=$W/candidate; mkdir -p "$CAND"
G=ambitproject.dev
# Two gateway replicas for the whole run: old and new versions serve side by side during the rollout.
printf 'gateway:\n  replicas: 2\n' > "$CAND/replicas.yaml"
REPORT=${UPGRADE_REPORT:-$PWD/upgrade-report-$COMP.txt}
results=()
note() { results+=("$1"); echo "$1"; }
crds() { kubectl apply --server-side --force-conflicts -f "$1" >/dev/null; }
proxy_image() { kubectl -n "$NS" get deploy -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].spec.template.spec.containers[0].image}'; }

# One run at a time: two runs on one cluster corrupt each other's results. The lock is a ConfigMap, so any
# process sees it; one older than 3 hours is stale (a run that was killed).
lock=ambit-upgrade-check-lock
if ! kubectl -n "$NS" create configmap "$lock" --from-literal=since="$(date -u +%s)" --from-literal=what="$COMP $VER" >/dev/null 2>&1; then
  since=$(kubectl -n "$NS" get configmap "$lock" -o jsonpath='{.data.since}' 2>/dev/null || echo 0)
  if (( $(date -u +%s) - since > 10800 )); then
    kubectl -n "$NS" delete configmap "$lock" >/dev/null
    kubectl -n "$NS" create configmap "$lock" --from-literal=since="$(date -u +%s)" --from-literal=what="$COMP $VER" >/dev/null
  else
    echo "FAIL another upgrade check is running ($(kubectl -n "$NS" get configmap "$lock" -o jsonpath='{.data.what}')); wait for it, or delete configmap $lock if it was killed"
    exit 1
  fi
fi
unlock() { kubectl -n "$NS" delete configmap "$lock" --ignore-not-found >/dev/null 2>&1 || true; }

# gateway_ready: a pod can report ready before its configuration has arrived; wait until tools/list
# answers correctly 5 times in a row.
gateway_ready() {
  local ok=0 t; t=$(token alice)
  for _ in $(seq 1 90); do
    if [[ -n "$( (tools "$t" >/dev/null 2>&1 && echo "$TOOLS") )" ]]; then ok=$((ok + 1)); else ok=0; fi
    (( ok >= 5 )) && return 0; sleep 2
  done
  fail "the gateway did not answer tools/list 5 times in a row"
}

# 0 · a run that was killed (no trap runs on SIGKILL) can leave a candidate installed: restore the pins
#     first, so the baseline really is the pinned versions.
if [[ "${1:-}" != restore ]]; then
  pinned=$(uv run --no-project --with pyyaml python -c "import yaml;print([d['version'] for d in yaml.safe_load(open('charts/ambit/Chart.yaml'))['dependencies'] if d['name']=='agentgateway'][0])")
  if [[ $(proxy_image) != *":$pinned" ]] || kubectl -n "$NS" get "budgets.ambitproject.dev" upgrade-eng >/dev/null 2>&1; then
    echo "a previous run left the cluster changed ($(proxy_image)); restoring the pinned versions first"
    kubectl apply --server-side --force-conflicts -f deploy/crds/ >/dev/null
    kubectl -n "$NS" delete "budgets.ambitproject.dev" upgrade-eng contract-eng --ignore-not-found >/dev/null
    command helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 6m >/dev/null
  fi
fi

# 1 · prepare the candidate (the repository is not changed)
case "$COMP" in
  agentgateway|toolhive)
    sub=$([[ "$COMP" == agentgateway ]] && echo agentgateway || echo toolhive-operator)
    FROM=$(uv run --no-project --with pyyaml python -c "import yaml;print([d['version'] for d in yaml.safe_load(open('charts/ambit/Chart.yaml'))['dependencies'] if d['name']=='$sub'][0])")
    cp -R charts/ambit "$CAND/chart"; rm -rf "$CAND/chart/charts"
    uv run --no-project --with pyyaml python - "$CAND/chart/Chart.yaml" "$sub" "$VER" <<'PY'
import sys, yaml
p, sub, ver = sys.argv[1:4]
c = yaml.safe_load(open(p))
for d in c["dependencies"]:
    if d["name"] == sub:
        d["version"] = ver
yaml.safe_dump(c, open(p, "w"), sort_keys=False)
PY
    command helm dependency update "$CAND/chart" >/dev/null
    mkdir -p "$CAND/crds"
    if [[ "$COMP" == agentgateway ]]; then
      command helm template agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds --version "$VER" > "$CAND/crds/agentgateway.yaml"
    else
      command helm template toolhive-operator-crds oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds --version "$VER" > "$CAND/crds/toolhive.yaml"
    fi
    export AMBIT_CHART="$CAND/chart" AMBIT_EXTRA_VALUES="$CAND/replicas.yaml" ;;
  clickhouse|collector|ratelimit|valkey|dex)
    key=evidence.clickhouse.image
    [[ "$COMP" == collector ]] && key=evidence.collector.image
    [[ "$COMP" == ratelimit ]] && key=limits.ratelimit.image
    [[ "$COMP" == valkey ]] && key=limits.valkey.image
    [[ "$COMP" == dex ]] && key=dex.image.tag
    FROM=$(uv run --no-project --with pyyaml python -c "
import yaml; v = yaml.safe_load(open('charts/ambit/values.yaml'))
for k in '$key'.split('.'): v = v[k]
print(v)")
    uv run --no-project --with pyyaml python - "$CAND/values.yaml" "$key" "$VER" <<'PY'
import sys, yaml
p, key, ver = sys.argv[1:4]
out = cur = {}
parts = key.split(".")
for k in parts[:-1]:
    cur = cur.setdefault(k, {})
cur[parts[-1]] = ver
out["gateway"] = {"replicas": 2}
yaml.safe_dump(out, open(p, "w"))
PY
    export AMBIT_EXTRA_VALUES="$CAND/values.yaml" ;;
  *) fail "unknown component $COMP" ;;
esac
note "component     $COMP   $FROM → $VER"

restore() {
  unset AMBIT_CHART AMBIT_EXTRA_VALUES
  crds deploy/crds/ || true
  command helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 6m >/dev/null 2>&1 || true
  if [[ "$COMP" == agentgateway ]]; then
    for _ in $(seq 1 60); do [[ $(proxy_image) == *":$FROM" ]] && break; sleep 3; done
  fi
  echo "restored $COMP $FROM"
}
rollback_check() { # after a real rollback, everything Ambit relies on must work again
  if out=$(./test/contracts.sh "$NS" 2>&1); then note "rollback      PASS  contracts hold again on $FROM"
  else failed=$((failed + 1)); note "rollback      FAIL  $(grep '^FAIL' <<<"$out" | head -1)"; fi
}
trap 'rc=$?; { printf "%s\n" "${results[@]}"; } > "$REPORT"; restore; unlock; on_exit $rc' EXIT

# 2 · baseline: the pinned versions run and every contract holds (otherwise a result would mean nothing)
( unset AMBIT_CHART; export AMBIT_EXTRA_VALUES="$CAND/replicas.yaml"; ensure_llm_setup "$NS"; kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null; gateway_ready
  out=$(./test/contracts.sh "$NS" 2>&1) || { grep -E '^FAIL' <<<"$out"; exit 1; } ) || fail "baseline: the pinned install does not pass the contract checks"
note "baseline      PASS  pinned versions healthy, 2 gateway replicas"

# 2b · state created on the pinned versions, checked again after the upgrade
A=$(token alice); M=$(token mira)
tools "$A"; call time_get_current_time '{"timezone":"UTC"}' >/dev/null
before=$(date -u +%s)
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: upgrade-eng }
spec: { group: eng, userRequestsPerDay: 100000 }
YAML
day=$(( before / 86400 * 86400 )); key="ambit-llm_group_eng_user_alice@acme.test_$day"
for _ in $(seq 1 60); do
  command curl -s -o /dev/null -H "Authorization: Bearer $A" -H 'Content-Type: application/json' \
    -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' http://llm.localtest.me:8080/v1/chat/completions || true
  count=$(kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli GET "$key" 2>/dev/null || true)
  [[ -n "$count" ]] && break; sleep 3
done
[[ -n "$count" ]] || fail "state: no budget counter before the upgrade"
mcp_open "$M"; sid=$SID; stok=$TOK
pins=$(kubectl -n "$NS" get "servers.$G" -o jsonpath='{range .items[*]}{.metadata.name}={range .status.conditions[?(@.type=="ToolsPinned")]}{.status}{end} {end}')
note "state         recorded: alice's tool call, budget count $count, mira's open MCP session, tool pins ($pins)"

# 3 · upgrade in place under steady MCP traffic
M=$(token mira)
( ok=0; bad=0; tok=$M; t0=$SECONDS
  while [[ ! -f "$CAND/stop" ]]; do
    (( SECONDS - t0 > 60 )) && { tok=$(token mira); t0=$SECONDS; }
    c=$(command curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"upgrade","version":"1"}}}' "$MCP")
    if [[ "$c" == 200 ]]; then ok=$((ok + 1)); else bad=$((bad + 1)); echo "$c" >> "$CAND/bad"; fi
    sleep 0.3
  done
  echo "$ok $bad" > "$CAND/traffic" ) &
traffic=$!
sleep 3
[[ -d "$CAND/crds" ]] && crds "$CAND/crds/"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 8m >/dev/null
if [[ "$COMP" == agentgateway ]]; then
  for _ in $(seq 1 80); do [[ $(proxy_image) == *":$VER" ]] && break; sleep 3; done
  [[ $(proxy_image) == *":$VER" ]] || fail "upgrade: the gateway pods never moved to $VER ($(proxy_image))"
  kubectl -n "$NS" rollout status deploy -l gateway.networking.k8s.io/gateway-name=ambit --timeout=300s >/dev/null
fi
for d in $(kubectl -n "$NS" get deploy -o name); do kubectl -n "$NS" rollout status "$d" --timeout=300s >/dev/null; done
gateway_ready
sleep 5; touch "$CAND/stop"; wait $traffic
read -r ok bad < "$CAND/traffic"
if [[ "$bad" == 0 ]]; then note "upgrade       PASS  $ok requests during the upgrade, 0 failed"
else note "upgrade       FAIL  $ok requests, $bad failed ($(sort "$CAND/bad" | uniq -c | tr -s ' ' | tr '\n' ' '))"; fi

failed=$([[ "$bad" == 0 ]] && echo 0 || echo 1)

# 3b · the state from before the upgrade still works
state=()
SID=$sid; TOK=$stok
r=$(mcp_send '{"jsonrpc":"2.0","id":7,"method":"tools/list"}' | head -c 200)
[[ "$r" == *'"tools"'* ]] || state+=("mira's open MCP session no longer answers: ${r:-no response}")
now=$(kubectl -n "$NS" exec deploy/ambit-valkey -- valkey-cli GET "$key" 2>/dev/null || true)
[[ -n "$now" && "$now" -ge "$count" ]] || state+=("budget count went from $count to '${now}'")
pins_now=$(kubectl -n "$NS" get "servers.$G" -o jsonpath='{range .items[*]}{.metadata.name}={range .status.conditions[?(@.type=="ToolsPinned")]}{.status}{end} {end}')
[[ "$pins_now" == "$pins" ]] || state+=("tool pins changed: $pins → $pins_now")
sign_in keycloak priya priya-test-pass
from=$(date -u -r "$((before - 120))" +%Y-%m-%d 2>/dev/null || date -u -d "@$((before - 120))" +%Y-%m-%d)
next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
if [[ $(api GET "/api/audit?email=alice@acme.test&from=$from&to=$next") == 200 ]]; then
  old_calls=$(uv run --no-project python -c "
import json, datetime
d = json.load(open('$W/api'))
print(sum(1 for c in d['calls'] if c['tool'] == 'get_current_time' and datetime.datetime.fromisoformat(c['time'].replace('Z', '+00:00')[:26] + '+00:00').timestamp() <= $before))")
  [[ "$old_calls" -ge 1 ]] || state+=("the audit lost alice's call from before the upgrade")
else state+=("audit search failed: $(head -c 150 "$W/api")"); fi
kubectl -n "$NS" delete "budgets.$G" upgrade-eng --ignore-not-found >/dev/null
if (( ${#state[@]} == 0 )); then note "state         PASS  session, budget count ($now), tool pins and audit records survived"
else failed=$((failed + 1)); note "state         FAIL  ${state[*]}"; fi

# 4 · contracts, 5 · acceptance steps and UI — all on the candidate; failures are collected, not fatal
if out=$(./test/contracts.sh "$NS" 2>&1); then note "contracts     PASS  $(grep -o '^PASS C[0-9]*' <<<"$out" | cut -d' ' -f2 | tr '\n' ' ')· $(grep '^PASS C3' <<<"$out" | sed 's/.*reads; //')"
else failed=$((failed + 1)); note "contracts     FAIL  $(grep '^FAIL' <<<"$out" | head -1)"; fi
for s in $STEPS; do
  if [[ "$s" == ui ]]; then
    pw=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
    if out=$(cd web && AMBIT_ADMIN_PASSWORD="$pw" npx playwright test 2>&1); then note "ui            PASS  $(grep -o '[0-9]* passed' <<<"$out" | tail -1)"
    else failed=$((failed + 1)); note "ui            FAIL  $(grep -E '^\s+[0-9]+ failed|✘' <<<"$out" | head -2 | tr -s ' ')"; fi
    continue
  fi
  if out=$(./test/step$s.sh "$NS" 2>&1); then note "step $(printf %-8s "$s") PASS"
  else failed=$((failed + 1)); note "step $(printf %-8s "$s") FAIL  $(grep '^FAIL' <<<"$out" | head -1)"; fi
done
[[ -n "$no4c" && -z "${3:-}" ]] && note "step 4c       SKIPPED  no GitHub App key (~/.config/ambit/github-app.pem)"

# 6 · roll back to the pinned versions and check the contracts again
trap 'rc=$?; { printf "%s\n" "${results[@]}"; } > "$REPORT"; unlock; on_exit $rc' EXIT
restore >/dev/null
rollback_check
if (( failed == 0 )); then note "verdict       READY: $COMP $VER passed every check"; FINISHED=1
else note "verdict       NOT READY: $failed check(s) failed"; exit 1; fi
