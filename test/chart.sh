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
check 'sa=one("AgentgatewayParameters","ambit")["spec"]["serviceAccount"]["metadata"]["annotations"]
assert sa=={"ambitproject.dev/test":"kind"}, sa' "gateway ServiceAccount carries gateway.serviceAccount.annotations"
none=$(helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml)
! grep -q 'serviceAccount:' <<<"$(sed -n '/kind: AgentgatewayParameters/,/^---/p' <<<"$none")" || { echo "FAIL chart: no serviceAccount overlay without annotations"; exit 1; }
check 'r=one("AgentgatewayPolicy","ambit-llm-route-headers")["spec"]["traffic"]["transformation"]["request"]["remove"]
assert set(r)=={"x-ambit-route","x-ambit-route-reason","x-ambit-config","x-ambit-classifier-input-tokens","x-ambit-classifier-output-tokens"}, r' "Gateway strips every header ambit-router sets, so a client cannot forge them"
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
check_qs "a=yaml.safe_load(one('ConfigMap','ambit-access-log-base')['data']['spec.yaml'])['frontend']['accessLog']['attributes']['add']
n={x['name']:x['expression'] for x in a}
assert set(['jwt.email','jwt.groups','jwt.sub','ambit.key_id','ambit.identity'])<=set(n), n
assert n['ambit.identity'].endswith(': \"jwt\")'), n['ambit.identity']" "audit records one identity, the key id and how it was decided"
off=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set apiKeys.enabled=false)
! grep -q apiKeyAuthentication <<<"$off" || { echo "FAIL chart: apiKeys.enabled=false must not render apiKeyAuthentication"; exit 1; }
grep -q 'action: Require' <<<"$off" || { echo "FAIL chart: keys off keeps the Require rules"; exit 1; }
noemail=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set llm.jwt.requireEmail=false --set mcp.jwt.requireEmail=false)
# requireEmail=true starts the JWT branch with has(jwt.sub) && has(jwt.email); false checks an email only to refuse the
# service account domain
! grep -q 'has(jwt.sub) && has(jwt.email)' <<<"$(sed -n '/name: ambit-llm-auth/,/^---/p' <<<"$noemail")" || { echo "FAIL chart: requireEmail=false must not require an email"; exit 1; }
grep -q -- '--require-email=false' <<<"$noemail" || { echo "FAIL chart: controller flag"; exit 1; }
check_qs "pols={x['metadata']['name']:x['spec'] for x in docs if x['kind']=='ValidatingAdmissionPolicy'}
# No Lease-holder check in admission: the API server's parameter cache can go stale and lock out every key write.
assert set(pols)=={'ambit-keys-writer','ambit-key-lease','ambit-agent-ack-writer'}, list(pols)
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
sel=['ambitproject.dev/key','ambitproject.dev/key-owner-record','ambitproject.dev/key-event','ambitproject.dev/agent','ambitproject.dev/agent-stops','ambitproject.dev/oauth-rotation']
bs={x['metadata']['name']:x['spec'] for x in docs if x['kind']=='ValidatingAdmissionPolicyBinding'}
assert set(bs)=={'ambit-keys-writer-%d'%i for i in range(6)}|{'ambit-key-lease','ambit-agent-ack-writer','ambit-keys-writer-stops-name'}, list(bs)
assert all('paramRef' not in b for b in bs.values()), bs
bs.pop('ambit-key-lease'); bs.pop('ambit-agent-ack-writer'); bs.pop('ambit-keys-writer-stops-name')
for n,b in bs.items():
  i=int(n[-1])
  assert b['policyName']=='ambit-keys-writer' and b['validationActions']==['Deny'], b
  assert b['matchResources']['namespaceSelector']=={'matchLabels':{'kubernetes.io/metadata.name':'ambit-system'}}, b
  assert b['matchResources']['objectSelector']=={'matchExpressions':[{'key':sel[i],'operator':'Exists'}]}, b" "admission: console-only writes to key objects (six label bindings), cleanup exempt, no Lease-holder check"
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
assert any(set(x['verbs'])=={'get','list','watch','create','update','patch','delete'} for x in rule), r
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
# The collector is ready only when its pipelines run: the exporter created otel_logs and the OTLP receiver
# listens. Without a probe, helm --wait passed while it still started, and the first calls' records were lost.
check_qs 'd=one("Deployment","ambit-otel-collector")["spec"]["template"]["spec"]["containers"][0]
assert d["readinessProbe"]["httpGet"]["port"]=="health" and {"name":"health","containerPort":13133} in d["ports"], d
c=yaml.safe_load(one("ConfigMap","ambit-otel-collector")["data"]["config.yaml"])
assert c["extensions"]["health_check"]["endpoint"]=="0.0.0.0:13133", c.get("extensions")
assert "health_check" in c["service"]["extensions"], c["service"]' "collector is ready only when its pipelines run"
# Audit-only mode (#81): one value, refused unless enforce or audit, and audit only with the audit store.
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set access.mode=Audit >/dev/null 2>&1 || { echo "FAIL chart: access.mode must be enforce or audit"; exit 1; }
! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set access.mode=audit --set evidence.enabled=false >/dev/null 2>&1 || { echo "FAIL chart: access.mode=audit needs evidence.enabled=true"; exit 1; }
# The gate's timeout must be a positive duration: 0 would mean "no gate", not "no limit".
for v in 0s 0 0m0s -5s 2 abc; do
  ! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set-string access.activationTimeout=$v >/dev/null 2>&1 || { echo "FAIL chart: access.activationTimeout=$v must be refused"; exit 1; }
done
for v in -1s abc; do
  ! helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set-string access.activationSettle=$v >/dev/null 2>&1 || { echo "FAIL chart: access.activationSettle=$v must be refused"; exit 1; }
