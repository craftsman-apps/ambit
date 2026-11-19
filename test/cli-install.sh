#!/usr/bin/env bash
# ambit install and ambit uninstall on a real cluster (spec D2 section 9, cluster test 1): install into an empty
# cluster, both Helm releases deployed, an AccessRule, uninstall, and the CRDs and the AccessRule still there.
#
# The target is a new kind cluster (CLI_KIND_NAME, default ambit-install), or with CLI_KUBECONFIG an existing
# cluster (k3s in CI) whose context must be CLI_CONTEXT. The script prints the target and refuses any context
# other than the expected one; it never uses the kind cluster `ambit`.
# Runs in CI (CI=true), or on a developer machine with CLI_INSTALL_LOCAL=yes under the kind lock:
#   lockf /tmp/ambit-kind.lock env CLI_INSTALL_LOCAL=yes CLI_ARCHIVES=<console image archive> test/cli-install.sh
# Needs bin/ambit built with -tags dev, kind (for a kind target), kubectl and helm; charts/ambit/charts filled
# (helm dependency build charts/ambit). CLI_ARCHIVES: image archives to load into kind (the branch image).
# CLI_CONSOLE_IMAGE: the console image as repository:tag (default ambit-console:dev, never pulled).
set -euo pipefail
[[ "${CI:-}" == true || "${CLI_INSTALL_LOCAL:-}" == yes ]] || {
  echo "test/cli-install.sh creates a cluster: run it in CI, or set CLI_INSTALL_LOCAL=yes under lockf /tmp/ambit-kind.lock" >&2; exit 2; }
cd "$(dirname "$0")/.."
W=$(mktemp -d)
AMBIT=$PWD/bin/ambit
NS=ambit-system
CREATED=""
pass() { echo "PASS: $*"; }
fail() {
  echo "FAIL: $*" >&2
  if [[ -n ${KUBECONFIG:-} ]]; then  # the cluster is deleted on exit: show its state now
    kubectl get pods -A >&2 || true
    kubectl -n ambit-system describe pods 2>/dev/null | tail -n 80 >&2 || true
  fi
  exit 1
}
cleanup() {
  if [[ -n $CREATED ]]; then kind delete cluster --name "$CREATED" --kubeconfig "$W/kubeconfig" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

if [[ -n ${CLI_KUBECONFIG:-} ]]; then
  KC=$CLI_KUBECONFIG
  WANT=${CLI_CONTEXT:?CLI_CONTEXT names the context of CLI_KUBECONFIG}
else
  NAME=${CLI_KIND_NAME:-ambit-install}
  [[ $NAME != ambit ]] || fail "the kind cluster ambit is shared; use another CLI_KIND_NAME"
  KC=$W/kubeconfig
  kind create cluster --name "$NAME" --kubeconfig "$KC" --wait 2m >/dev/null
  CREATED=$NAME
  for a in ${CLI_ARCHIVES:-}; do kind load image-archive "$a" --name "$NAME" >/dev/null; done
  WANT=kind-$NAME
fi
export KUBECONFIG=$KC   # kubectl and helm below; never the default kubeconfig
CTX=$(kubectl config current-context)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
echo "Target: context $CTX ($SERVER), namespace $NS"
[[ $CTX == "$WANT" && $CTX != kind-ambit ]] || fail "the context is $CTX, not $WANT"
A=("$AMBIT" --kubeconfig "$KC" --context "$CTX")

IMG=${CLI_CONSOLE_IMAGE:-ambit-console:dev}
cat > "$W/image.yaml" <<YAML
console: { image: { repository: ${IMG%:*}, tag: ${IMG##*:}, pullPolicy: Never } }
YAML
VALUES=(-f deploy/quickstart.yaml -f "$W/image.yaml")   # later files win, as in Helm

# 1 · install into an empty cluster: exit 0, or 3 (installed and healthy; the quick-start values are not production)
rc=0; "${A[@]}" install "${VALUES[@]}" --chart-dir charts --yes > "$W/install.log" 2>&1 || rc=$?
cat "$W/install.log"
[[ $rc == 0 || $rc == 3 ]] || fail "1 ambit install exited $rc"
grep -q "Installing Ambit (charts from charts) into context $CTX ($SERVER), namespace $NS" "$W/install.log" || fail "1 no target line"
grep -q '^✓ Workloads ready' "$W/install.log" || fail "1 the workloads were not ready"
for r in ambit-crds ambit; do
  [[ $(helm list -n "$NS" --filter "^$r\$" -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["status"])') == deployed ]] || fail "1 release $r is not deployed"
done
pass "1 ambit install: exit $rc, helm list shows ambit-crds and ambit deployed"

# 2 · a second install refuses and points at upgrade
rc=0; "${A[@]}" install "${VALUES[@]}" --chart-dir charts --yes > "$W/install2.log" 2>&1 || rc=$?
[[ $rc == 1 ]] && grep -q "Ambit is already installed" "$W/install2.log" && grep -q "use ambit upgrade" "$W/install2.log" || { cat "$W/install2.log"; fail "2 second install"; }
pass "2 a second ambit install refuses: use ambit upgrade"

# 3 · an object people wrote
kubectl -n "$NS" apply -f - >/dev/null <<'YAML'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: cli-install-test }
spec: { allSignedIn: true, models: [nothing] }
YAML

# 4 · uninstall: both releases gone; the CRDs and the AccessRule stay
"${A[@]}" uninstall --yes > "$W/uninstall.log" 2>&1 || { cat "$W/uninstall.log"; fail "4 ambit uninstall"; }
cat "$W/uninstall.log"
grep -q "Uninstalled ambit (revision 1) and ambit-crds (revision 1) from context $CTX" "$W/uninstall.log" || fail "4 summary line"
[[ $(helm list -n "$NS" -q | wc -l) -eq 0 ]] || fail "4 releases left: $(helm list -n "$NS" -q)"
kubectl get crd accessrules.ambitproject.dev >/dev/null || fail "4 the CRD accessrules.ambitproject.dev is gone"
kubectl -n "$NS" get accessrule cli-install-test >/dev/null || fail "4 the AccessRule is gone"
pass "4 ambit uninstall: no release left; CRDs and the AccessRule stay"
