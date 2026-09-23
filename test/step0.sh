#!/usr/bin/env bash
# Step 0 acceptance: GatewayClass accepted, Gateway programmed, host port 8080 reaches the proxy.
set -euo pipefail
NS=${1:-ambit-system}
kubectl wait gatewayclass/agentgateway --for=condition=Accepted --timeout=120s
kubectl -n "$NS" wait gateway/ambit --for=condition=Programmed --timeout=180s
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:8080/ || true)
# No routes exist yet, so the proxy itself must answer 404. 000 means nothing is listening.
[ "$code" = "404" ] || { echo "FAIL: expected 404 from agentgateway on :8080, got $code"; exit 1; }
echo "PASS: step 0"
