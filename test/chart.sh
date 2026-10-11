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
check_qs 'e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_LLM_URL","value":"http://llm.localtest.me:8080"} in e and {"name":"AMBIT_MCP_URL","value":"http://mcp.localtest.me:8080/mcp"} in e, e
assert not [x for x in e if x["name"]=="AMBIT_TEST_HOOKS"], "test hooks must be off by default"' "console gets the LLM and MCP URLs for key snippets"
ext=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set console.url=https://console.example.com --set llm.host=llm.example.com --set mcp.host=mcp.example.com --set gateway.tls.enabled=false)
qs=$ext check_qs 'e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_LLM_URL","value":"https://llm.example.com"} in e and {"name":"AMBIT_MCP_URL","value":"https://mcp.example.com/mcp"} in e, e' "snippet URLs take https from console.url even when the gateway does not terminate TLS"
check 'e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_TEST_HOOKS","value":"true"} in e' "kind values enable the test hooks"
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
check_qs "pols={x['metadata']['name']:x['spec'] for x in docs if x['kind']=='ValidatingAdmissionPolicy'}
# No Lease-holder check in admission: the API server's parameter cache can go stale and lock out every key write.
assert set(pols)=={'ambit-keys-writer','ambit-key-lease'}, list(pols)
assert all('paramKind' not in p for p in pols.values()), pols
w=pols['ambit-keys-writer']
assert w['failurePolicy']=='Fail', w
rs=w['matchConstraints']['resourceRules'][0]
assert set(rs['resources'])=={'secrets','configmaps'} and set(rs['operations'])=={'CREATE','UPDATE','DELETE'}, rs
import json
we=json.dumps(w)
for s in ['system:serviceaccount:ambit-system:ambit-console','authentication.kubernetes.io/pod-name','system:serviceaccount:kube-system:namespace-controller','system:serviceaccount:kube-system:generic-garbage-collector']:
  assert s in we, s
assert [v['expression'] for v in w['validations']]==['variables.console || variables.cleanup'], w['validations']
assert w['validations'][0]['message']=='only the Ambit console may write personal API key objects', w
sel=['ambitproject.dev/key','ambitproject.dev/key-owner-record','ambitproject.dev/key-event']
bs={x['metadata']['name']:x['spec'] for x in docs if x['kind']=='ValidatingAdmissionPolicyBinding'}
assert set(bs)=={'ambit-keys-writer-%d'%i for i in range(3)}|{'ambit-key-lease'}, list(bs)
assert all('paramRef' not in b for b in bs.values()), bs
bs.pop('ambit-key-lease')
for n,b in bs.items():
  i=int(n[-1])
  assert b['policyName']=='ambit-keys-writer' and b['validationActions']==['Deny'], b
  assert b['matchResources']['namespaceSelector']=={'matchLabels':{'kubernetes.io/metadata.name':'ambit-system'}}, b
  assert b['matchResources']['objectSelector']=={'matchExpressions':[{'key':sel[i],'operator':'Exists'}]}, b" "admission: console-only writes to key objects (three label bindings), cleanup exempt, no Lease-holder check"
check_qs "p=one('ValidatingAdmissionPolicy','ambit-key-lease')['spec']
assert p['failurePolicy']=='Fail' and 'paramKind' not in p, p
r=p['matchConstraints']['resourceRules'][0]
assert r['apiGroups']==['coordination.k8s.io'] and r['resources']==['leases'] and set(r['operations'])=={'UPDATE','DELETE'} and r['resourceNames']==['ambit-key-writer'], r
e=p['validations'][0]['expression']
for s in ['system:serviceaccount:ambit-system:ambit-console','system:serviceaccount:kube-system:namespace-controller','system:serviceaccount:kube-system:generic-garbage-collector']: assert s in e, s
assert p['validations'][0]['message']=='only the Ambit console may change the key Lease', p
b=one('ValidatingAdmissionPolicyBinding','ambit-key-lease')['spec']
assert b['policyName']=='ambit-key-lease' and b['validationActions']==['Deny'] and 'paramRef' not in b, b
assert b['matchResources']['namespaceSelector']=={'matchLabels':{'kubernetes.io/metadata.name':'ambit-system'}}, b" "admission: only the console changes the key Lease"
check_qs "r=one('Role','ambit-console')['rules']
rule=[x for x in r if x['apiGroups']==[''] and set(x['resources'])=={'secrets','configmaps'} and not x.get('resourceNames')]
assert any(set(x['verbs'])=={'get','list','watch','create','update','delete'} for x in rule), r
l=[x for x in r if x['apiGroups']==['coordination.k8s.io']]
assert len(l)==1 and l[0]['resources']==['leases'] and set(l[0]['verbs'])=={'get','create','update'}, l" "console Role: key objects and Lease (no Lease delete)"
check_qs "d=one('Deployment','ambit-console')['spec']
assert d['strategy']=={'type':'RollingUpdate','rollingUpdate':{'maxSurge':0,'maxUnavailable':1}} and d['replicas']==1, d
e={x['name']:x for x in d['template']['spec']['containers'][0]['env']}
assert e['AMBIT_KEYS_ENABLED']['value']=='true' and e['AMBIT_KEYS_INACTIVITY_DAYS']['value']=='30' and e['AMBIT_KEYS_MAX_DAYS']['value']=='365', e
assert e['AMBIT_POD_NAME']['valueFrom']['fieldRef']['fieldPath']=='metadata.name', e" "console: RollingUpdate maxSurge 0, key settings, pod name"
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set console.replicas=2 >/dev/null 2>&1 || { echo "FAIL chart: console.replicas>1 must be refused"; exit 1; }
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --kube-version 1.29.0 >/dev/null 2>&1 || { echo "FAIL chart: Kubernetes < 1.30 must be refused"; exit 1; }
helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --kube-version v1.30.4-eks-a737599 >/dev/null || { echo "FAIL chart: vendor-suffixed 1.30 must render"; exit 1; }
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set apiKeys.maxDays=60 >/dev/null 2>&1 || { echo "FAIL chart: maxDays must be 30, 90 or 365"; exit 1; }
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set apiKeys.inactivityDays=0 >/dev/null 2>&1 || { echo "FAIL chart: inactivityDays 1-365"; exit 1; }
# NOTES warn when a gateway accepts JWTs without an email (offboarding cannot block them). Client-side dry run: no cluster.
notes() { KUBECONFIG=/dev/null helm install ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --dry-run=client "$@" | sed -n '/^NOTES:/,$p'; }
warn='JWTs without an email are accepted; offboarding cannot block them before they expire'
! grep -qF "$warn" <<<"$(notes)" || { echo "FAIL chart: no requireEmail warning by default"; exit 1; }
for g in llm mcp; do
  grep -qF "$warn" <<<"$(notes --set $g.jwt.requireEmail=false)" || { echo "FAIL chart: NOTES must warn when $g.jwt.requireEmail=false"; exit 1; }
done
echo "PASS chart"
