#!/usr/bin/env bash
# The files of `ambit init --gitops argocd` (spec D2 3.1.1) on Argo CD on kind: the parent Application syncs the
# dependency CRDs, the charts ambit-crds and ambit (the chart and image of release CLI_PREVIOUS, from ghcr.io, with
# the values from the repository) and the objects directory; the console must become Ready. The configuration
# repository is served from this machine (test/gitops/gitserve.go), so no GitHub repository or App is needed.
# Runs in CI (CI=true), or locally with CLI_INSTALL_LOCAL=yes under lockf /tmp/ambit-kind.lock. Needs Go, git,
# kind, kubectl, helm, docker and script(1).
set -euo pipefail
[[ "${CI:-}" == true || "${CLI_INSTALL_LOCAL:-}" == yes ]] || {
  echo "test/gitops/init-argocd.sh creates a kind cluster: run it in CI, or set CLI_INSTALL_LOCAL=yes under lockf /tmp/ambit-kind.lock" >&2; exit 2; }
cd "$(dirname "$0")/../.."
PREV=${CLI_PREVIOUS:-0.4.0}
NAME=${CLI_KIND_NAME:-ambit-gitops}
[[ $NAME != ambit ]] || { echo "the kind cluster ambit is shared" >&2; exit 2; }
NODE=$NAME-control-plane
W=$(mktemp -d)
GS=""
fail() {
  echo "FAIL: $*" >&2
  kubectl -n argocd get applications -o wide >&2 2>/dev/null || true
  # Why an Application is not Synced: its last operation and the resources that differ.
  kubectl -n argocd get applications -o go-template='{{range .items}}{{.metadata.name}}: {{with .status.operationState}}{{.phase}} {{.message}}{{end}}{{"\n"}}{{range .status.resources}}{{if ne .status "Synced"}}  {{.kind}} {{.name}}: {{.status}} {{with .health}}{{.status}} {{.message}}{{end}}{{"\n"}}{{end}}{{end}}{{end}}' >&2 2>/dev/null || true
  kubectl -n ambit-system get pods >&2 2>/dev/null || true
  # Why a pod is not ready (#363): the state and events of the Argo CD pods (an image that cannot be pulled shows
  # here), and the newest events of both namespaces.
  kubectl -n argocd get pods -o wide >&2 2>/dev/null || true
  kubectl -n argocd describe pods 2>/dev/null | grep -E '^(Name:|Events:|  (Warning|Normal) )|State:|Reason:|Message:|Ready:' >&2 || true
  kubectl -n argocd get events --sort-by=.lastTimestamp 2>/dev/null | tail -30 >&2 || true
  kubectl -n ambit-system get events --sort-by=.lastTimestamp 2>/dev/null | tail -30 >&2 || true
  exit 1
}
pass() { echo "PASS: $*"; }
cleanup() {
  [[ -n $GS ]] && kill "$GS" 2>/dev/null || true
  kind delete cluster --name "$NAME" --kubeconfig "$W/kubeconfig" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The CLI as release PREV would be (its charts are published), and the files it writes.
go build -ldflags "-X github.com/craftsman-apps/ambit/internal/cli.version=$PREV" -o "$W/ambit" ./cmd/ambit
mkdir "$W/repo" && cd "$W/repo"
# Answers, in the order of the questions: console address, LLM host, MCP on?, company sign-in, gateway tokens, TLS
# Secret, audit log?, budgets?, tool protection?, GitOps mode?, repository, branch, directory, parent?, replicas.
printf '%s\n' http://console.localtest.me:8080 "" n "" dex none n n n n acme/ambit-config "" "" y 1 > "$W/answers"
script -qec "$W/ambit init --gitops argocd" /dev/null < "$W/answers" > "$W/init.log" 2>&1 || { cat "$W/init.log"; fail "1 ambit init --gitops argocd"; }
grep -q "Wrote ambit/values.yaml" "$W/init.log" || { cat "$W/init.log"; fail "1 no values file"; }
ls -R ambit argocd argocd-parent.yaml >/dev/null || fail "1 files missing"
pass "1 ambit init --gitops argocd wrote ambit/values.yaml, ambit/crds/, argocd/ and argocd-parent.yaml"
git init -q -b main && git add -A && git -c user.name=test -c user.email=test@example.com commit -qm "ambit init --gitops argocd"
cd - >/dev/null

# The cluster, Argo CD, and the repository served to it from this machine (the kind network's gateway address).
kind create cluster --name "$NAME" --kubeconfig "$W/kubeconfig" --wait 2m >/dev/null
export KUBECONFIG=$W/kubeconfig
[[ $(kubectl config current-context) == "kind-$NAME" ]] || fail "unexpected context $(kubectl config current-context)"
echo "Target: context kind-$NAME"
HOST=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -E '^[0-9.]+$' | head -1)
mkdir "$W/serve" && git clone -q --bare "$W/repo" "$W/serve/ambit-config.git"
go run test/gitops/gitserve.go "$W/serve" "0.0.0.0:18418" & GS=$!
REPO=http://$HOST:18418/ambit-config.git
for _ in $(seq 1 30); do git ls-remote "http://127.0.0.1:18418/ambit-config.git" >/dev/null 2>&1 && break; sleep 1; done
git ls-remote "http://127.0.0.1:18418/ambit-config.git" >/dev/null || fail "2 the git server does not answer"
# The Argo CD images first, each pull tried 4 times with the registry's error printed: inside `helm --wait` a pull that
# fails shows nothing for 5 minutes (#363, argocd-redis Available 0/1 with no pod events in the log).
images=$(make -s argocd-images) || fail "2 could not list the Argo CD images (helm template failed)"
[[ -n $images ]] || fail "2 the list of Argo CD images is empty"
for img in $images; do
  for try in 1 2 3 4; do
    if err=$(timeout 180 docker exec "$NODE" crictl pull "$img" 2>&1); then echo "pulled $img"; break; fi
    echo "pull of $img failed (try $try of 4): $(echo "$err" | tail -1)" >&2
    [[ $try -lt 4 ]] || fail "2 the node cannot pull $img"
    sleep $((try * 10))
  done
