#!/usr/bin/env bash
# Audit-only mode (#81) on kind.
# Step 0 · the access-log policy ambit-access-log moves from the chart to the controller, under the same name:
#   install the previous release (evidence on), upgrade to this chart and measure the gap without an access-log
#   policy; the new controller is not ready before it wrote the policy. Roll back with the documented commands
#   (scale the controller to 0, wait until its pods are gone, delete the policy, helm rollback); without the
#   delete, helm rollback fails. After the rollback the controller is back to the old chart's replica count.
#   Upgrade again. Records reach ClickHouse after each move.
# Reinstalls Ambit: run under the shared kind lock. Needs this checkout's image in the cluster (make image).
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
done_step audit-0
