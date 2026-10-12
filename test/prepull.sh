#!/usr/bin/env bash
# Pull the third-party images an install and the acceptance steps use onto the kind node, 4 at a time, so the
# install and the steps do not wait for them. CI runs this in the background while the Ambit image builds.
# Images come from the rendered chart and the test manifests, so the list follows version changes.
set -uo pipefail
cd "$(dirname "$0")/.."
NODE=${KIND_NODE:-ambit-control-plane}
list=$(mktemp)
agw=$(sed -n 's/^AGENTGATEWAY_VERSION ?= //p' Makefile)
{
  helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml 2>/dev/null
  cat test/*/*.yaml
} | sed -nE 's/^[[:space:]-]*image:[[:space:]]*"?([^"[:space:]]+)"?.*/\1/p' | grep -v -e '^ambit-' -e ':dev$' -e '{{' | sort -u > "$list"
echo "cr.agentgateway.dev/agentgateway:$agw" >> "$list"
# Images CI already loaded (the agentgateway ones, from hack/thirdparty-cache.sh) are not pulled again.
xargs -P 4 -I{} sh -c 'if docker exec '"$NODE"' crictl inspecti -q "{}" >/dev/null 2>&1; then echo "present {}";
  elif docker exec '"$NODE"' crictl pull "{}" >/dev/null 2>&1; then echo "pulled {}"; else echo "not pulled {}"; fi' < "$list"
