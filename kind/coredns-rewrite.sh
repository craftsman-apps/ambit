#!/usr/bin/env bash
# kind only: make console.localtest.me resolve to the Ambit gateway inside the cluster, so pods
# reach Dex at the same issuer URL the browser uses. Safe to run twice.
set -euo pipefail
HOST=console.localtest.me
TARGET=ambit.${1:-ambit-system}.svc.cluster.local
cm=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if grep -q "$HOST" <<<"$cm"; then echo "CoreDNS rewrite already present"; exit 0; fi
new=$(awk -v r="rewrite name $HOST $TARGET" '{print} $1=="ready"{match($0,/^[ \t]*/); print substr($0,1,RLENGTH) r}' <<<"$cm")
grep -q "rewrite name $HOST" <<<"$new" || { echo "FAIL: could not find the 'ready' line in the Corefile"; exit 1; }
kubectl -n kube-system create configmap coredns --from-literal=Corefile="$new" --dry-run=client -o yaml | kubectl replace -f - >/dev/null
kubectl -n kube-system rollout restart deployment coredns >/dev/null
kubectl -n kube-system rollout status deployment coredns --timeout=90s >/dev/null
echo "CoreDNS: $HOST -> $TARGET"
