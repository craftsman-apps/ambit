#!/usr/bin/env bash
# ambit install, upgrade and uninstall on a real cluster (spec D2 section 9, cluster test 1): install --dry-run,
# install into an empty cluster, a model call through the gateway (CI only: it reads the bootstrap password),
# upgrade -f with a changed value, helm rollback with Helm 3 and Helm 4, upgrade --reuse-values, uninstall (the
# CRDs and an AccessRule stay), then journey J3: the previous release installed with plain Helm, and ambit upgrade.
# HELM3 and HELM4: the two Helm binaries for the rollbacks (default: helm for both).
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

# 0 · install --dry-run: the API server validates, and nothing is created (no release, no Ambit CRD, no namespace)
"${A[@]}" install "${VALUES[@]}" --chart-dir charts --dry-run > "$W/dry.log" 2>&1 || { cat "$W/dry.log"; fail "0 ambit install --dry-run"; }
grep -q '^Would install ambit-crds' "$W/dry.log" && grep -q 'dry run: nothing was changed' "$W/dry.log" || { cat "$W/dry.log"; fail "0 dry-run output"; }
[[ $(helm list -A -q | wc -l) -eq 0 ]] || fail "0 the dry run made a release"
! kubectl get crd accessrules.ambitproject.dev >/dev/null 2>&1 || fail "0 the dry run made a CRD"
! kubectl get crd gateways.gateway.networking.k8s.io >/dev/null 2>&1 || fail "0 the dry run applied a dependency CRD"
pass "0 ambit install --dry-run: validated, nothing created"

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

# 2a · a model call through the gateway: the test model, a rule, a token from Dex, a chat completion (CI only:
# the token needs the bootstrap admin password from its Secret)
if [[ ${CI:-} == true ]]; then
  kubectl -n "$NS" apply -f deploy/local/test-model.yaml >/dev/null
  kubectl -n "$NS" apply -f - >/dev/null <<'YAML'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: everyone-test }
spec: { allSignedIn: true, models: [local-test] }
YAML
  kubectl -n "$NS" wait --for=condition=Ready model/local-test accessrule/everyone-test --timeout=180s >/dev/null || fail "2a model or rule not Ready"
  kubectl -n "$NS" port-forward svc/ambit 18080:8080 >/dev/null 2>&1 & PF=$!
  sleep 3
  { set +x; } 2>/dev/null
  PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
  TOK=$(curl -s --resolve console.localtest.me:18080:127.0.0.1 http://console.localtest.me:18080/dex/token -d grant_type=password \
    -d client_id=ambit-cli -d scope="openid email profile" --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])') || fail "2a no token from Dex"
  ok=""
  for _ in $(seq 1 30); do
    if curl -s --resolve llm.localtest.me:18080:127.0.0.1 http://llm.localtest.me:18080/v1/chat/completions -H "Authorization: Bearer $TOK" \
      -H 'Content-Type: application/json' -d '{"model":"local/echo","messages":[{"role":"user","content":"hello from cli-install"}]}' \
      | grep -q 'You said: hello from cli-install'; then ok=1; break; fi
    sleep 4
  done
  kill "$PF" 2>/dev/null || true
  [[ -n $ok ]] || fail "2a the model call through the gateway did not answer"
  pass "2a a model call through the gateway answered"
fi

# 2b · upgrade -f with a changed value: revision 2, the value stored
echo "evidence: { retention: 4380h }" > "$W/changed.yaml"
rc=0; "${A[@]}" upgrade "${VALUES[@]}" -f "$W/changed.yaml" --chart-dir charts --yes > "$W/upgrade.log" 2>&1 || rc=$?
cat "$W/upgrade.log"
[[ $rc == 0 || $rc == 3 ]] || fail "2b ambit upgrade exited $rc"
grep -q "applies the same version again (a repair)" "$W/upgrade.log" || fail "2b no repair line"
rev() { helm history "$1" -n "$NS" -o json | python3 -c 'import json,sys; h=json.load(sys.stdin)[-1]; print(h["revision"], h["status"])'; }
[[ $(rev ambit) == "2 deployed" ]] || fail "2b ambit is at $(rev ambit), want 2 deployed"
helm get values ambit -n "$NS" -o json | grep -q '"retention":"4380h"' || fail "2b the changed value is not stored"
pass "2b ambit upgrade -f: revision 2 with the changed value"

