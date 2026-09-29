#!/usr/bin/env bash
# kind only: make *.localtest.me resolve to the Ambit gateway inside the cluster, so pods
# reach Dex at the same issuer URL the browser uses. Safe to run twice.
set -euo pipefail
# Every *.localtest.me name (console, and the test IdP keycloak) goes to the Ambit gateway.
RULE="rewrite name regex (.+)\\.localtest\\.me ambit.${1:-ambit-system}.svc.cluster.local answer auto"
cm=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if grep -qF "$RULE" <<<"$cm"; then echo "CoreDNS rewrite already present"; exit 0; fi
cm=$(grep -v 'localtest\.me\|localtest.me' <<<"$cm")   # drop an older rule, if any
new=$(R="$RULE" awk '{print} $1=="ready"{match($0,/^[ \t]*/); print substr($0,1,RLENGTH) ENVIRON["R"]}' <<<"$cm")
grep -q "localtest" <<<"$new" || { echo "FAIL: could not find the 'ready' line in the Corefile"; exit 1; }
kubectl -n kube-system create configmap coredns --from-literal=Corefile="$new" --dry-run=client -o yaml | kubectl replace -f - >/dev/null
kubectl -n kube-system rollout restart deployment coredns >/dev/null
kubectl -n kube-system rollout status deployment coredns --timeout=90s >/dev/null
echo "CoreDNS: *.localtest.me -> ambit.${1:-ambit-system}.svc.cluster.local"