done
helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set-string access.activationTimeout=1m30s --set-string access.activationSettle=0s >/dev/null || { echo "FAIL chart: activationTimeout 1m30s with activationSettle 0s must render"; exit 1; }
check_qs 'c=one("Deployment","ambit-controller")["spec"]["template"]["spec"]["containers"][0]["command"]
assert "--access-mode=enforce" in c and "--activation-timeout=120s" in c and "--activation-settle=3s" in c, c
e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_ACCESS_MODE","value":"enforce"} in e, e' "enforce by default: controller flags and console setting"
qs=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set access.mode=audit) check_qs 'c=one("Deployment","ambit-controller")["spec"]["template"]["spec"]["containers"][0]["command"]
assert "--access-mode=audit" in c, c
e=one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]
assert {"name":"AMBIT_ACCESS_MODE","value":"audit"} in e, e' "access.mode=audit reaches the controller and the console"
qs=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set protection.enabled=true) check_qs 'm=one("AgentgatewayPolicy","ambit-mcp-guard")["spec"]["backend"]["mcp"]["guardrails"]["processors"][0]["methods"]
assert m=={"tools/list":"Response","tools/call":"Full","resources/read":"Response","prompts/get":"Response"}, m' "the guard masks tool results, resource reads and prompts (issue 297)"
# The controller writes the access-log policy (same name); the chart renders only its static part, unchanged.
check_qs 'assert not [x for x in docs if x["kind"]=="AgentgatewayPolicy" and x["metadata"]["name"]=="ambit-access-log"]
s=yaml.safe_load(one("ConfigMap","ambit-access-log-base")["data"]["spec.yaml"])
assert s["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"Gateway","name":"ambit"}], s
assert [x["name"] for x in s["frontend"]["accessLog"]["attributes"]["add"]]==["jwt.email","jwt.groups","jwt.sub","ambit.key_id","ambit.identity","ambit.agent","ambit.agent_mode","ambit.agent_owner_at_issue","ambit.platform_declared","ambit.grant_id","ambit.token_issuer","ambit.router","ambit.route","ambit.routeReason","ambit.classifier.input_tokens","ambit.classifier.output_tokens"], s
assert s["frontend"]["accessLog"]["attributes"]["remove"]==["mcp.error.message"], s
assert s["frontend"]["accessLog"]["otlp"]=={"protocol":"GRPC","backendRef":{"name":"ambit-otel-collector","port":4317}}, s' "access-log base ConfigMap, no chart AgentgatewayPolicy ambit-access-log"
noev=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set evidence.enabled=false)
! grep -q 'name: ambit-access-log-base' <<<"$noev" || { echo "FAIL chart: no access-log base without evidence"; exit 1; }
check_qs 'r=one("Role","ambit-controller")["rules"]
def can(g,res,verbs): return any(g in x["apiGroups"] and res in x["resources"] and set(verbs)<=set(x["verbs"]) and not x.get("resourceNames") for x in r)
assert can("","events",["get","list","watch"]), r
assert can("agentgateway.dev","agentgatewaypolicies/status",["get","list","watch"]) and can("agentgateway.dev","agentgatewaymodels/status",["get","list","watch"]), r
assert not [x for x in r if {"pods/exec","pods/portforward"} & set(x["resources"])], r' "controller reads Events and agentgateway status, no pod exec or port-forward"
audit_warn='access.mode is audit: every signed-in request to a model or MCP tool is allowed'
! grep -qF "$audit_warn" <<<"$(notes)" || { echo "FAIL chart: no audit warning in enforce mode"; exit 1; }
grep -qF "$audit_warn" <<<"$(notes --set access.mode=audit)" || { echo "FAIL chart: NOTES must warn in audit mode"; exit 1; }
# Prompt guard (llm.promptGuard): off renders nothing; each mode renders one AgentgatewayPolicy on the LLM route and the
# access-log field; bad values are refused at render time (values.schema.json and the template).
pg() { helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml "$@"; }
check 'assert not [x for x in docs if x["metadata"]["name"]=="ambit-llm-guard"]
assert not [e for e in one("Deployment","ambit-router")["spec"]["template"]["spec"]["containers"][0]["env"] if e["name"]=="AMBIT_PROMPT_GUARD"]
assert "ambit.promptGuard" not in one("ConfigMap","ambit-access-log-base")["data"]["spec.yaml"]' "prompt guard off by default: no policy, no access-log field"
pgcheck() { local saved=$out; out=$(pg --set llm.promptGuard.enabled=true "${@:3}") || { echo "FAIL chart: render: $2"; exit 1; }; check "$1" "$2"; out=$saved; }
guard='g=one("AgentgatewayPolicy","ambit-llm-guard")["spec"]; r=g["backend"]["ai"]["promptGuard"]["request"]
assert g["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"ambit-llm"}], g
assert "response" not in g["backend"]["ai"]["promptGuard"] and "streaming" not in g["backend"]["ai"]["promptGuard"], g
log=yaml.safe_load(one("ConfigMap","ambit-access-log-base")["data"]["spec.yaml"])["frontend"]["accessLog"]["attributes"]["add"]
assert {"name":"ambit.promptGuard","expression":"guardrails.map(g, g.phase + \":\" + g.guard + \":\" + g.action)"} in log, log
import json
def refusal(m): return {"message":json.dumps({"error":{"code":"prompt_guard","message":m,"type":"permission_error"},"type":"error"},separators=(",",":")),"headers":[{"name":"content-type","value":"application/json"}]}
env=one("Deployment","ambit-router")["spec"]["template"]["spec"]["containers"][0]["env"]
rg=[json.loads(e["value"]) for e in env if e["name"]=="AMBIT_PROMPT_GUARD"]'
pgcheck "$guard"'
assert r==[{"regex":{"action":"Audit","builtins":["CreditCard","Ssn","Email","PhoneNumber"]},"response":refusal("The prompt was refused by the prompt guard of this gateway.")}], r
assert rg==[{"mode":"audit","builtins":["CreditCard","Ssn","Email","PhoneNumber"],"patterns":[],"webhook":{}}], rg' \
  "prompt guard on: audit mode and the four built-ins by default, one policy on ambit-llm, the access-log field, a JSON refusal body, the rules on ambit-router"
pgcheck "$guard"'
assert r==[{"regex":{"action":"Reject","builtins":["Email"],"matches":["(?i)ignore (all )?previous instructions","AKIA[0-9A-Z]{16}","\\d{3}-\\d{4}"]},"response":refusal("No.")}], r
assert rg==[{"mode":"reject","builtins":["Email"],"patterns":["(?i)ignore (all )?previous instructions","AKIA[0-9A-Z]{16}","\\d{3}-\\d{4}"],"webhook":{}}], rg' \
  "prompt guard reject: built-ins, custom patterns (escapes kept) and the message; ambit-router gets the same rules" \
  --set llm.promptGuard.mode=reject --set 'llm.promptGuard.builtins={Email}' --set llm.promptGuard.rejectMessage=No. \
  --set-json 'llm.promptGuard.patterns=["(?i)ignore (all )?previous instructions","AKIA[0-9A-Z]{16}","\\d{3}-\\d{4}"]'
pgcheck "$guard"'
assert r[0]["regex"]["action"]=="Mask" and r[0]["regex"]["matches"]==["secret-[0-9]+"] and "builtins" not in r[0]["regex"], r' \
  "prompt guard mask: patterns only" --set llm.promptGuard.mode=mask --set-json 'llm.promptGuard.builtins=[]' --set-json 'llm.promptGuard.patterns=["secret-[0-9]+"]'
pgcheck "$guard"'
assert r[1]=={"webhook":{"action":"Reject","backendRef":{"name":"guard-hook","namespace":"guards","port":9000},"failureMode":"FailOpen"}}, r
assert rg[0]["webhook"]=={"url":"http://guard-hook.guards.svc:9000/request","failureMode":"FailOpen"}, rg' \
  "prompt guard webhook after the patterns, Reject in mask mode, namespace, port and failure mode; ambit-router calls the same webhook" \
  --set llm.promptGuard.mode=mask --set llm.promptGuard.webhook.service=guard-hook --set llm.promptGuard.webhook.namespace=guards \
  --set llm.promptGuard.webhook.port=9000 --set llm.promptGuard.webhook.failureMode=FailOpen
pgcheck "$guard"'
assert r==[{"webhook":{"action":"Audit","backendRef":{"name":"guard-hook","port":8000},"failureMode":"FailClosed"}}], r
assert rg[0]["webhook"]["url"]=="http://guard-hook.ambit-system.svc:8000/request", rg' \
  "prompt guard webhook only, audit mode: action Audit, the release namespace" --set-json 'llm.promptGuard.builtins=[]' --set llm.promptGuard.webhook.service=guard-hook
long=$(printf 'a%.0s' $(seq 1 1025))
for bad in "--set llm.promptGuard.mode=block" "--set-json llm.promptGuard.builtins=[\"Iban\"]" "--set llm.promptGuard.patterns[0]=$long" \
    "--set llm.promptGuard.pattern=x" "--set llm.promptGuard.webhook.failureMode=Open" "--set llm.promptGuard.webhook.port=0" "--set llm.promptGuard.webhook.service=Guard_Hook" \
    "--set-json llm.promptGuard.builtins=[]" "--set llm.promptGuard.patterns[0]=(?=lookahead)" "--set llm.promptGuard.patterns[0]=a(b"; do
  # shellcheck disable=SC2086
  ! pg --set llm.promptGuard.enabled=true $bad >/dev/null 2>&1 || { echo "FAIL chart: prompt guard values must be refused: $bad"; exit 1; }
done
echo "PASS prompt guard: bad mode, built-in, long or invalid pattern, unknown key, failure mode, port, Service name, and nothing to check are refused"
# Injection guard (protection.injectionGuard, #82): off by default; the guard gets the whole value in AMBIT_INJECTION_GUARD.
ig() { helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set protection.enabled=true "$@"; }
! grep -q AMBIT_INJECTION_GUARD <<<"$(ig)" || { echo "FAIL chart: the injection guard must be off by default"; exit 1; }
! grep -q AMBIT_INJECTION_GUARD <<<"$(ig --set protection.injectionGuard.enabled=false --set protection.injectionGuard.mode=reject)" || { echo "FAIL chart: enabled=false renders the injection guard"; exit 1; }
igv=$(ig --set protection.injectionGuard.enabled=true --set protection.injectionGuard.mode=mask --set 'protection.injectionGuard.disabledBuiltins[0]=role-prefix' \
  --set 'protection.injectionGuard.patterns[0]=(?i)secret handshake' --set protection.injectionGuard.webhook.service=inj-hook --set protection.injectionGuard.webhook.namespace=guards \
  --set protection.injectionGuard.webhook.port=9000 --set protection.injectionGuard.webhook.timeout=500ms --set protection.injectionGuard.webhook.failureMode=FailOpen)
IGV="$igv" uv run --no-project --with pyyaml python -c "import os,json,yaml
docs=[d for d in yaml.safe_load_all(os.environ['IGV']) if d]
d=[x for x in docs if x['kind']=='Deployment' and x['metadata']['name']=='ambit-guard'][0]
v=[e['value'] for e in d['spec']['template']['spec']['containers'][0]['env'] if e['name']=='AMBIT_INJECTION_GUARD'][0]
assert json.loads(v)=={'mode':'mask','builtins':True,'disabledBuiltins':['role-prefix'],'patterns':['(?i)secret handshake'],
  'webhook':{'url':'http://inj-hook.guards.svc:9000/request','timeout':'500ms','failureMode':'FailOpen'}}, v" || { echo "FAIL chart: the guard's AMBIT_INJECTION_GUARD value"; exit 1; }
igd=$(ig --set protection.injectionGuard.enabled=true)
grep -q 'AMBIT_INJECTION_GUARD' <<<"$igd" && grep -q 'mode.*audit' <<<"$igd" && grep -q 'builtins.*true' <<<"$igd" || { echo "FAIL chart: injectionGuard.enabled=true must give audit mode with the built-in rules"; exit 1; }
longp=$(printf 'a%.0s' $(seq 1 1025))
# every built-in rule switched off, and nothing else to check: refused (the names come from the schema)
allrules=$(python3 -c "import json;print(' '.join('--set protection.injectionGuard.disabledBuiltins[%d]=%s'%(i,n) for i,n in enumerate(json.load(open('charts/ambit/values.schema.json'))['properties']['protection']['properties']['injectionGuard']['properties']['disabledBuiltins']['items']['enum'])))")
for bad in "--set protection.injectionGuard.mode=block" "--set protection.injectionGuard.disabledBuiltins[0]=no-such-rule" "--set protection.injectionGuard.patterns[0]=$longp" \
    "--set protection.injectionGuard.patterns[0]=(?<=a)b" "--set protection.injectionGuard.patterns[0]=a(b" "--set protection.injectionGuard.builtins=false" \
    "--set protection.injectionGuard.webhook.timeout=soon" "--set protection.injectionGuard.webhook.failureMode=Open" "--set protection.injectionGuard.webhook.port=0" \
    "--set protection.injectionGuard.webhook.service=Guard_Hook" "--set protection.injectionGuard.rules=x" "--set protection.injectionGuard.webhook.timeout=0s" \
    "--set protection.injectionGuard.webhook.timeout=0.0ms" "--set protection.injectionGuard.patterns[0]=^" "--set protection.injectionGuard.patterns[0]=a*" "$allrules"; do
  # shellcheck disable=SC2086
  ! ig --set protection.injectionGuard.enabled=true $bad >/dev/null 2>&1 || { echo "FAIL chart: injection guard values must be refused: $bad"; exit 1; }
done
echo "PASS injection guard: off by default; the value reaches the guard; bad mode, rule name, long or invalid pattern, nothing to check, webhook timeout, failure mode, port and Service name are refused"
# helm upgrade --reuse-values from 0.1.0 (#81): the old release's values replace the chart's defaults, so every key
# added since 0.1.0 is missing. Render every template with the chart's own values.yaml swapped for 0.1.0's.
old=$(mktemp -d); trap 'rm -rf "$old"' EXIT
cp -R charts/ambit "$old/ambit"
git show v0.1.0:charts/ambit/values.yaml >"$old/ambit/values.yaml"
reuse=$(helm template ambit "$old/ambit" -n ambit-system -f deploy/quickstart.yaml 2>&1) || { echo "FAIL chart: render with the values of 0.1.0 only: $reuse"; exit 1; }
grep -q -- '--users-require-verified-email=true' <<<"$reuse" || { echo "FAIL chart: values of 0.1.0 only must keep users on verified emails"; exit 1; }
grep -q -- '--access-mode=enforce' <<<"$reuse" && grep -q -- '--activation-timeout=120s' <<<"$reuse" && grep -q -- '--activation-settle=3s' <<<"$reuse" || { echo "FAIL chart: values of 0.1.0 only must give enforce and the default timings"; exit 1; }
! grep -q 'access.mode is audit' <<<"$(helm template ambit "$old/ambit" -n ambit-system -f deploy/quickstart.yaml --show-only templates/NOTES.txt 2>&1)" || { echo "FAIL chart: no audit warning with the values of 0.1.0 only"; exit 1; }
igold=$(helm template ambit "$old/ambit" -n ambit-system -f deploy/quickstart.yaml --set protection.enabled=true --set protection.injectionGuard.enabled=true 2>&1) || { echo "FAIL chart: injectionGuard.enabled=true with the values of 0.1.0 only: $igold"; exit 1; }
grep -q 'AMBIT_INJECTION_GUARD' <<<"$igold" && grep -q 'mode.*audit' <<<"$igold" || { echo "FAIL chart: the values of 0.1.0 only plus injectionGuard.enabled must give audit mode with the built-in rules"; exit 1; }
# The public front door (frontDoor.*): off by default, and when on, HTTPS only, one host, one route, keys only.
fdv=(-f deploy/quickstart.yaml -f test/frontdoor/values.yaml)
fdt() { helm template ambit charts/ambit -n ambit-system "${fdv[@]}" "$@"; }
dflt=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml --set limits.enabled=true)
uv run --no-project --with pyyaml python -c "import sys,yaml
d=[x for x in yaml.safe_load_all(sys.stdin) if x and x['kind']=='NetworkPolicy' and x['metadata']['name']=='ambit-ratelimit'][0]
r=d['spec']['ingress']
assert r==[{'from':[{'podSelector':{'matchLabels':{'gateway.networking.k8s.io/gateway-name':'ambit'}}}],'ports':[{'port':8081}]}], r  # no port 8080 rule without the front door, none open to all pods" <<<"$dflt" || { echo "FAIL chart: the ratelimit network policy opens only port 8081, to the gateway pods, without the front door"; exit 1; }
! grep -q '/json: the public front door' <<<"$dflt" || { echo "FAIL chart: the ratelimit Service publishes port 8080 only with the front door"; exit 1; }
! grep -q 'ambit-public' <<<"$dflt" || { echo "FAIL chart: nothing of the front door is rendered by default"; exit 1; }
! grep -q 'ambit.ingress' <<<"$dflt" || { echo "FAIL chart: no ambit.ingress attribute without the front door"; exit 1; }
! grep -q -- '--public-gateway' <<<"$dflt" || { echo "FAIL chart: no controller flag without the front door"; exit 1; }
fd=$(fdt) || { echo "FAIL chart: the front door values must render"; exit 1; }
check_fd() { uv run --no-project --with pyyaml python -c "import sys,yaml,json
docs=[d for d in yaml.safe_load_all(sys.stdin) if d]
def one(kind,name): return [x for x in docs if x['kind']==kind and x['metadata']['name']==name][0]
$1" <<<"$fd" || { echo "FAIL chart: $2"; exit 1; }; }
check_fd 'g=one("Gateway","ambit-public")["spec"]
l=g["listeners"]
assert len(l)==1 and l[0]["protocol"]=="HTTPS" and l[0]["tls"]["mode"]=="Terminate" and l[0]["hostname"]=="mcp-public.localtest.me", l
assert l[0]["tls"]["certificateRefs"]==[{"name":"ambit-public-tls"}], l
assert l[0]["allowedRoutes"]=={"namespaces":{"from":"Same"},"kinds":[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute"}]}, l
assert g["infrastructure"]["parametersRef"]["name"]=="ambit-public", g' "public Gateway: one HTTPS listener bound to the host, same-namespace HTTPRoutes only, no HTTP listener"
check_fd 'on=[x["metadata"]["name"] for x in docs if x["kind"] in ("HTTPRoute","GRPCRoute","TLSRoute","TCPRoute","UDPRoute") and any(p["name"]=="ambit-public" for p in x["spec"].get("parentRefs",[]))]
assert on==["ambit-mcp-public"], on
r=one("HTTPRoute","ambit-mcp-public")["spec"]
assert r["hostnames"]==["mcp-public.localtest.me"] and len(r["rules"])==1, r
assert r["rules"][0]["matches"]==[{"path":{"type":"Exact","value":"/mcp"}}], r
assert r["rules"][0]["backendRefs"]==[{"group":"agentgateway.dev","kind":"AgentgatewayBackend","name":"ambit-mcp-public"}], r
assert set(r["rules"][0]["filters"][0]["requestHeaderModifier"]["remove"])=={"X-Ambit-User","x-api-key","authorization"}, r
for x in docs:
  if x["kind"]=="HTTPRoute" and x["metadata"]["name"]!="ambit-mcp-public":
    assert all(p["name"]!="ambit-public" for p in x["spec"].get("parentRefs",[])), x["metadata"]["name"]
    assert "mcp-public.localtest.me" not in x["spec"].get("hostnames",[]), x["metadata"]["name"]' "only the route ambit-mcp-public is attached to ambit-public: exact /mcp on the public host; no private route names it"
check_fd 'p=one("AgentgatewayPolicy","ambit-mcp-public-auth")["spec"]
t=p["traffic"]
assert p["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"ambit-mcp-public"}], p
assert "jwtAuthentication" not in t, "no token is accepted on the public route"
assert t["apiKeyAuthentication"]["secretSelector"]["matchLabels"]=={"ambitproject.dev/key":"true","ambitproject.dev/key-mcp":"true"}, t
e=t["authorization"]["policy"]["matchExpressions"]
assert t["authorization"]["action"]=="Require" and len(e)==2 and "sourceRanges" in e[0] and "cidr(r).containsIP(source.address)" in e[0], e
assert "has(apiKey.keyId)" in e[1] and "Bearer ambit_" in e[1] and "timestamp(apiKey.until)" in e[1], e' "public route: keys only (no JWT), a valid unexpired MCP key is required"
check_fd 'p=one("AgentgatewayPolicy","ambit-public-limits")["spec"]
assert p["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"Gateway","name":"ambit-public"}], p
g=p["traffic"]["rateLimit"]["global"]
assert g["domain"]=="ambit-public" and g["backendRef"]=={"name":"ambit-ratelimit","port":8081} and g["failureMode"]=="FailOpen", g
assert [d["entries"][0]["name"] for d in g["descriptors"]]==["fd_credential"], g  # the source limit is counted before authentication (below)
assert "x-forwarded" not in json.dumps(g).lower(), g
s=one("AgentgatewayPolicy","ambit-public-source-limit")["spec"]
assert s["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"Gateway","name":"ambit-public"}], s
e=s["traffic"]["extAuth"]
assert s["traffic"]["phase"]=="PreRouting" and set(s["traffic"])=={"phase","extAuth"}, s
assert e["backendRef"]=={"name":"ambit-ratelimit","port":8080} and e["failureMode"]=="FailOpen", e
assert e["http"]["path"]=="\"/json\"" and "string(source.address)" in e["http"]["body"] and "fd_source" in e["http"]["body"] and "\"domain\": \"ambit-public\"" in e["http"]["body"], e
assert e["http"]["allowedRequestHeaders"]==["user-agent"], e  # never the Authorization header: it holds the key of the caller
assert "x-forwarded" not in json.dumps(e).lower(), e
svc=[p["port"] for p in one("Service","ambit-ratelimit")["spec"]["ports"]]
assert svc==[8081,8080], svc  # port 8080 (/json) is published only with the front door
assert p["traffic"]["rateLimit"]["local"]==[{"requests":200,"unit":"Seconds"}], p
c=yaml.safe_load(one("ConfigMap","ambit-ratelimit-front-door")["data"]["front-door.yaml"])
assert c=={"domain":"ambit-public","descriptors":[{"key":"fd_credential","rate_limit":{"unit":"minute","requests_per_unit":600}},{"key":"fd_bucket","value":"mcp","descriptors":[{"key":"fd_source","rate_limit":{"unit":"minute","requests_per_unit":120}}]}]}, c
assert e["http"]["body"].count("\"fd_bucket\", \"value\": \"mcp\"}")==1 and "request." not in e["http"]["body"], e  # without OAuth every request is in the mcp bucket
v=one("Deployment","ambit-ratelimit")["spec"]["template"]["spec"]["volumes"][0]
assert [s["configMap"]["name"] for s in v["projected"]["sources"]]==["ambit-ratelimit-config","ambit-ratelimit-front-door"], v
ing=one("NetworkPolicy","ambit-ratelimit")["spec"]["ingress"]
n=ing[0]["from"]
assert {"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit-public"}}} in n, n
# port 8080 (/json, no authentication) is for the public gateway pods only, never for every pod (#417)
assert ing[0]["from"]==[{"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit"}}},{"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit-public"}}}], ing  # port 8081: both gateways
assert ing[0]["ports"]==[{"port":8081}] and ing[1]=={"from":[{"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit-public"}}}],"ports":[{"port":8080}]} and len(ing)==2, ing
assert all("from" in r for r in ing), ing' "limits: per key (after authentication) and per source address (before authentication, every request) shared through the ratelimit service, plus a per-pod limit; the ratelimit service admits the public gateway pods"
check_fd 'v=one("ValidatingAdmissionPolicy","ambit-public-routes")["spec"]
assert v["failurePolicy"]=="Fail" and set(v["matchConstraints"]["resourceRules"][0]["resources"])=={"httproutes","grpcroutes","tlsroutes","tcproutes","udproutes"}, v
e=v["validations"][0]["expression"]
for part in ["ambit-mcp-public","mcp-public.localtest.me","size(object.spec.rules) == 1","path.type == \"Exact\"","path.value == \"/mcp\"","backendRefs[0].name == \"ambit-mcp-public\"","size(object.spec.parentRefs) == 1"]: assert part in e, (part, e)
b=one("ValidatingAdmissionPolicyBinding","ambit-public-routes")["spec"]
assert b["policyName"]=="ambit-public-routes" and b["validationActions"]==["Deny"], b' "admission policy: no other route may name the public Gateway"
check_fd 'a=one("AgentgatewayParameters","ambit-public")["spec"]["service"]["spec"]
assert a["type"]=="ClusterIP" and "externalTrafficPolicy" not in a, a
c=one("Deployment","ambit-controller")["spec"]["template"]["spec"]["containers"][0]["command"]
assert "--public-gateway=ambit-public" in c, c
s=yaml.safe_load(one("ConfigMap","ambit-access-log-base")["data"]["spec.yaml"])["frontend"]["accessLog"]["attributes"]["add"]
assert {"name":"ambit.ingress","expression":"\"private\""} in s, s
# Nothing rendered by the chart is a load balancer except through the gateways Parameters; no Service names the public gateway.
assert all(x["spec"].get("type","ClusterIP")=="ClusterIP" for x in docs if x["kind"]=="Service"), [x["metadata"]["name"] for x in docs if x["kind"]=="Service" and x["spec"].get("type","ClusterIP")!="ClusterIP"]
# The only policies aimed at the public Gateway or its route are the chart'"'"'s three; the controller adds its access-log and offboarding policies.
t=sorted(x["metadata"]["name"] for x in docs if x["kind"]=="AgentgatewayPolicy" and any(r["kind"] in ("Gateway","HTTPRoute") and r["name"] in ("ambit-public","ambit-mcp-public") for r in x["spec"].get("targetRefs",[])))
assert t==["ambit-mcp-public-auth","ambit-public-limits","ambit-public-source-limit"], t' "public gateway: ClusterIP in the test values, controller flag, ambit.ingress in the access-log base, only three chart policies target it"
check_fd 'g=one("AgentgatewayPolicy","ambit-mcp-guard")["spec"]["targetRefs"]
assert [t["name"] for t in g]==["ambit-mcp","ambit-mcp-public"], g
n=one("NetworkPolicy","ambit-mcp-servers-front-door")["spec"]
assert n["podSelector"]["matchLabels"]=={"toolhive":"true","ambitproject.dev/front-door":"true"}, n
assert n["ingress"]==[{"from":[{"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit-public"}}}],"ports":[{"port":8080}]}], n
base=one("NetworkPolicy","ambit-mcp-servers")["spec"]["ingress"]
assert not any("ambit-public" in json.dumps(i) for i in base), base
b=one("NetworkPolicy","ambit-console-broker")["spec"]["ingress"]
assert not any("ambit-public" in json.dumps(i) for i in b), b
gd=one("NetworkPolicy","ambit-guard")["spec"]["ingress"][0]["from"]
assert {"podSelector":{"matchLabels":{"gateway.networking.k8s.io/gateway-name":"ambit-public"}}} in gd, gd' "public gateway: the guard covers the public backend; only front-door server pods and the guard admit the public gateway pods; the token broker does not"
# The load balancer defaults of the real thing (type LoadBalancer): the client address is kept and the source ranges are passed on.
lb=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml -f test/frontdoor/values.yaml --set frontDoor.service.type=LoadBalancer --set 'frontDoor.service.loadBalancerSourceRanges={203.0.113.0/24}' --set 'frontDoor.service.annotations.service\.beta\.kubernetes\.io/aws-load-balancer-type=nlb')
fd=$lb check_fd 'a=one("AgentgatewayParameters","ambit-public")["spec"]["service"]
assert a["spec"]["type"]=="LoadBalancer" and a["spec"]["externalTrafficPolicy"]=="Local" and a["spec"]["loadBalancerSourceRanges"]==["203.0.113.0/24"], a
assert a["metadata"]["annotations"]=={"service.beta.kubernetes.io/aws-load-balancer-type":"nlb"}, a' "LoadBalancer: externalTrafficPolicy Local, source ranges and annotations reach the Service"
pp=$(helm template ambit charts/ambit -n ambit-system -f deploy/quickstart.yaml -f test/frontdoor/values.yaml --set frontDoor.clientAddress=proxyProtocol)
fd=$pp check_fd 'p=one("AgentgatewayPolicy","ambit-public-proxy-protocol")["spec"]
assert p["frontend"]["proxyProtocol"]=={"mode":"Strict","version":"All"}, p
e=one("AgentgatewayPolicy","ambit-public-source-limit")["spec"]["traffic"]["extAuth"]["http"]
assert "string(source.address)" in e["body"], e' "clientAddress proxyProtocol: the Gateway requires the PROXY protocol"
# Refusals: each message names the value.
refuse() { local why=$1; shift; ! ${RT:-fdt} "$@" >/dev/null 2>"$W2" || { echo "FAIL chart: $why must be refused"; exit 1; }; grep -q "$MSG" "$W2" || { echo "FAIL chart: $why: wrong message: $(head -c 300 "$W2")"; exit 1; }; }
W2=$(mktemp)
MSG='frontDoor.host' refuse "no host" --set frontDoor.host=
MSG='certificateSecret' refuse "no certificate" --set frontDoor.tls.certificateSecret=
MSG='also a private host name' refuse "the console's host name" --set frontDoor.host=console.localtest.me
MSG='also a private host name' refuse "the MCP host name" --set frontDoor.host=mcp.localtest.me
MSG='limits.enabled' refuse "no shared limits" --set limits.enabled=false
MSG='protection.enabled' refuse "no guard" --set protection.enabled=false
MSG='evidence.enabled' refuse "no audit" --set evidence.enabled=false
MSG='apiKeys.enabled' refuse "no API keys" --set apiKeys.enabled=false
MSG='mcp.enabled' refuse "no MCP" --set mcp.enabled=false
MSG='clientAddress' refuse "trusting X-Forwarded-For" --set frontDoor.clientAddress=xForwardedFor
MSG='NodePort' refuse "the PROXY protocol on a node port" --set frontDoor.clientAddress=proxyProtocol --set frontDoor.service.type=NodePort
MSG='failureMode' refuse "a bad failure mode" --set frontDoor.rateLimit.failureMode=Maybe
MSG='perSourcePerMinute' refuse "a zero limit" --set frontDoor.rateLimit.perSourcePerMinute=0
# OAuth sign-in (oauth.*): Hydra and its network. Off by default; when on, the routes are exact, no Secret is rendered,
# and every refusal names its value.
oav=("${fdv[@]}" -f test/oauth/values.yaml)
oat() { helm template ambit charts/ambit -n ambit-system "${oav[@]}" "$@"; }
RT=oat  # refuse() below renders with the OAuth values
uv run --no-project --with pyyaml python -c "import sys,yaml
docs=[d for d in yaml.safe_load_all(sys.stdin) if d]
bad=[(d['kind'],d['metadata']['name']) for d in docs if d['kind']!='ValidatingAdmissionPolicy' and any(x in d['metadata']['name'] for x in ('ambit-hydra','ambit-auth'))]
g=[l['name'] for d in docs if d['kind']=='Gateway' and d['metadata']['name']=='ambit-public' for l in d['spec']['listeners']]
assert not bad and g==['https'], (bad,g)" <<<"$fd" || { echo "FAIL chart: nothing of OAuth is rendered by default"; exit 1; }
oa=$(oat) || { echo "FAIL chart: the OAuth values must render"; exit 1; }
check_oa() { uv run --no-project --with pyyaml python -c "import sys,yaml,json
docs=[d for d in yaml.safe_load_all(sys.stdin) if d]
def one(kind,name): return [x for x in docs if x['kind']==kind and x['metadata']['name']==name][0]
$1" <<<"$oa" || { echo "FAIL chart: $2"; exit 1; }; }
check_oa 'l=one("Gateway","ambit-public")["spec"]["listeners"]
assert [(x["name"],x["hostname"],x["protocol"]) for x in l]==[("https","mcp-public.localtest.me","HTTPS"),("https-auth","auth.localtest.me","HTTPS")], l
assert all(x["tls"]["mode"]=="Terminate" and x["tls"]["certificateRefs"]==[{"name":"ambit-public-tls"}] for x in l), l' "public Gateway: a second HTTPS listener for the sign-in host, same certificate, no HTTP listener"
check_oa 'on=sorted(x["metadata"]["name"] for x in docs if x["kind"] in ("HTTPRoute","GRPCRoute","TLSRoute","TCPRoute","UDPRoute") and any(p["name"]=="ambit-public" for p in x["spec"].get("parentRefs",[])))
assert on==["ambit-auth-app","ambit-auth-hydra","ambit-mcp-public"], on
r=one("HTTPRoute","ambit-auth-hydra")["spec"]
assert r["parentRefs"]==[{"name":"ambit-public","sectionName":"https-auth"}] and r["hostnames"]==["auth.localtest.me"], r
assert len(r["rules"])==1, r
m=sorted((x["method"],x["path"]["type"],x["path"]["value"]) for x in r["rules"][0]["matches"])
assert m==[("GET","Exact","/.well-known/jwks.json"),("POST","Exact","/oauth2/revoke"),("POST","Exact","/oauth2/token")], m
assert r["rules"][0]["backendRefs"]==[{"name":"ambit-hydra-public","port":4444}], r
# Hydra'"'"'s registration, discovery, userinfo, admin and health paths have no route.
backends=[b["name"] for x in docs if x["kind"]=="HTTPRoute" for ru in x["spec"]["rules"] for b in ru.get("backendRefs",[])]
assert "ambit-hydra-admin" not in backends, backends' "exact routes to Hydra: token, revoke and JWKS only; the admin Service has no route"
check_oa 'v=one("ValidatingAdmissionPolicy","ambit-public-routes")["spec"]["validations"][0]["expression"]
assert "ambit-auth-hydra" in v and "/oauth2/token" in v and "ambit-hydra-public" in v, v' "the route policy admits the Hydra route as the chart writes it, and no other"
check_oa 's=one("StatefulSet","ambit-hydra")["spec"]; p=s["template"]["spec"]
assert s["podManagementPolicy"]=="OrderedReady" and s["replicas"]==2, s
assert [c["name"] for c in p["initContainers"]]==["check-database","migrate"] and [c["name"] for c in p["containers"]]==["hydra","hydra-keys"]
for c in p["initContainers"]+p["containers"]:
  assert c["securityContext"]=={"allowPrivilegeEscalation":False,"readOnlyRootFilesystem":True,"capabilities":{"drop":["ALL"]}}, c["name"]
  if "hydra" in c["image"]: assert "@sha256:" in c["image"], c["image"]
assert p["securityContext"]["runAsNonRoot"] is True and p["automountServiceAccountToken"] is False, p["securityContext"]
# The DSN and the system secret come from Secrets only, never as a value.
for c in p["initContainers"]+p["containers"]:
  for e in c.get("env",[]): assert "valueFrom" in e and "secretKeyRef" in e["valueFrom"] and "value" not in e, (c["name"],e)
hy=[c for c in p["containers"] if c["name"]=="hydra"][0]
assert {e["name"] for e in hy["env"]}=={"DSN","SECRETS_SYSTEM"}, hy["env"]
assert hy["readinessProbe"]["httpGet"]["port"]=="admin" and [c for c in p["containers"] if c["name"]=="hydra-keys"][0]["readinessProbe"]["httpGet"]["path"]=="/readyz"' "Hydra pods start one at a time, check the database first, migrate, run read-only and non-root, and take every secret from a Secret"
check_oa 'import json
cm=one("ConfigMap","ambit-hydra")["data"]["hydra.yml"]; c=yaml.safe_load(cm)
low=cm.lower()
assert "dsn" not in low and "secrets" not in low and "password" not in low, "the config holds no secret"
assert c["strategies"]["access_token"]=="jwt" and c["ttl"]["access_token"]=="15m" and c["ttl"]["auth_code"]=="1m" and c["ttl"]["refresh_token"]=="720h", c["ttl"]
assert c["oauth2"]["pkce"]=={"enforced":True,"enforced_for_public_clients":True}, c["oauth2"]
assert c["oidc"]["dynamic_client_registration"]=={"enabled":False}, c["oidc"]
assert c["webfinger"]["jwks"]["broadcast_keys"]==["hydra.jwt.access-token"], c["webfinger"]
assert c["urls"]["self"]["issuer"]=="https://auth.localtest.me" and c["urls"]["login"]=="https://auth.localtest.me/login", c["urls"]
assert c["log"]["leak_sensitive_values"] is False and c["oauth2"]["expose_internal_errors"] is False
# a changed config rolls the pods
assert len(one("StatefulSet","ambit-hydra")["spec"]["template"]["metadata"]["annotations"]["checksum/config"])==64' "Hydra's configuration: 15-minute JWT tokens, PKCE for everyone, no dynamic registration, one published key set, no secret in it"
sec_off=$(sed -n '/^kind: Secret$/,/^---/p' <<<"$fd" | grep -c '^  name:' || true)
sec_on=$(sed -n '/^kind: Secret$/,/^---/p' <<<"$oa" | grep -c '^  name:' || true)
[[ "$sec_off" == "$sec_on" ]] || { echo "FAIL chart: OAuth must render no Secret (the chart holds no key, password or DSN): $sec_off before, $sec_on after"; exit 1; }
check_oa 'for n in ("ambit-hydra-public","ambit-hydra-admin"):
  s=one("Service",n)["spec"]; assert s.get("type","ClusterIP")=="ClusterIP" and len(s["ports"])==1, s
n=one("NetworkPolicy","ambit-hydra")["spec"]
assert n["policyTypes"]==["Ingress"] and n["podSelector"]=={"matchLabels":{"app.kubernetes.io/name":"ambit-hydra"}}, n
pub=[i for i in n["ingress"] if i["ports"]==[{"port":4444}]][0]; adm=[i for i in n["ingress"] if i["ports"]==[{"port":4445}]][0]
assert sorted(list(f["podSelector"]["matchLabels"].values())[0] for f in pub["from"])==["agentgateway","ambit-auth","ambit-console","ambit-public"], pub
assert sorted(list(f["podSelector"]["matchLabels"].values())[0] for f in adm["from"])==["ambit-auth","ambit-console"], adm
assert len(n["ingress"])==2, n
pdb=one("PodDisruptionBudget","ambit-hydra")["spec"]; assert pdb["minAvailable"]==1' "Hydra: ClusterIP Services, the admin port only for ambit-auth and the console, the public port only for the public Gateway, the controller, ambit-auth and the console (the public key list)"
check_oa 'r=one("HTTPRoute","ambit-auth-app")["spec"]
assert r["parentRefs"]==[{"name":"ambit-public","sectionName":"https-auth"}] and r["hostnames"]==["auth.localtest.me"] and len(r["rules"])==1, r
m=sorted((x["method"],x["path"]["type"],x["path"]["value"]) for x in r["rules"][0]["matches"])
assert m==[("GET","Exact","/.well-known/oauth-authorization-server"),("GET","Exact","/callback"),("GET","Exact","/consent"),("GET","Exact","/error"),("GET","Exact","/login"),("GET","Exact","/oauth2/auth"),("POST","Exact","/consent"),("POST","Exact","/register")], m
assert r["rules"][0]["backendRefs"]==[{"name":"ambit-auth","port":8080}], r
# the two routes do not overlap: /oauth2/token, /oauth2/revoke and the key list are Hydra'"'"'s only
assert not any(x["path"]["value"] in ("/oauth2/token","/oauth2/revoke","/.well-known/jwks.json") for x in r["rules"][0]["matches"])
p=one("AgentgatewayPolicy","ambit-auth-public-headers")["spec"]
assert p["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"ambit-auth-app"}], p
t=p["traffic"]["transformation"]["request"]
assert t["set"]==[{"name":"x-ambit-client-address","value":"string(source.address)"}] and set(t["remove"])=={"x-forwarded-for","x-real-ip","forwarded"}, t
v=one("ValidatingAdmissionPolicy","ambit-public-routes")["spec"]["validations"][0]["expression"]
assert "ambit-auth-app" in v and "GET /login" in v, v' "ambit-auth: exact pages on the sign-in host, the Gateway writes the client address and removes forwarded headers, the route policy admits only this route"
check_oa 'd=one("Deployment","ambit-auth")["spec"]; p=d["template"]["spec"]
assert d["replicas"]==2 and p["securityContext"]["runAsNonRoot"] is True, d
c=p["containers"][0]; assert c["command"]==["/ambit-auth","serve"] and c["securityContext"]["readOnlyRootFilesystem"] is True, c
env={e["name"]:e for e in c["env"]}
for n in ("AMBIT_AUTH_IDP_CLIENT_SECRET","AMBIT_AUTH_COOKIE_SECRET"): assert "secretKeyRef" in env[n]["valueFrom"] and "value" not in env[n], n
assert env["AMBIT_AUTH_PUBLIC_URL"]["value"]=="https://auth.localtest.me" and env["AMBIT_AUTH_RESOURCE"]["value"]=="https://mcp-public.localtest.me/mcp", env
assert env["AMBIT_AUTH_REGISTRATION_HOSTS"]["value"]=="chatgpt.com,claude.ai" and env["AMBIT_AUTH_MAX_CLIENTS"]["value"]=="500" and env["AMBIT_AUTH_REGISTER_PER_HOUR"]["value"]=="10", env
assert env["AMBIT_AUTH_VALKEY_ADDRESS"]["value"]=="ambit-valkey:6379" and "AMBIT_AUTH_VALKEY_TLS" not in env, env
pr=p["volumes"][0]["projected"]["sources"][0]["serviceAccountToken"]; assert pr["audience"]=="ambit-console", pr
role=one("Role","ambit-auth")["rules"]
assert role==[{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["ambit-offboarded","ambit-agent-stops","ambit-oauth-rotation"],"verbs":["get"]}], role
n=one("NetworkPolicy","ambit-auth")["spec"]["ingress"]
assert n[0]["ports"]==[{"port":8080}] and n[0]["from"][0]["podSelector"]["matchLabels"]=={"gateway.networking.k8s.io/gateway-name":"ambit-public"}, n
assert n[1]["ports"]==[{"port":8081}] and n[1]["from"][0]["podSelector"]["matchLabels"]=={"app.kubernetes.io/name":"ambit-hydra"}, n
vk=[x for x in docs if x["kind"]=="NetworkPolicy" and x["metadata"]["name"]=="ambit-valkey"][0]["spec"]["ingress"][0]["from"]
assert {"podSelector":{"matchLabels":{"app.kubernetes.io/name":"ambit-auth"}}} in vk, vk' "ambit-auth: two replicas, secrets from Secrets only, a token for the console only, reads three ConfigMaps by name, pages reachable from the public Gateway only, the hook from Hydra only, allowed to reach Valkey"
off_dcr=$(oat --set oauth.registration.dynamic.enabled=false)
! grep -q 'value: /register' <<<"$off_dcr" && ! grep -q 'path: { type: Exact, value: /register }' <<<"$off_dcr" || { echo "FAIL chart: no /register route when dynamic registration is off"; exit 1; }
grep -q 'AMBIT_AUTH_REGISTRATION' <<<"$off_dcr" || { echo "FAIL chart: ambit-auth must be told that registration is off"; exit 1; }
MSG='oauth.signIn.issuer' refuse "no identity provider" --set oauth.signIn.issuer=
MSG='oauth.signIn.issuer' refuse "an http identity provider" --set oauth.signIn.issuer=http://idp.example.com
MSG='end with a slash' refuse "an issuer with a final slash" --set oauth.signIn.issuer=https://idp.example.com/
MSG='oauth.signIn.clientID' refuse "no client ID" --set oauth.signIn.clientID=
MSG='oauth.signIn.clientSecret' refuse "no client secret" --set oauth.signIn.clientSecret.name=
MSG='oauth.cookieSecret' refuse "no cookie secret" --set oauth.cookieSecret.name=
MSG='limits.enabled' refuse "OAuth without shared limits" --set limits.enabled=false
MSG='redirectHosts' refuse "dynamic registration without hosts" --set 'oauth.registration.dynamic.redirectHosts={}'
MSG='oauth.rateLimit.bearerPerMinute must be a whole number' refuse "no bearer limit" --set oauth.rateLimit.bearerPerMinute=0
MSG='lower-case host name' refuse "a wildcard registration host" --set 'oauth.registration.dynamic.redirectHosts={*.example.com}'
MSG='lower-case host name' refuse "a registration host with a scheme" --set 'oauth.registration.dynamic.redirectHosts={https://chatgpt.com}'
MSG='optional path' refuse "a registration path with a dot segment" --set 'oauth.registration.dynamic.redirectHosts={chatgpt.com/a/../b}'
MSG='optional path' refuse "a registration path with a trailing slash" --set 'oauth.registration.dynamic.redirectHosts={chatgpt.com/a/}'
MSG='optional path' refuse "a registration path with a query" --set 'oauth.registration.dynamic.redirectHosts={chatgpt.com/a?b=c}'
MSG='perSourcePerHour' refuse "a registration limit of 0" --set oauth.registration.dynamic.perSourcePerHour=0
MSG='maxClients' refuse "a client cap of 0" --set oauth.registration.dynamic.maxClients=0
# S2-1: the global registration limit x (hours that a never-used client stays + 1) must not exceed the cap of clients
MSG='globalPerHour (300) x (neverUsedClientHours (1) + 1) = 600 is more than maxClients (500)' refuse "a global registration limit that can fill the cap" --set oauth.registration.dynamic.globalPerHour=300
MSG='globalPerHour (200) x (neverUsedClientHours (3) + 1) = 800 is more than maxClients (500)' refuse "a never-used client that stays so long that the global limit fills the cap" --set oauth.registration.dynamic.neverUsedClientHours=3
MSG='globalPerHour (200) x (neverUsedClientHours (1) + 1) = 400 is more than maxClients (300)' refuse "a cap that the defaults can fill" --set oauth.registration.dynamic.maxClients=300
MSG='neverUsedClientHours must be 1 to 720' refuse "a negative number of hours for a never-used client" --set oauth.registration.dynamic.neverUsedClientHours=-1
MSG='globalPerHour must be 1 to 100000' refuse "a negative global registration limit" --set oauth.registration.dynamic.globalPerHour=-1
# ... and values that obey the rule render: 100 an hour x (4 + 1) = 500, and 1000 an hour with a cap of 5000 x 2
exact=$(oat --set oauth.registration.dynamic.globalPerHour=100 --set oauth.registration.dynamic.neverUsedClientHours=4) || { echo "FAIL chart: 100 an hour x (4 + 1) = 500 = maxClients must render"; exit 1; }
grep -q 'AMBIT_AUTH_NEVER_USED_CLIENT_HOURS, value: "4"' <<<"$exact" && grep -q 'AMBIT_AUTH_REGISTER_GLOBAL_PER_HOUR, value: "100"' <<<"$exact" || { echo "FAIL chart: the global limit and the never-used hours do not reach ambit-auth"; exit 1; }
oat --set oauth.registration.dynamic.globalPerHour=1000 --set oauth.registration.dynamic.maxClients=5000 >/dev/null || { echo "FAIL chart: 1000 an hour x 2 with a cap of 5000 must render"; exit 1; }
echo "PASS chart: the global registration limit times the hours of a never-used client must not exceed the client cap (defaults 200 an hour, 1 hour, cap 500)"
check_oa 'r=one("HTTPRoute","ambit-mcp-public")["spec"]
assert [x["name"] for x in r["rules"]]==["keys","oauth"], r["rules"]
k,o=r["rules"]
assert k["matches"]==[{"path":{"type":"Exact","value":"/mcp"},"headers":[{"name":"x-api-key","type":"RegularExpression","value":".*"}]},{"path":{"type":"Exact","value":"/mcp"},"headers":[{"name":"Authorization","type":"RegularExpression","value":"^Bearer ambit_.*"}]}], k["matches"]
assert o["matches"]==[{"path":{"type":"Exact","value":"/mcp"}},{"method":"GET","path":{"type":"Exact","value":"/.well-known/oauth-protected-resource/mcp"}}], o["matches"]
for x in (k,o):
  assert x["backendRefs"]==[{"group":"agentgateway.dev","kind":"AgentgatewayBackend","name":"ambit-mcp-public"}], x
  assert set(x["filters"][0]["requestHeaderModifier"]["remove"])=={"X-Ambit-User","x-api-key","authorization"}, x
ka=one("AgentgatewayPolicy","ambit-mcp-public-auth")["spec"]["targetRefs"]
assert ka==[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"ambit-mcp-public","sectionName":"keys"}], ka
p=one("AgentgatewayPolicy","ambit-mcp-public-oauth")["spec"]
assert p["targetRefs"]==[{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"ambit-mcp-public","sectionName":"oauth"}], p["targetRefs"]
j=p["traffic"]["jwtAuthentication"]
assert j["mode"]=="Strict" and len(j["providers"])==1, j
pr=j["providers"][0]
assert pr["issuer"]=="https://auth.localtest.me" and pr["audiences"]==["https://mcp-public.localtest.me/mcp"], pr
assert pr["jwks"]["remote"]=={"url":"http://ambit-hydra-public.ambit-system.svc:4444/.well-known/jwks.json","cacheDuration":"5m"}, pr["jwks"]
assert pr["validation"]["requiredClaims"]==["aud","exp","sub"], pr
assert j["mcp"]["resourceMetadata"]["resource"]=="https://mcp-public.localtest.me/mcp" and j["mcp"]["resourceMetadata"]["authorization_servers"]==["https://auth.localtest.me"], j["mcp"]
ex=p["traffic"]["authorization"]["policy"]["matchExpressions"]
assert p["traffic"]["authorization"]["action"]=="Require" and "false" not in ex and any("ambit_grant" in e for e in ex) and any("svc.ambit.internal" in e for e in ex), ex
v=one("ValidatingAdmissionPolicy","ambit-public-routes")["spec"]["validations"][0]["expression"]
assert "\"oauth\"" in v and "oauth-protected-resource/mcp" in v, v
c=[x for x in one("Deployment","ambit-controller")["spec"]["template"]["spec"]["containers"][0]["command"]]
assert "--agent-token-issuer=https://auth.localtest.me" in c, c' "the public MCP route: a keys rule and a token rule (more specific match first), each with its own policy; tokens of the sign-in host only, for this MCP URL, a person and an agent and a grant; the controller reads agent claims from that issuer"
check_oa 'e=[x for x in one("Deployment","ambit-console")["spec"]["template"]["spec"]["containers"][0]["env"]]
assert {"name":"AMBIT_OAUTH_ADDR","value":":8085"} in e and {"name":"AMBIT_HYDRA_ADMIN","value":"http://ambit-hydra-admin:4445"} in e, e
assert {"name":"AMBIT_OAUTH_ISSUER","value":"https://auth.localtest.me"} in e and {"name":"AMBIT_OAUTH_RESOURCE","value":"https://mcp-public.localtest.me/mcp"} in e, e
assert {"name":"oauth","port":8085,"targetPort":"oauth"} in one("Service","ambit-console")["spec"]["ports"]
n=one("NetworkPolicy","ambit-console-oauth")["spec"]["ingress"]
assert n[1]=={"from":[{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"ambit-auth"}}}],"ports":[{"port":8085}]}, n
r=one("ClusterRole","ambit-system-ambit-console-tokenreview")["rules"]
assert r==[{"apiGroups":["authentication.k8s.io"],"resources":["tokenreviews"],"verbs":["create"]}], r' "the console: an internal API on its own port for ambit-auth only (network policy and a token check), and the right to check tokens and nothing else of the authentication API"
check_oa 'v=one("ValidatingAdmissionPolicy","ambit-oauth-block-fence")["spec"]
e=v["validations"][0]["expression"]
assert all(sa in e for sa in ("ambit-controller","ambit-console","ambit-auth")), e
assert "ambit-oauth-emergency-block" in v["matchConditions"][0]["expression"] and "oldObject" in v["matchConditions"][0]["expression"], v
b=one("ValidatingAdmissionPolicyBinding","ambit-oauth-block-fence")["spec"]
assert b["validationActions"]==["Deny"] and b["policyName"]=="ambit-oauth-block-fence", b' "the emergency block policy is written by an operator only: Ambit's controller, console and sign-in server are refused (admission), checked by the old object's name on a delete"
[[ "$fd" != *ambit-oauth-block-fence* ]] || { echo "FAIL chart: the block fence is rendered without OAuth"; exit 1; }
check_oa 'ex=one("AgentgatewayPolicy","ambit-mcp-public-oauth")["spec"]["traffic"]["authorization"]["policy"]["matchExpressions"]
aud=[e for e in ex if "jwt.aud" in e]
assert len(aud)==1 and aud[0].count("https://mcp-public.localtest.me/mcp")==2 and "jwt.aud == [" in aud[0], ex' "the token rule requires the audience to be exactly the MCP URL (not only to contain it)"
blk=$(oat --set oauth.emergencyBlock=true)
# (not "oa=$blk check_oa": a variable set for one call is exported, and the rendered chart is more than the 128 KB that Linux allows in one)
( oa=$blk; check_oa 'ex=one("AgentgatewayPolicy","ambit-mcp-public-oauth")["spec"]["traffic"]["authorization"]["policy"]["matchExpressions"]
assert "false" in ex, ex
k=one("AgentgatewayPolicy","ambit-mcp-public-auth")["spec"]["traffic"]["authorization"]["policy"]["matchExpressions"]
assert "false" not in k, "the keys rule must not be touched"' "oauth.emergencyBlock adds a false clause to the token rule only, never to the key rule" )
none=$(helm template ambit charts/ambit -n ambit-system "${fdv[@]}")
! grep -q 'ambit-mcp-public-oauth\|sectionName: keys\|ambit-console-oauth\|ambit-console-tokenreview' <<<"$none" || { echo "FAIL chart: nothing of OAuth in the front door's objects without OAuth"; exit 1; }
# Rate limits of the sign-in host: separate buckets, counted at the Gateway before the route and the credential, by the address of the connection.
check_oa 'e=one("AgentgatewayPolicy","ambit-public-source-limit")["spec"]["traffic"]["extAuth"]["http"]["body"]
b=e.split("\"fd_bucket\", \"value\": ",1)[1].split("}, {\"key\": \"fd_source\"",1)[0]
for want in ("\"/oauth2/token\"","\"/oauth2/revoke\"","\"/register\"","\"/.well-known/\"","\"/mcp\"","\"x-api-key\"","\"authorization\""):  # every other path of the sign-in host is the sign-in bucket
    assert want in b, (want, b)
for bucket in ("token","registration","signin","discovery","mcp","bearer"):
    assert "\"%s\"" % bucket in b, (bucket, b)
assert b.count("request.host")==6 and "\"auth.localtest.me\"" in b and "\"mcp-public.localtest.me\"" in b, b  # the host is part of the choice: the sign-in buckets only on the sign-in host (security review N8)
assert b.index("request.host")<b.index("\"/oauth2/token\""), b
assert "string(source.address)" in e and "x-forwarded" not in e.lower() and "x-ambit-client-address" not in e.lower() and "forwarded" not in b.lower(), e
c=yaml.safe_load(one("ConfigMap","ambit-ratelimit-front-door")["data"]["front-door.yaml"])
got={d["value"]:(d["descriptors"][0]["key"],d["descriptors"][0]["rate_limit"]["unit"],d["descriptors"][0]["rate_limit"]["requests_per_unit"]) for d in c["descriptors"] if d["key"]=="fd_bucket"}
assert got=={"mcp":("fd_source","minute",120),"discovery":("fd_source","minute",600),"signin":("fd_source","minute",300),"token":("fd_source","minute",600),"registration":("fd_source","hour",10),"bearer":("fd_source","minute",1200)}, got
# #436: the bearer bucket is only for a header that is, as a whole, "Bearer" and a token in the shape of a JWT, and only without x-api-key, on the MCP host and the path /mcp.
# The shape is the security point (the bucket is chosen before the token is checked): the key rule reads "Bearer " off the header with a replace of every occurrence,
# so "Bearer Bearer ambit_..." is a key and must stay in the mcp bucket. The same regular expression, tried here on the cases that matter.
import re
rx=re.search(r"matches\(\"(\^Bearer eyJ[^\"]*)\"\)", b).group(1).replace("\\\\","\\")
seg=b.rsplit("\"discovery\" : ",1)[1]  # the condition of the bearer bucket: the MCP host, the path /mcp, no key header, a header in the shape
assert seg.count("request.host")==2 and "mcp-public.localtest.me" in seg and "auth.localtest.me" not in seg.replace("mcp-public.localtest.me","") and "request.path == \"/mcp\"" in seg and "!(\"x-api-key\" in request.headers)" in seg and ".matches(" in seg and seg.rstrip().endswith("\"bearer\" : \"mcp\""), seg
good="Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnLW5hdHVyZQ"
for h,want in ((good,True),("Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.",True),("Bearer ambit_0123456789abcdef",False),("Bearer Bearer ambit_0123456789abcdef",False),("Bearer Bearer "+good[7:],False),("Bearer abc",False),
               ("Bearer ",False),("bearer "+good[7:],False),(good+" x",False),(good+", Bearer ambit_k",False),("Bearer  "+good[7:],False),("Bearer xeyJ.a.b",False),("Bearer eyJ.a",False),(good[7:],False)):
    assert bool(re.search(rx,h))==want, (h,want,rx)
assert "ambit_" not in "eyJ"  # no key starts with eyJ: a header in the shape is never a key
env={e["name"]:e["value"] for e in one("Deployment","ambit-auth")["spec"]["template"]["spec"]["containers"][0]["env"] if "value" in e}
g=one("AgentgatewayPolicy","ambit-public-limits")["spec"]["traffic"]["rateLimit"]["global"]["descriptors"][0]["entries"][0]["expression"]
assert "jwt.ambit_agent" in g and "apiKey.keyId" in g and g.endswith("\"none\")"), g  # a token counts for its agent, not in one counter for every token; no credential: no counter
assert c["descriptors"][0]=={"key":"fd_credential","value":"none","rate_limit":{"unlimited":True}} and c["descriptors"][1]["key"]=="fd_credential" and "value" not in c["descriptors"][1], c["descriptors"][:2]
assert env["AMBIT_AUTH_DISCOVERY_PER_MINUTE"]=="600" and env["AMBIT_AUTH_SIGNIN_PER_MINUTE"]=="300" and env["AMBIT_AUTH_REGISTER_PER_HOUR"]=="10" and env["AMBIT_AUTH_REGISTER_GLOBAL_PER_HOUR"]=="200" and env["AMBIT_AUTH_NEVER_USED_CLIENT_HOURS"]=="1" and env["AMBIT_AUTH_MAX_CLIENTS"]=="500", env' "the source limit has five buckets (mcp, discovery, sign-in, token, registration): the Gateway reads the path and two header names, never a client address header; ambit-auth gets the same numbers"
set_rl=$(oat --set oauth.rateLimit.signInPerMinute=77 --set oauth.registration.dynamic.perSourcePerHour=3)
bearer_rl=$(oat --set oauth.rateLimit.bearerPerMinute=77)
grep -q 'requests_per_unit: 77' <<<"$bearer_rl" && ! grep -q 'requests_per_unit: 1200' <<<"$bearer_rl" || { echo "FAIL chart: oauth.rateLimit.bearerPerMinute must reach the ratelimit config"; exit 1; }
grep -q 'requests_per_unit: 77' <<<"$set_rl" && grep -q 'requests_per_unit: 3 }' <<<"$set_rl" && grep -q 'AMBIT_AUTH_SIGNIN_PER_MINUTE, value: "77"' <<<"$set_rl" || { echo "FAIL chart: the bucket values must reach the ratelimit config and ambit-auth"; exit 1; }
MSG='oauth.rateLimit.signInPerMinute' refuse "a sign-in limit of 0" --set oauth.rateLimit.signInPerMinute=0
MSG='whole number' refuse "a sign-in limit with a fraction" --set oauth.rateLimit.signInPerMinute=77.5
MSG='oauth.rateLimit.tokenPerMinute' refuse "a token limit of 0" --set oauth.rateLimit.tokenPerMinute=0
MSG='oauth.rateLimit.discoveryPerMinute' refuse "a discovery limit of 0" --set oauth.rateLimit.discoveryPerMinute=0
# The Hydra janitor: a daily CronJob on the same database Secret, with Hydra's own lifetimes, one run at a time, no secret in it; off with
# oauth.janitor.enabled=false, and not there at all without OAuth.
check_oa 'j=one("CronJob","ambit-hydra-janitor")["spec"]
assert j["schedule"]=="17 * * * *" and j["concurrencyPolicy"]=="Forbid", j
t=j["jobTemplate"]["spec"]; p=t["template"]["spec"]; c=p["containers"][0]
assert t["activeDeadlineSeconds"]==1800 and t["backoffLimit"]==2 and p["restartPolicy"]=="Never", t
assert c["args"][-4:]==["--batch-size","1000","--limit","100000"], c["args"]
assert c["args"][:6]==["janitor","-e","-c","/etc/hydra/hydra.yml","--tokens","--requests"] and "--grants" in c["args"] and "--keep-if-younger" in c["args"], c["args"]
assert c["image"]==[x for x in one("StatefulSet","ambit-hydra")["spec"]["template"]["spec"]["containers"] if x["name"]=="hydra"][0]["image"], "the janitor runs the image of Hydra"
assert [e["name"] for e in c["env"]]==["DSN"] and "secretKeyRef" in c["env"][0]["valueFrom"] and "value" not in c["env"][0], c["env"]
assert c["securityContext"]["readOnlyRootFilesystem"] is True and p["securityContext"]["runAsNonRoot"] is True and p["automountServiceAccountToken"] is False, p
assert {v["name"] for v in p["volumes"]}=={"config","ca","tmp"} and [v for v in p["volumes"] if v["name"]=="config"][0]["configMap"]["name"]=="ambit-hydra", p["volumes"]' "the Hydra janitor: a daily CronJob, one run at a time, the image and database Secret of Hydra, Hydra's own lifetimes, read-only and non-root, no secret value"
off_j=$(oat --set oauth.janitor.enabled=false)
! grep -q 'ambit-hydra-janitor' <<<"$off_j" || { echo "FAIL chart: oauth.janitor.enabled=false must render no janitor"; exit 1; }
! grep -q 'ambit-hydra-janitor' <<<"$fd" || { echo "FAIL chart: no janitor without OAuth"; exit 1; }
small=$(oat --set oauth.janitor.limit=3 | sed -n '/^kind: CronJob/,/^---/p')
grep -q '"--batch-size", "3"\|--batch-size, "3", --limit, "3"' <<<"$small" || { echo "FAIL chart: a batch is never larger than the limit (Hydra refuses it)"; exit 1; }
sch=$(oat --set oauth.janitor.schedule='5 4 * * 0' --set oauth.janitor.timeoutMinutes=10 | sed -n '/^kind: CronJob/,/^---/p')
grep -q 'schedule: "5 4 \* \* 0"' <<<"$sch" && grep -q 'activeDeadlineSeconds: 600' <<<"$sch" || { echo "FAIL chart: the janitor schedule and time limit are values"; exit 1; }
MSG='maxGrantDays' refuse "a grant life of 400 days" --set oauth.maxGrantDays=400
one_r=$(oat --set oauth.hydra.replicas=1)
! grep -q 'name: ambit-hydra$' <(sed -n '/kind: PodDisruptionBudget/,/^---/p' <<<"$one_r") || { echo "FAIL chart: one Hydra pod needs no PodDisruptionBudget"; exit 1; }
MSG='frontDoor.enabled' refuse "OAuth without the front door" --set frontDoor.enabled=false
MSG='frontDoor.authHost' refuse "OAuth without a sign-in host" --set frontDoor.authHost=
MSG='own' refuse "the sign-in host named like the MCP host" --set frontDoor.authHost=mcp-public.localtest.me
MSG='own' refuse "the sign-in host named like the console" --set frontDoor.authHost=console.localtest.me
MSG='oauth.database.dsnSecret' refuse "no DSN Secret" --set oauth.database.dsnSecret.name=
MSG='oauth.database.caSecret' refuse "no CA Secret" --set oauth.database.caSecret.key=
MSG='oauth.hydra.systemSecret' refuse "no system Secret" --set oauth.hydra.systemSecret.name=
MSG='accessTokenMinutes' refuse "a token that lives two hours" --set oauth.accessTokenMinutes=120
MSG='refreshTokenIdleDays' refuse "no refresh life" --set oauth.refreshTokenIdleDays=0
MSG='oauth.hydra.replicas' refuse "no Hydra pod" --set oauth.hydra.replicas=0
MSG='agents.stopRetentionMinutes' refuse "a stop entry that does not outlive the access tokens" --set oauth.accessTokenMinutes=30 --set agents.stopRetentionMinutes=30
MSG='agents.tokenIssuer' refuse "another agent token issuer than the sign-in host" --set agents.tokenIssuer=https://other.example.com
rm -f "$W2"
echo "PASS chart"
