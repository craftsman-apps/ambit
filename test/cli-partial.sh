#!/usr/bin/env bash
# Spec 9.2a "partial local creation": kill `ambit local up` after the cluster exists and before the charts are
# Ready; the next local up repairs it and local down deletes it. Then a kind cluster with the same name but
# without the ownership proofs is refused by both commands, and nothing suggests deleting it. One cluster at a
# time: the foreign cluster is created only after local down removed the first one.
# Runs only in CI (CI=true; spec 9.4, ruling S15). Needs bin/ambit built with -tags dev, kind and kubectl.
# CLI_ARCHIVES: image archives to load (the branch image).
set -euo pipefail
[[ "${CI:-}" == true ]] || { echo "test/cli-partial.sh creates kind clusters and runs only in CI (CI=true); on a developer machine run the unit tests" >&2; exit 2; }
cd "$(dirname "$0")/.."
NAME=${CLI_NAME:-ambit} PORT=${CLI_PORT:-8080} HTTPS_PORT=${CLI_HTTPS_PORT:-8443}
W=$(mktemp -d)
export KUBECONFIG=$W/kubeconfig
AMBIT=$PWD/bin/ambit
CTX=kind-$NAME
STATE=${XDG_CONFIG_HOME:-$HOME/.config}/ambit/local/$NAME.json   # os.UserConfigDir() on Linux
OWNED=0   # 1 once the state file and the node label show that local up owns the cluster
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  if [[ $OWNED == 1 ]]; then
    "$AMBIT" local down --name "$NAME" --yes >/dev/null 2>&1 || true
  else
    echo "cleanup: no cluster owned by local up to delete; what is left stays for the CI runner's teardown" >&2
    kind get clusters >&2 || true
    cat "$STATE" >&2 2>/dev/null || echo "no state file $STATE" >&2
  fi
}
trap cleanup EXIT

cat > "$W/dev-values.yaml" <<'YAML'
console: { image: { repository: ambit-console, tag: dev, pullPolicy: Never } }
YAML
UP=(local up --name "$NAME" --port "$PORT" --https-port "$HTTPS_PORT" --chart-dir charts --no-key --dev-values "$W/dev-values.yaml")
for a in ${CLI_ARCHIVES:-}; do UP+=(--dev-load-archive "$a"); done

# 1 · start local up and kill it once the charts are being installed (the namespace exists, pods not Ready yet)
"$AMBIT" "${UP[@]}" > "$W/up1.log" 2>&1 &
PID=$!
for _ in $(seq 1 300); do
  kubectl --context "$CTX" get namespace ambit-system >/dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || { cat "$W/up1.log"; fail "1 local up ended before the charts step"; }
  sleep 1
done
kill -9 "$PID"; wait "$PID" 2>/dev/null || true
grep -q '"status": "creating"' "$STATE" || fail "1 state file $STATE is not 'creating'"
grep -q '"containerID": "' "$STATE" || fail "1 state file $STATE has no container ID"
kubectl --context "$CTX" get nodes -L ambitproject.dev/created-by | grep -q ambit-cli || fail "1 the node has no ownership label"
OWNED=1
pass "1 local up killed during the charts step; state 'creating' with the container ID, and the node label, are there"

# 2 · the next local up repairs it
"$AMBIT" "${UP[@]}" > "$W/up2.log" 2>&1 || { cat "$W/up2.log"; fail "2 repair"; }
grep -q "(exists; repairing)" "$W/up2.log" && grep -q "local/echo answered:" "$W/up2.log" || { cat "$W/up2.log"; fail "2 repair output"; }
grep -q '"status": "ready"' "$STATE" || fail "2 state is not 'ready'"
pass "2 local up repaired the interrupted cluster"
# From here on the script holds the password: never trace it.
{ set +x; } 2>/dev/null
PW=$("$AMBIT" local password --name "$NAME")

# 3 · local down deletes it
"$AMBIT" local down --name "$NAME" --yes > "$W/down.log" 2>&1 || { cat "$W/down.log"; fail "3 local down"; }
OWNED=0
! kind get clusters | grep -qx "$NAME" || fail "3 the cluster still exists"
[[ ! -e "$STATE" ]] || fail "3 the state file is still there"
pass "3 local down deleted the repaired cluster"

# 4 · a kind cluster with the same name that ambit did not create is refused by local up and local down, and survives
kind create cluster --name "$NAME" --kubeconfig "$KUBECONFIG" --wait 60s >/dev/null
for cmd in "local up --name $NAME --no-key --chart-dir charts --port $PORT --https-port $HTTPS_PORT" "local down --name $NAME --yes"; do
  rc=0; "$AMBIT" $cmd > "$W/foreign.log" 2>&1 || rc=$?
  [[ $rc == 1 ]] || { cat "$W/foreign.log"; fail "4 ambit $cmd exited $rc, want 1"; }
  grep -q "a kind cluster named \"$NAME\" exists and was not created by ambit local up" "$W/foreign.log" || { cat "$W/foreign.log"; fail "4 refusal text"; }
  ! grep -qi "kind delete\|delete cluster" "$W/foreign.log" || fail "4 the refusal suggests a delete command"
done
kind get clusters | grep -qx "$NAME" || fail "4 the foreign cluster was deleted"
pass "4 a cluster without the ownership proofs is refused by local up and local down"
# 5 · redaction (spec 9.2a): no captured stream holds the admin password
for f in "$W"/up1.log "$W"/up2.log "$W"/down.log "$W"/foreign.log; do
  ! grep -qF -- "$PW" "$f" || fail "5 the password appears in $(basename "$f")"
done
pass "5 no password in the captured streams"
# The script created this cluster with kind itself, so it removes it the same way (cleanup never deletes it).
kind delete cluster --name "$NAME" --kubeconfig "$KUBECONFIG" >/dev/null