done
make -s argocd >/dev/null || fail "2 Argo CD install"
pass "2 Argo CD on kind; the repository is $REPO"

# The repository URL in the Applications is GitHub's; point it at the local server, as a fork would.
sed "s#https://github.com/acme/ambit-config.git#$REPO#" "$W/repo/argocd-parent.yaml" | kubectl apply -n argocd -f - >/dev/null
( cd "$W/repo" && for f in argocd/*.yaml; do sed -i "s#https://github.com/acme/ambit-config.git#$REPO#" "$f"; done &&
  git -c user.name=test -c user.email=test@example.com commit -qam "local repository URL" && git push -q "$W/serve/ambit-config.git" main ) || fail "3 push"
for _ in $(seq 1 180); do
  kubectl -n ambit-system get deploy ambit-console >/dev/null 2>&1 && break
  sleep 5
done
kubectl -n ambit-system rollout status deploy/ambit-console --timeout=10m >/dev/null || fail "3 the console did not become Ready"
# The two CRD Applications are Synced; ambit and ambit-objects exist (controllers change some objects they own, so
# their sync status is not compared here: the console Deployment from the chart is the proof).
for a in ambit-dependency-crds ambit-crds; do
  for _ in $(seq 1 60); do
    [[ $(kubectl -n argocd get application "$a" -o jsonpath='{.status.sync.status}') == Synced ]] && break
    sleep 5
  done
  [[ $(kubectl -n argocd get application "$a" -o jsonpath='{.status.sync.status}') == Synced ]] || fail "3 Application $a is not Synced after 5 minutes"
done
kubectl -n argocd get application ambit ambit-objects >/dev/null || fail "3 Applications ambit and ambit-objects"
pass "3 the parent Application synced the four Applications; the console of Ambit $PREV is Ready"
