#!/usr/bin/env bash
# CI: fetch what every kind cluster would otherwise download from cr.agentgateway.dev, which failed twice on
# 2026-10-10: the chart dependencies (into charts/ambit/charts/) and the two agentgateway images (saved to the tar
# file given). The workflows cache both, keyed by charts/ambit/Chart.lock, and load them into each cluster, so the
# registry is contacted only when that file changes.
#   hack/thirdparty-cache.sh "$RUNNER_TEMP/thirdparty/agentgateway.tar"
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:?usage: hack/thirdparty-cache.sh <image tar>}

retry() { # 3 tries, 20 s apart: the registry's failures were short
  local n
  for n in 1 2 3; do "$@" && return 0; [[ $n == 3 ]] || { echo "retry $n: $*" >&2; sleep 20; }; done
  return 1
}

retry helm dependency build charts/ambit >/dev/null
v=$(awk '/name: agentgateway$/ { f = 1 } f && $1 == "version:" { print $2; exit }' charts/ambit/Chart.lock)
[[ -n "$v" ]] || { echo "no agentgateway version in charts/ambit/Chart.lock" >&2; exit 1; }
images=("cr.agentgateway.dev/controller:$v" "cr.agentgateway.dev/agentgateway:$v")
for i in "${images[@]}"; do retry docker pull -q "$i"; done
mkdir -p "$(dirname "$out")"
docker save "${images[@]}" -o "$out"
echo "cached ${images[*]} and $(ls charts/ambit/charts | tr '\n' ' ')"
