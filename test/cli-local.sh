#!/usr/bin/env bash
# The ambit CLI end to end on kind (spec 9.2 job cli-local, and the redaction and restricted-permission gates of
# 9.2a): local up with the test model, check (with --deep: a test call and its audit record), local up again
# with --verbose (repair; it keeps the test model), the password on a terminal only, check as a ServiceAccount
# bound to `view`, and local down. Every captured stream is searched for the password and the tokens. The fake
# OpenRouter key of 9.2a is tested with mocks in TestUpFakeKeyNeverPrinted (ruling S14), so no run here replaces
# the test model.
# Runs only in CI (CI=true): it creates a kind cluster, and a developer machine has room for one extra cluster at
# most (spec 9.4, ruling S15). Needs bin/ambit built with -tags dev, kind, kubectl, curl, python3 and script(1).
# CLI_ARCHIVES: image archives to load (the branch image).
set -euo pipefail
[[ "${CI:-}" == true ]] || { echo "test/cli-local.sh creates a kind cluster and runs only in CI (CI=true); on a developer machine run the unit tests" >&2; exit 2; }
cd "$(dirname "$0")/.."
NAME=${CLI_NAME:-ambit} PORT=${CLI_PORT:-8080} HTTPS_PORT=${CLI_HTTPS_PORT:-8443}
W=$(mktemp -d)
O=$W/out          # every captured stdout, stderr and JSON output; searched for secrets in step 7
mkdir "$O"
export KUBECONFIG=$W/kubeconfig   # never the runner's default kubeconfig
# A "previous" current context, so the test can see that local up gives it back (spec 4.1 step 2, kubeconfig).
kubectl config set-cluster previous --server=https://127.0.0.1:1 >/dev/null
kubectl config set-context previous --cluster=previous >/dev/null
kubectl config use-context previous >/dev/null
AMBIT=$PWD/bin/ambit
CTX=kind-$NAME
OWNED=0           # 1 once local up has established ownership (state file and node label); only then may cleanup delete
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  if [[ $OWNED == 1 ]]; then
    "$AMBIT" local down --name "$NAME" --yes >/dev/null 2>&1 || true
  else
    echo "cleanup: local up did not establish ownership of $NAME; the cluster is left for the CI runner's teardown" >&2
    kind get clusters >&2 || true
    kubectl --context "$CTX" get nodes -L ambitproject.dev/created-by >&2 2>/dev/null || true
  fi
}
trap cleanup EXIT

cat > "$W/dev-values.yaml" <<'YAML'
console: { image: { repository: ambit-console, tag: dev, pullPolicy: Never } }
YAML
UP=(local up --name "$NAME" --port "$PORT" --https-port "$HTTPS_PORT" --chart-dir charts --no-key --dev-values "$W/dev-values.yaml")
for a in ${CLI_ARCHIVES:-}; do UP+=(--dev-load-archive "$a"); done

# 1 · a fresh local up with the test model: every step passes and the test call answers
"$AMBIT" "${UP[@]}" > "$O/up1.log" 2>&1 || { cat "$O/up1.log"; fail "1 local up"; }
OWNED=1
grep -q "local/echo answered: You said: Say hello in five words." "$O/up1.log" || { cat "$O/up1.log"; fail "1 no test answer"; }
[[ $(kubectl config current-context) == previous ]] || fail "1 local up did not restore the current context (now $(kubectl config current-context))"
pass "1 local up created $NAME and the test call answered"

# 2 · check: health passes on the new install
"$AMBIT" check --context "$CTX" > "$O/check.log" 2> "$O/check.err" || { cat "$O/check.log" "$O/check.err"; fail "2 ambit check exited $?"; }
grep -q '^✓ H1 ' "$O/check.log" && grep -q '^✓ H4 ' "$O/check.log" && ! grep -q '^✗' "$O/check.log" || { cat "$O/check.log"; fail "2 check lines"; }
pass "2 ambit check: health passes"

# From here on the script holds secrets (the password and tokens): never trace them.
{ set +x; } 2>/dev/null

