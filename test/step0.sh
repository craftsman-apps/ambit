#!/usr/bin/env bash
# Step 0 acceptance: GatewayClass accepted, Gateway programmed, proxy running, host port 8080 reaches it.
set -euo pipefail
NS=${1:-ambit-system}
kubectl wait gatewayclass/agentgateway --for=condition=Accepted --timeout=120s
kubectl -n "$NS" wait gateway/ambit --for=condition=Programmed --timeout=180s
# Programmed means config accepted, not that the proxy pod is up.
kubectl -n "$NS" wait deploy -l gateway.networking.k8s.io/gateway-name=ambit --for=condition=Available --timeout=180s
# No routes exist yet, so the proxy itself must answer 404. 000 means nothing is listening.
code=000
for _ in $(seq 1 15); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8080/ || true)
  [ "$code" = "404" ] && break
  sleep 2
done
[ "$code" = "404" ] || { echo "FAIL: expected 404 from agentgateway on :8080, got $code"; exit 1; }
echo "PASS: step 0"
