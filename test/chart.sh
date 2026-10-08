#!/usr/bin/env bash
# Rendering checks for chart rules a cluster test would only find late. No cluster needed.
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml)
check() { uv run --no-project --with pyyaml python -c "import sys,yaml
docs=[d for d in yaml.safe_load_all(sys.stdin) if d]
def one(kind,name): return [x for x in docs if x['kind']==kind and x['metadata']['name']==name][0]
$1" <<<"$out" || { echo "FAIL chart: $2"; exit 1; }; }
check 'd=one("Deployment","ambit-router"); s=d["spec"]; t=s["template"]["spec"]
assert "replicas" not in s
assert t["terminationGracePeriodSeconds"]==35
assert t["containers"][0]["lifecycle"]["preStop"]["sleep"]["seconds"]==5' "router Deployment: no replicas, grace 35 s, preStop sleep 5 s"
check 'r=one("Role","ambit-router")["rules"]
assert all(x.get("resourceNames") and set(x["resourceNames"])<={"ambit-router-config","ambit-router-credentials"} for x in r), r' "router RBAC names its two objects only"
check 'n=one("NetworkPolicy","ambit-router")["spec"]["ingress"]
assert any(f.get("podSelector",{}).get("matchLabels",{}).get("gateway.networking.k8s.io/gateway-name")=="ambit" for i in n for f in i.get("from",[]))' "only the gateway reaches port 9000"
check 's=one("Service","ambit-router")["spec"]["ports"][0]
assert s["port"]==9000 and s["appProtocol"]=="kubernetes.io/h2c"' "router Service is h2c on 9000"
check 'e=one("AgentgatewayParameters","ambit")["spec"]["env"]
assert {"name":"AGENTGATEWAY_MESSAGES_PREFER_COMPLETIONS","value":"true"} in e, e' "gateway converts /v1/messages to Chat Completions, as in v1.5.0"
check 'sa=one("AgentgatewayParameters","ambit")["spec"]["serviceAccount"]["metadata"]["annotations"]
assert sa=={"ambitproject.dev/test":"kind"}, sa' "gateway ServiceAccount carries gateway.serviceAccount.annotations"
none=$(helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml)
! grep -q 'serviceAccount:' <<<"$(sed -n '/kind: AgentgatewayParameters/,/^---/p' <<<"$none")" || { echo "FAIL chart: no serviceAccount overlay without annotations"; exit 1; }
grep -q 'x-ambit-config' <<<"$out" || { echo "FAIL chart: Gateway must strip x-ambit-config"; exit 1; }
grep -q -- '--router-deployment=ambit-router' <<<"$out" || { echo "FAIL chart: controller flag"; exit 1; }
off=$(helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --set routing.router.enabled=false)
! grep -q 'name: ambit-router$' <<<"$off" || { echo "FAIL chart: routing.router.enabled=false renders no ambit-router"; exit 1; }
! grep -q -- '--router-deployment' <<<"$off" || { echo "FAIL chart: no controller flag when off"; exit 1; }
echo "PASS chart"