# 3 · check --deep --production: one test call per model, and the collector sends that call's record to ClickHouse (P4)
PW=$("$AMBIT" local password --name "$NAME")
TOK=$(curl -s "http://console.localtest.me:$PORT/dex/token" -d grant_type=password -d client_id=ambit-cli \
  -d scope="openid email profile" --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
AMBIT_TOKEN=$TOK "$AMBIT" check --context "$CTX" --deep --production --verbose --output json > "$O/deep.json" 2> "$O/deep.err" || true
python3 - "$O/deep.json" <<'PY' || { cat "$O/deep.err"; fail "3 --deep results"; }
import json, sys
r = {x["id"]: x for x in json.load(open(sys.argv[1]))}
for id in ("H8", "P4"):
    assert r[id]["status"] == "pass", (id, r[id])
PY
pass "3 --deep: the models answer and the audit record leaves the collector"

# 4 · local up again, with --verbose: repair mode, idempotent, still the test model
"$AMBIT" "${UP[@]}" --verbose > "$O/up2.log" 2> "$O/up2.err" || { cat "$O/up2.log" "$O/up2.err"; fail "4 second local up"; }
grep -q "(exists; repairing)" "$O/up2.log" || fail "4 the second run did not repair"
grep -q "local/echo answered:" "$O/up2.log" || fail "4 the repair did not keep the test model"
pass "4 local up again repairs the same cluster and keeps the test model"

# 5 · on a terminal (and only there) the summary shows the password. The terminal log holds the password by
# design, so it is kept outside $O and checked on its own.
script -qec "$AMBIT ${UP[*]}" /dev/null > "$W/tty.log" 2>&1 || { fail "5 local up on a terminal (log not printed: it holds the password)"; }
grep -qF "Password: $PW" "$W/tty.log" || fail "5 no password in the terminal summary"
! grep -qF "Password: $PW" "$O/up1.log" "$O/up2.log" || fail "5 the password is in a summary without a terminal"
pass "5 the password is printed on a terminal only"

# 6 · restricted permissions: a ServiceAccount bound to `view` gets only pass, warn, fail, unknown or n/a
kubectl --context "$CTX" -n ambit-system create serviceaccount ambit-view >/dev/null
kubectl --context "$CTX" create clusterrolebinding ambit-view --clusterrole=view --serviceaccount=ambit-system:ambit-view >/dev/null
SATOK=$(kubectl --context "$CTX" -n ambit-system create token ambit-view)
kubectl --context "$CTX" config view --raw --minify > "$W/view.kubeconfig"
kubectl --kubeconfig "$W/view.kubeconfig" config set-credentials view --token="$SATOK" >/dev/null
kubectl --kubeconfig "$W/view.kubeconfig" config set-context --current --user=view >/dev/null
rc=0; "$AMBIT" check --kubeconfig "$W/view.kubeconfig" --production --verbose --output json > "$O/view.json" 2> "$O/view.err" || rc=$?
[[ $rc -le 1 ]] || { cat "$O/view.err"; fail "6 check as view exited $rc"; }
python3 - "$O/view.json" <<'PY' || fail "6 restricted results"
import json, sys
rs = json.load(open(sys.argv[1]))
assert len(rs) == 18, len(rs)                      # H1-H8 and P1-P10
for r in rs:
    assert r["status"] in ("pass", "warn", "fail", "unknown", "n/a"), r
    if r["status"] == "unknown":
        assert "cannot " in r["message"], r       # names the missing permission
assert any(r["status"] == "unknown" for r in rs)   # view cannot read Secrets, so some checks could not look
PY
pass "6 check with view only: every line ends in a valid status; unknown lines name the permission"

# 7 · redaction (spec 9.2a): no captured stream holds the password, the Dex token or the ServiceAccount token
for f in "$O"/*; do
  for s in "$PW" "$TOK" "$SATOK"; do
    ! grep -qF -- "$s" "$f" || fail "7 a secret appears in $(basename "$f")"
  done
done
pass "7 no password or token in $(find "$O" -type f | wc -l | tr -d ' ') captured streams"

# 8 · local down deletes the cluster
"$AMBIT" local down --name "$NAME" --yes > "$W/down.log" 2>&1 || { cat "$W/down.log"; fail "8 local down"; }
OWNED=0
! kind get clusters | grep -qx "$NAME" || fail "8 the cluster still exists"
pass "8 local down deleted $NAME"
