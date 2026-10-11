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
# Quick start: the gateways accept tokens from Ambit's own Dex (llm/mcp.jwt.dex), fetched in-cluster.
qs=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml)
check_qs() { uv run --no-project --with pyyaml python -c "import sys,yaml
docs=[d for d in yaml.safe_load_all(sys.stdin) if d]
def one(kind,name): return [x for x in docs if x['kind']==kind and x['metadata']['name']==name][0]
$1" <<<"$qs" || { echo "FAIL chart: $2"; exit 1; }; }
check_qs 'for n in ("ambit-llm-auth","ambit-mcp-auth"):
  p=one("AgentgatewayPolicy",n)["spec"]["traffic"]["jwtAuthentication"]["providers"]
  assert p==[{"issuer":"http://console.localtest.me:8080/dex","audiences":["ambit-cli"],"jwks":{"remote":{"url":"http://ambit-dex.ambit-system.svc:5556/dex/keys"}}}], (n,p)' "jwt.dex points both gateways at Dex"
check_qs 'c=yaml.safe_load(one("Secret","ambit-dex-config")["stringData"]["config.yaml"])
assert c["oauth2"]["passwordConnector"]=="local", c["oauth2"]
assert c["staticClients"]==[{"id":"ambit-cli","name":"Ambit CLI","public":True}], c.get("staticClients")' "jwt.dex: Dex has the public ambit-cli client and the password grant"
check_qs 'e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_DEX_URL","value":"http://ambit-dex:5556/dex"} in e' "console reaches Dex in-cluster"
check 'c=yaml.safe_load(one("Secret","ambit-dex-config")["stringData"]["config.yaml"])
assert "passwordConnector" not in c["oauth2"] and "staticClients" not in c, c' "no password grant or ambit-cli without jwt.dex"
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set llm.jwt.issuer=https://idp.example.com >/dev/null 2>&1 || { echo "FAIL chart: jwt.dex with jwt.issuer must be refused"; exit 1; }
nopw=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set bootstrapAdmin.enabled=false)
grep -q 'ambit-cli' <<<"$nopw" && ! grep -q 'passwordConnector' <<<"$nopw" || { echo "FAIL chart: without local accounts, keep ambit-cli but no password grant"; exit 1; }
# Personal API keys (spec 3.1-3.5): both gateways accept keys and JWTs, a Require rule decides, headers are removed.
for g in llm mcp; do
check_qs "p=one('AgentgatewayPolicy','ambit-$g-auth')['spec']['traffic']
assert p['jwtAuthentication']['mode']=='Permissive', p
k=p['apiKeyAuthentication']
assert k['mode']=='Permissive' and k['secretSelector']['matchLabels']=={'ambitproject.dev/key':'true','ambitproject.dev/key-$g':'true'}, k
assert 'x-api-key' in k['location']['expression'], k
a=p['authorization']; assert a['action']=='Require', a
r=one('HTTPRoute','ambit-$g')['spec']['rules'][0]['filters'][0]['requestHeaderModifier']['remove']
assert set(r)=={'X-Ambit-User','x-api-key','authorization'}, r" "$g gateway: keys + JWT, Require, credential headers removed"
done
check_qs "a=one('AgentgatewayPolicy','ambit-access-log')['spec']['frontend']['accessLog']['attributes']['add']
n={x['name']:x['expression'] for x in a}
assert set(['jwt.email','jwt.groups','jwt.sub','ambit.key_id','ambit.identity'])<=set(n), n
assert n['ambit.identity'].endswith(': \"jwt\")'), n['ambit.identity']" "audit records one identity, the key id and how it was decided"
off=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set apiKeys.enabled=false)
! grep -q apiKeyAuthentication <<<"$off" || { echo "FAIL chart: apiKeys.enabled=false must not render apiKeyAuthentication"; exit 1; }
grep -q 'action: Require' <<<"$off" || { echo "FAIL chart: keys off keeps the Require rules"; exit 1; }
noemail=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set llm.jwt.requireEmail=false --set mcp.jwt.requireEmail=false)
! grep -q "jwt.email.trim().matches" <<<"$(sed -n '/name: ambit-llm-auth/,/^---/p' <<<"$noemail")" || { echo "FAIL chart: requireEmail=false must not require an email"; exit 1; }
grep -q -- '--require-email=false' <<<"$noemail" || { echo "FAIL chart: controller flag"; exit 1; }
echo "PASS chart"