# 2c · helm rollback with Helm 3, then Helm 4, of releases the CLI wrote
H3=${HELM3:-helm} H4=${HELM4:-helm}
echo "Helm for the rollbacks: $($H3 version --short) and $($H4 version --short)"
$H3 rollback ambit 1 -n "$NS" >/dev/null || fail "2c helm 3 rollback"
[[ $(rev ambit) == "3 deployed" ]] && ! helm get values ambit -n "$NS" -o json | grep -q 4380h || fail "2c after helm 3 rollback: $(rev ambit)"
$H4 rollback ambit 2 -n "$NS" >/dev/null || fail "2c helm 4 rollback"
[[ $(rev ambit) == "4 deployed" ]] && helm get values ambit -n "$NS" -o json | grep -q 4380h || fail "2c after helm 4 rollback: $(rev ambit)"
pass "2c helm rollback works with $($H3 version --short) and $($H4 version --short)"

# 2d · upgrade --reuse-values keeps the stored values
rc=0; "${A[@]}" upgrade --reuse-values --chart-dir charts --yes > "$W/reuse.log" 2>&1 || rc=$?
[[ $rc == 0 || $rc == 3 ]] || { cat "$W/reuse.log"; fail "2d ambit upgrade --reuse-values exited $rc"; }
[[ $(rev ambit) == "5 deployed" ]] && helm get values ambit -n "$NS" -o json | grep -q 4380h || fail "2d after --reuse-values: $(rev ambit)"
pass "2d ambit upgrade --reuse-values: revision 5 with the stored values"

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
grep -q "Uninstalled ambit (revision 5) and ambit-crds (revision 3) from context $CTX" "$W/uninstall.log" || fail "4 summary line"
[[ $(helm list -n "$NS" -q | wc -l) -eq 0 ]] || fail "4 releases left: $(helm list -n "$NS" -q)"
kubectl get crd accessrules.ambitproject.dev >/dev/null || fail "4 the CRD accessrules.ambitproject.dev is gone"
kubectl -n "$NS" get accessrule cli-install-test >/dev/null || fail "4 the AccessRule is gone"
pass "4 ambit uninstall: no release left; CRDs and the AccessRule stay"

# 5 · journey J3: the previous release installed with plain Helm as docs/install.md says, then ambit upgrade
if [[ -n ${CLI_PREVIOUS:-} ]]; then
  R=https://github.com/craftsman-apps/ambit/releases/download/v$CLI_PREVIOUS
  curl -fsSL "$R/crds.yaml" -o "$W/crds.yaml" && curl -fsSL "$R/quickstart.yaml" -o "$W/quickstart-prev.yaml" || fail "5 download v$CLI_PREVIOUS"
  kubectl apply --server-side --force-conflicts -f "$W/crds.yaml" >/dev/null
  helm install ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds --version "$CLI_PREVIOUS" -n "$NS" --create-namespace --wait >/dev/null || fail "5 helm install ambit-crds $CLI_PREVIOUS"
  helm install ambit oci://ghcr.io/craftsman-apps/charts/ambit --version "$CLI_PREVIOUS" -n "$NS" -f "$W/quickstart-prev.yaml" --wait --timeout 10m >/dev/null || fail "5 helm install ambit $CLI_PREVIOUS"
  rc=0; "${A[@]}" upgrade "${VALUES[@]}" --chart-dir charts --yes > "$W/j3.log" 2>&1 || rc=$?
  cat "$W/j3.log"
  [[ $rc == 0 || $rc == 3 ]] || fail "5 ambit upgrade from $CLI_PREVIOUS exited $rc"
  grep -q "Upgrade Ambit $CLI_PREVIOUS → (charts from charts) in context $CTX" "$W/j3.log" || fail "5 no target line"
  [[ $(rev ambit) == "2 deployed" && $(rev ambit-crds) == "2 deployed" ]] || fail "5 after the upgrade: ambit $(rev ambit), ambit-crds $(rev ambit-crds)"
  pass "5 J3: Ambit $CLI_PREVIOUS installed with plain Helm, then ambit upgrade: both releases at revision 2"
fi
