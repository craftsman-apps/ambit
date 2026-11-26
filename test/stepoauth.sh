#!/usr/bin/env bash
# Step oauth: OAuth sign-in (oauth.*, docs/oauth.md), the part that runs without a person signing in: Hydra on a
# PostgreSQL database with TLS, its one signing key, the exact public routes, the network rules, the database checks
# and the way pods start. (The sign-in flow itself is added with ambit-auth.) The test makes its own PostgreSQL 17 and 15
# (test/oauth/postgres.yaml), certificates and passwords, and prints none of them. Nothing is exposed: the test reaches
# the pods through kubectl port-forward on random local ports. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
source test/frontdoor/lib.sh
PUB=mcp-public.localtest.me
AUTH=auth.localtest.me
PYI=python@sha256:79e7a9b9ff1cbceff819f856fb374477792a5967759d94df266de7b7b4120e6f
L=ambitproject.dev/oauth-test=true
: > "$W/pids"
cleanup() {
  for p in $(cat "$W/pids" 2>/dev/null); do kill "$p" 2>/dev/null || true; done
  kubectl -n "$NS" delete pod -l "$L" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete secret -l "$L" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete statefulset,deployment,service,configmap -l "$L" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pvc -l "$L" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete job ambit-hydra-janitor-e2e --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap ambit-offboarded ambit-agent-stops --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT
rnd() { openssl rand -hex "${1:-16}"; }
oaset() { helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/frontdoor/values.yaml -f test/oauth/values.yaml \
  --set frontDoor.rateLimit.perCredentialPerMinute=600 --set frontDoor.rateLimit.perSourcePerMinute=1000 --set frontDoor.rateLimit.perPodPerSecond=200 \
  --set "oauth.signIn.issuer=https://ambit-oauth-idp.$NS.svc" --set "oauth.auth.consoleURL=http://ambit-oauth-console.$NS.svc:8085" "$@" >/dev/null; }
# oaset_real: the same, with the real console behind ambit-auth (the stand-in is for the sign-in checks of step 12)
oaset_real() { oaset --set "oauth.auth.consoleURL=" "$@"; }
freeport() { "${PY[@]}" -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
# adm POD METHOD PATH [json]: Hydra's admin API in one pod, through a port-forward. Prints the HTTP status; the body is in $W/adm.
adm() {
  local pod=$1 m=$2 p=$3 port pid code
  port=$(freeport)
  kubectl -n "$NS" port-forward "pod/$pod" "$port:4445" >/dev/null 2>&1 & pid=$!
  for _ in $(seq 1 40); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.3; done
  code=$(command curl -s -o "$W/adm" -w '%{http_code}' --max-time 20 -X "$m" ${4:+-H 'Content-Type: application/json' -d "$4"} "http://127.0.0.1:$port$p" || true)
  kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
  echo "$code"
}
kids() { # kids POD SET: the key IDs of a set, space separated ("-" when the set does not exist)
  local c; c=$(adm "$1" GET "/admin/keys/$2")
  [[ "$c" == 404 ]] && { echo "-"; return; }
  [[ "$c" == 200 ]] || { echo "<$c>"; return; }
  "${PY[@]}" -c 'import json,sys;print(" ".join(sorted(k["kid"] for k in json.load(open(sys.argv[1]))["keys"])))' "$W/adm"
}
# auth PORT METHOD PATH [curl args]: one request to the sign-in host on a public Gateway pod. The status is printed, the body is in $W/au.
auth() {
  local port=$1 m=$2 p=$3; shift 3
  command curl -s -o "$W/au" -D "$W/auh" -w '%{http_code}' --max-time 15 --cacert "$W/ca.pem" --path-as-is --connect-to "$AUTH:443:127.0.0.1:$port" -X "$m" "$@" "https://$AUTH$p" || true
}
pubpods() { kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit-public --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'; }
hydra_ready() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null; }
IMG=$(kubectl -n "$NS" get deploy ambit-console -o jsonpath='{.spec.template.spec.containers[0].image}')

# --- 0 · setup: certificates, two PostgreSQL servers with TLS only, their roles, the Secrets, the install
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=oauth-test-ca" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" -keyout "$W/dbca.key" -out "$W/dbca.pem" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=oauth-other-ca" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" -keyout "$W/other.key" -out "$W/other.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$W/pg.key" -out "$W/pg.csr" -subj "/CN=ambit-pg" >/dev/null 2>&1
SANS=""; for v in 17 15; do SANS+="DNS:ambit-pg$v.$NS.svc,DNS:ambit-pg$v.$NS.svc.cluster.local,"; done
printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "${SANS%,}" > "$W/pg.ext"
openssl x509 -req -in "$W/pg.csr" -CA "$W/dbca.pem" -CAkey "$W/dbca.key" -CAcreateserial -out "$W/pg.crt" -days 1 -extfile "$W/pg.ext" >/dev/null 2>&1
# the public certificate: one for both public host names (the same Secret serves both listeners)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$PUB" -addext "subjectAltName=DNS:$PUB,DNS:$AUTH" -keyout "$W/tls.key" -out "$W/ca.pem" >/dev/null 2>&1
kubectl -n "$NS" create secret tls ambit-public-tls --cert="$W/ca.pem" --key="$W/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
SU=$(rnd); HP=$(rnd); SP=$(rnd); SYS=$(rnd 24)
sec() { kubectl -n "$NS" create secret generic "$@" --dry-run=client -o yaml | kubectl label --local -f - "$L" -o yaml | kubectl apply -f - >/dev/null; }
sec ambit-pg-tls --from-file=tls.crt="$W/pg.crt" --from-file=tls.key="$W/pg.key"
sec ambit-pg-superuser --from-literal=password="$SU"
sec ambit-oauth-db-ca --from-file=ca.crt="$W/dbca.pem"
sec ambit-oauth-wrong-ca --from-file=ca.crt="$W/other.pem"
sec ambit-oauth-hydra --from-literal=system="$SYS"
kubectl -n "$NS" apply -f test/oauth/postgres.yaml >/dev/null
for v in 17 15; do kubectl -n "$NS" rollout status "sts/ambit-pg$v" --timeout=300s >/dev/null; done
for v in 17 15; do
  kubectl -n "$NS" exec "ambit-pg$v-0" -- psql -q -U postgres -v ON_ERROR_STOP=1 \
    -c "CREATE ROLE hydra LOGIN PASSWORD '$HP' NOSUPERUSER CONNECTION LIMIT 40" -c "CREATE DATABASE hydra OWNER hydra" -c "CREATE DATABASE hydra_conc OWNER hydra" \
    -c "CREATE ROLE hydra_small LOGIN PASSWORD '$SP' NOSUPERUSER CONNECTION LIMIT 10" -c "CREATE DATABASE hydra_small OWNER hydra_small" >/dev/null
done
# dsn VERSION [ROLE PASSWORD DATABASE HOST-SUFFIX EXTRA]: the address Hydra gets; sslmode=verify-full unless EXTRA replaces it
dsn() {
  local v=$1 role=${2:-hydra} pw=${3:-$HP} db=${4:-hydra} host=${5-.$NS.svc} extra=${6:-sslmode=verify-full&sslrootcert=/ca/ca.crt&max_conns=8&max_idle_conns=4}
  echo "postgres://$role:$pw@ambit-pg$v$host:5432/$db?$extra"
}
sec ambit-oauth-db --from-literal=dsn="$(dsn 17)"
# the company's identity provider and the console's internal API: stand-ins (test/oauthtest), with a certificate of the test CA
IDPH="ambit-oauth-idp.$NS.svc"
openssl req -newkey rsa:2048 -nodes -keyout "$W/idp.key" -out "$W/idp.csr" -subj "/CN=$IDPH" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$IDPH" > "$W/idp.ext"
openssl x509 -req -in "$W/idp.csr" -CA "$W/dbca.pem" -CAkey "$W/dbca.key" -CAcreateserial -out "$W/idp.crt" -days 1 -extfile "$W/idp.ext" >/dev/null 2>&1
kubectl -n "$NS" create secret tls ambit-oauth-idp-tls --cert="$W/idp.crt" --key="$W/idp.key" --dry-run=client -o yaml | kubectl label --local -f - "$L" -o yaml | kubectl apply -f - >/dev/null
sec ambit-oauth-idp --from-literal=client-secret="$(rnd 24)"
sec ambit-oauth-cookie --from-literal=cookie="$(rnd 32)"
sec ambit-oauth-idp-ca --from-file=ca.crt="$W/dbca.pem"
kubectl -n "$NS" create configmap ambit-oauth-test --from-literal=issuer="https://$IDPH" --from-literal=redirect="https://$AUTH/callback" --dry-run=client -o yaml | kubectl label --local -f - "$L" -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" apply -f test/oauth/auth-test.yaml >/dev/null
for d in ambit-oauth-idp ambit-oauth-console; do kubectl -n "$NS" rollout status "deploy/$d" --timeout=300s >/dev/null; done
oaset --wait --timeout 8m
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
pass "0 installed: PostgreSQL 17 and 15 with TLS only, Hydra on 17 (2 pods), ambit-auth (2 pods), the public Gateway with the sign-in host"

# --- 1 · the pods start one at a time, and each key set holds exactly one key that Ambit created
t0=$(kubectl -n "$NS" get pod ambit-hydra-0 -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}')
c1=$(kubectl -n "$NS" get pod ambit-hydra-1 -o jsonpath='{.metadata.creationTimestamp}')
[[ "$c1" > "$t0" || "$c1" == "$t0" ]] || fail "1 pod 1 was created ($c1) before pod 0 was Ready ($t0)"
KA=$(kids ambit-hydra-0 hydra.jwt.access-token); KI=$(kids ambit-hydra-0 hydra.openid.id-token)
[[ "$KA" != *" "* && "$KA" != "-" && "$KA" != "<"* ]] || fail "1 the access-token set must hold one key, has: $KA"
[[ "$KI" != *" "* && "$KI" != "-" && "$KI" != "<"* ]] || fail "1 the id-token set must hold one key, has: $KI"
[[ "$(kids ambit-hydra-1 hydra.jwt.access-token)" == "$KA" ]] || fail "1 the two pods see different access-token keys"
for p in ambit-hydra-0 ambit-hydra-1; do
  n=$(kubectl -n "$NS" logs "$p" -c hydra | grep -c 'does not exist yet' || true)
  [[ "$n" == 0 ]] || fail "1 Hydra of $p created a key set by itself ($n times): the key must come from hydra-keys"
done
pass "1 pod 1 started after pod 0 was Ready; both key sets hold one key; Hydra created none itself"

# --- 2 · the public routes: exact paths and methods, nothing else of Hydra
PODS=($(pubpods)); [[ ${#PODS[@]} -eq 2 ]] || fail "2 want 2 public gateway pods, have: ${PODS[*]}"
PA=$(forward "${PODS[0]}"); PB=$(forward "${PODS[1]}")
wait_for 180 '[[ $(auth "$PA" GET /.well-known/jwks.json) == 200 ]]' || fail "2 the JWKS is not served on the sign-in host: HTTP $(auth "$PA" GET /.well-known/jwks.json) $(head -c 200 "$W/au")"
for port in "$PA" "$PB"; do
  [[ $(auth "$port" GET /.well-known/jwks.json) == 200 ]] || fail "2 JWKS on a public pod: $(head -c 200 "$W/au")"
  "${PY[@]}" -c 'import json,sys;k=json.load(open(sys.argv[1]))["keys"];assert len(k)==2 and all("d" not in x for x in k), k;print(" ".join(sorted(x["kid"] for x in k)))' "$W/au" > "$W/jk" || fail "2 the JWKS must hold two public keys and no private part"
  grep -q "$KA" "$W/jk" || fail "2 the JWKS does not hold the access-token key $KA"
done
for p in /oauth2/token /oauth2/revoke; do
  c=$(auth "$PA" POST "$p" -d x=y); [[ "$c" == 400 || "$c" == 401 ]] && grep -q '"error"' "$W/au" || fail "2 POST $p without a client: HTTP $c $(head -c 200 "$W/au")"
done
for p in /oauth2/register /.well-known/openid-configuration /userinfo /oauth2/device/auth /oauth2/device/verify /oauth2/sessions/logout /oauth2/fallbacks/error \
         /oauth2/fallbacks/consent /admin/keys/hydra.jwt.access-token /admin/clients /health/ready /health/alive /version /metrics/prometheus / /mcp /hooks/token /internal/stop /metrics /healthz \
         /oauth2//token /oauth2/token/ /OAUTH2/token '/oauth2/%74oken' /oauth2/./token /oauth2/x/../token /LOGIN /login/ /callback/ /consent/ /register/ /oauth2/auth/; do
  for m in GET POST; do
    c=$(auth "$PA" "$m" "$p" -d x=y)
    # /oauth2/./token and /oauth2/x/../token are the same path as /oauth2/token to a proxy that normalises; the others are not
    if [[ "$m" == POST && ( "$p" == /oauth2/./token || "$p" == /oauth2/x/../token || "$p" == '/oauth2/%74oken' ) ]]; then
      [[ "$c" == 404 || "$c" == 400 || "$c" == 401 ]] || fail "2 $m $p: HTTP $c"
    else
      [[ "$c" == 404 ]] || fail "2 $m $p on the sign-in host: HTTP $c, want 404: $(head -c 200 "$W/au")"
    fi
  done
done
for m in PUT DELETE PATCH OPTIONS HEAD; do
  for p in /oauth2/token /oauth2/revoke /.well-known/jwks.json; do
    c=$(auth "$PA" "$m" "$p"); [[ "$c" == 404 ]] || fail "2 $m $p: HTTP $c, want 404"
  done
done
[[ $(auth "$PA" GET /oauth2/token) == 404 && $(auth "$PA" GET /oauth2/revoke) == 404 && $(auth "$PA" POST /.well-known/jwks.json) == 404 ]] || fail "2 a wrong method on a Hydra path is not 404"
# Hydra's paths under the other public host name, and a request for another name, never reach Hydra
c=$(command curl -s -o "$W/au" -w '%{http_code}' --max-time 15 --cacert "$W/ca.pem" --resolve "$PUB:$PA:127.0.0.1" "https://$PUB:$PA/.well-known/jwks.json" || true)
[[ "$c" == 404 ]] || fail "2 the JWKS under the MCP host: HTTP $c"
c=$(command curl -sk -o "$W/au" -w '%{http_code}' --max-time 15 --resolve "other.localtest.me:$PA:127.0.0.1" "https://other.localtest.me:$PA/.well-known/jwks.json" || true)
[[ "$c" == 404 || "$c" == 000 ]] || fail "2 the JWKS under another host name: HTTP $c"
c=$(auth "$PA" GET /.well-known/jwks.json -H 'Host: console.localtest.me'); [[ "$c" == 404 ]] || fail "2 Host header console.localtest.me under the sign-in name: HTTP $c"
pass "2 on the sign-in host: JWKS (GET), token and revoke (POST) reach Hydra; 70 other paths, methods and path variants and other host names are 404"

# the admission policy: a change of the Hydra route that opens more than the chart wrote is refused (server-side dry run)
for patch in '[{"op":"add","path":"/spec/rules/0/filters/-","value":{"type":"URLRewrite","urlRewrite":{"path":{"type":"ReplaceFullPath","replaceFullPath":"/admin/keys/hydra.jwt.access-token"}}}}]' \
  '[{"op":"replace","path":"/spec/rules/0/backendRefs/0/port","value":4445}]' \
  '[{"op":"replace","path":"/spec/rules/0/backendRefs/0/name","value":"ambit-hydra-admin"}]' \
  '[{"op":"add","path":"/spec/rules/0/matches/-","value":{"method":"GET","path":{"type":"Exact","value":"/oauth2/sessions/logout"}}}]' \
  '[{"op":"remove","path":"/spec/rules/0/matches/2"}]' \
  '[{"op":"replace","path":"/spec/parentRefs/0/sectionName","value":"https"}]' \
  '[{"op":"add","path":"/spec/rules/0/filters/0/requestHeaderModifier/remove","value":["authorization"]}]' \
  '[{"op":"replace","path":"/spec/hostnames/0","value":"*.example.com"}]'; do
  out=$(kubectl -n "$NS" patch httproute ambit-auth-hydra --type json -p "$patch" --dry-run=server 2>&1) && fail "2 the admission policy admitted a change of the Hydra route: $patch"
  grep -q "ambit-public-routes" <<<"$out" || fail "2 the change was refused, but not by the route policy: $patch: $(head -c 200 <<<"$out")"
done
for patch in '[{"op":"add","path":"/spec/rules/0/filters/-","value":{"type":"RequestMirror","requestMirror":{"backendRef":{"name":"ambit-hydra-admin","port":4445}}}}]' \
  '[{"op":"add","path":"/spec/rules/0/filters/-","value":{"type":"URLRewrite","urlRewrite":{"path":{"type":"ReplaceFullPath","replaceFullPath":"/admin/keys/hydra.jwt.access-token"}}}}]' \
  '[{"op":"remove","path":"/spec/rules/0/filters"}]' \
  '[{"op":"replace","path":"/spec/rules/0/filters/0/requestHeaderModifier/remove","value":["X-Ambit-User"]}]' \
  '[{"op":"replace","path":"/spec/rules/0/backendRefs/0/group","value":"other.example"}]'; do
  out=$(kubectl -n "$NS" patch httproute ambit-mcp-public --type json -p "$patch" --dry-run=server 2>&1) && fail "2 the admission policy admitted a change of the MCP route: $patch"
  grep -q "ambit-public-routes" <<<"$out" || fail "2 the change was refused, but not by the route policy: $patch: $(head -c 200 <<<"$out")"
done
# a change that keeps what the chart wrote is admitted: the policy refuses the change, not the route
kubectl -n "$NS" patch httproute ambit-auth-hydra --type json -p '[{"op":"replace","path":"/spec/rules/0/name","value":"token"}]' --dry-run=server >/dev/null 2>&1 || fail "2 a change that keeps the chart's route was refused"
pass "2 the route policy refuses a rewrite, a mirror, another header edit, another backend or port, an extra or a missing path, a wildcard host and another listener"

# --- 3 · who may reach Hydra (NetworkPolicy)
probe() { # probe NAME [label=value ...]: a pod that can run Python, with the given labels
  local n=$1; shift
  kubectl -n "$NS" delete pod "$n" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl -n "$NS" run "$n" --image="$PYI" --restart=Never --labels="$(IFS=,; echo "ambitproject.dev/oauth-test=true${*:+,$*}")" --command -- sleep 600 >/dev/null
}
reach() { # reach POD URL: prints 200, or the failure ("no" after 4 s)
  kubectl -n "$NS" exec "$1" -- python -c 'import sys,urllib.request
try: print(urllib.request.urlopen(sys.argv[1], timeout=4).status)
except Exception as e: print("no")' "$2" 2>/dev/null || echo no
}
HPUB=http://ambit-hydra-public:4444/health/ready; HADM=http://ambit-hydra-admin:4445/health/ready
probe np-other; probe np-auth app.kubernetes.io/name=ambit-auth; probe np-gw gateway.networking.k8s.io/gateway-name=ambit-public
probe np-agw app.kubernetes.io/name=agentgateway; probe np-private gateway.networking.k8s.io/gateway-name=ambit; probe np-console app.kubernetes.io/name=ambit-console
kubectl -n "$NS" wait pod -l "$L" --for=condition=Ready --timeout=180s >/dev/null
[[ $(reach np-other "$HPUB") == no && $(reach np-other "$HADM") == no ]] || fail "3 an unrelated pod reaches Hydra"
[[ $(reach np-private "$HPUB") == no && $(reach np-private "$HADM") == no ]] || fail "3 a pod of the private Gateway reaches Hydra"
[[ $(reach np-gw "$HPUB") == 200 && $(reach np-gw "$HADM") == no ]] || fail "3 the public Gateway must reach 4444 and not 4445"
[[ $(reach np-agw "$HPUB") == 200 && $(reach np-agw "$HADM") == no ]] || fail "3 agentgateway's controller must reach 4444 and not 4445"
[[ $(reach np-auth "$HADM") == 200 && $(reach np-auth "$HPUB") == 200 ]] || fail "3 ambit-auth must reach 4445 and 4444 (the authorize front)"
[[ $(reach np-console "$HADM") == 200 && $(reach np-console "$HPUB") == no ]] || fail "3 the console must reach 4445 and not 4444"
kubectl -n "$NS" delete pod -l "$L" --wait=false >/dev/null
pass "3 Hydra's public port is open to the public Gateway and the controller only; the admin port to ambit-auth and the console only (ambit-auth also reaches the public port, for its authorize front)"

# --- 4 · the database check, one pod per case (the init container of Hydra's pods, run alone)
chk() { # chk NAME DSN [replicas [ca secret]]: runs check-database; prints "ok" or "refused", the log in $W/chk-NAME
  local n=chk-$1 ca=${4:-ambit-oauth-db-ca}
  kubectl -n "$NS" delete pod "$n" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $n, labels: { ambitproject.dev/oauth-test: "true" } }
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: $IMG
      imagePullPolicy: IfNotPresent
      command: [/ambit-auth, check-database, --replicas=${3:-2}]
      env: [{ name: DSN, value: "$2" }]
      volumeMounts: [{ name: ca, mountPath: /ca }]
  volumes: [{ name: ca, secret: { secretName: $ca, items: [{ key: ca.crt, path: ca.crt }] } }]
EOF
  wait_for 90 "[[ \$(kubectl -n '$NS' get pod $n -o jsonpath='{.status.phase}') == Succeeded || \$(kubectl -n '$NS' get pod $n -o jsonpath='{.status.phase}') == Failed ]]" || { echo hung; return; }
  kubectl -n "$NS" logs "$n" > "$W/chk-$1" 2>&1 || true
  [[ $(kubectl -n "$NS" get pod "$n" -o jsonpath='{.status.phase}') == Succeeded ]] && echo ok || echo refused
}
want() { # want NAME ok|refused PATTERN DSN [replicas [ca]]
  local r; r=$(chk "$1" "$4" "${5:-2}" "${6:-ambit-oauth-db-ca}")
  [[ "$r" == "$2" ]] || fail "4 $1: $r, want $2: $(tail -c 400 "$W/chk-$1")"
  grep -q -- "$3" "$W/chk-$1" || fail "4 $1: the log must say '$3': $(tail -c 400 "$W/chk-$1")"
  ! grep -qE "$HP|$SU|$SP" "$W/chk-$1" || fail "4 $1: the log repeats a password"
}
want pg17 ok 'postgresMajor=17' "$(dsn 17)"
want pg15 ok 'postgresMajor=15' "$(dsn 15)"
for m in disable allow prefer require verify-ca; do
  want "mode-$m" refused "sslmode=verify-full (it says $m)" "$(dsn 17 hydra "$HP" hydra .$NS.svc "sslmode=$m&sslrootcert=/ca/ca.crt&max_conns=8")"
done
want nomode refused 'sslmode=verify-full (it says (not set))' "$(dsn 17 hydra "$HP" hydra .$NS.svc 'sslrootcert=/ca/ca.crt&max_conns=8')"
want noca refused 'sslrootcert' "$(dsn 17 hydra "$HP" hydra .$NS.svc 'sslmode=verify-full&max_conns=8')"
want nopool refused 'max_conns' "$(dsn 17 hydra "$HP" hydra .$NS.svc 'sslmode=verify-full&sslrootcert=/ca/ca.crt')"
want wrongca refused 'certificate was not accepted' "$(dsn 17)" 2 ambit-oauth-wrong-ca
want wrongname refused 'certificate was not accepted' "$(dsn 17 hydra "$HP" hydra '')"          # the short Service name is not in the certificate
want superuser refused 'is a superuser' "$(dsn 17 postgres "$SU")"
want limit refused 'CONNECTION LIMIT' "$(dsn 17 hydra_small "$SP" hydra_small)" 2
want limitok ok 'database ok' "$(dsn 17 hydra_small "$SP" hydra_small)" 1
want badpw refused 'SQLSTATE 28P01' "$(dsn 17 hydra WRONG$HP)"
pass "4 the database check: 17 and 15 pass; sslmode disable, allow, prefer, require, verify-ca and none, no CA file, no pool limit, a wrong CA, a name that is not in the certificate, a superuser, too few connections and a wrong password are refused; no log repeats a password"

# --- 5 · a bad database address stops one pod, not the service
sec ambit-oauth-db-bad --from-literal=dsn="$(dsn 17 hydra "$HP" hydra .$NS.svc 'sslmode=require&sslrootcert=/ca/ca.crt&max_conns=8')"
oaset --set oauth.database.dsnSecret.name=ambit-oauth-db-bad
init_exit() { kubectl -n "$NS" get pod ambit-hydra-1 -o jsonpath='{.status.initContainerStatuses[?(@.name=="check-database")].lastState.terminated.exitCode}{.status.initContainerStatuses[?(@.name=="check-database")].state.terminated.exitCode}' 2>/dev/null; }
wait_for 180 '[[ "$(init_exit)" == 1* ]]' || fail "5 the database check of the new pod did not fail: exit '$(init_exit)'"
kubectl -n "$NS" logs ambit-hydra-1 -c check-database 2>&1 | grep -q 'sslmode=verify-full (it says require)' || fail "5 the pod's log does not name the fix: $(kubectl -n "$NS" logs ambit-hydra-1 -c check-database 2>&1 | tail -c 300)"
[[ "$(hydra_ready ambit-hydra-0)" == True ]] || fail "5 pod 0 must keep serving while pod 1 is refused"
[[ $(auth "$PA" GET /.well-known/jwks.json) == 200 ]] || fail "5 the JWKS must still be served from pod 0"
# A StatefulSet does not replace a pod that failed its start when the template is fixed: the operator deletes it (docs/oauth.md).
oaset
kubectl -n "$NS" delete pod ambit-hydra-1 --wait=true >/dev/null
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
pass "5 a Secret with sslmode=require stops the roll-out at the first pod with a message that names the fix; the other pod keeps serving; the right Secret and a deleted pod recover"

# --- 6 · migrations: two runs at the same time on an empty database (Hydra v26.2.0 can fail one of them: that is why the pods start one at a time), then a repeat
sec ambit-oauth-db-conc --from-literal=dsn="$(dsn 15 hydra "$HP" hydra_conc)"
HYIMG=$(kubectl -n "$NS" get sts ambit-hydra -o jsonpath='{.spec.template.spec.containers[?(@.name=="hydra")].image}')
mig() { # mig NAME: Hydra's migration against the empty database hydra_conc on PostgreSQL 15
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $1, labels: { ambitproject.dev/oauth-test: "true" } }
spec:
  restartPolicy: Never
  containers:
    - name: m
      image: $HYIMG
      args: [migrate, sql, up, -e, --yes]
      env: [{ name: DSN, valueFrom: { secretKeyRef: { name: ambit-oauth-db-conc, key: dsn } } }]
      volumeMounts: [{ name: ca, mountPath: /ca }]
  volumes: [{ name: ca, secret: { secretName: ambit-oauth-db-ca, items: [{ key: ca.crt, path: ca.crt }] } }]
EOF
}
phase_of() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.phase}'; }
await() { wait_for 120 "[[ \$(phase_of $1) == Succeeded || \$(phase_of $1) == Failed ]]" || fail "6 $1 did not finish"; phase_of "$1"; }
mig mig-a; mig mig-b
pa=$(await mig-a); pb=$(await mig-b)
# Hydra v26.2.0 can fail both runs of an empty database that start together (a race on its first table): what matters is that a
# run on its own then succeeds, as the retry of a pod's init container does
echo "  two at the same time: $pa and $pb"
for m in mig-a mig-b; do [[ $(phase_of $m) == Succeeded ]] || kubectl -n "$NS" logs $m 2>&1 | grep -iE "SQLSTATE|level=error" | head -2 | cut -c1-200; done
mig mig-c
[[ $(await mig-c) == Succeeded ]] || fail "6 a repeat of the migration must succeed whatever the first two did: $(kubectl -n "$NS" logs mig-c | tail -c 400)"
pass "6 two migrations of an empty PostgreSQL 15 database started together: $pa and $pb (they may fail if they overlap: the pods start one at a time); a run on its own then succeeds"

# --- 7 · another database: PostgreSQL 15 (a new, empty database gets its own key), then back to 17 (its key is still there)
sec ambit-oauth-db-15 --from-literal=dsn="$(dsn 15)"
oaset --set oauth.database.dsnSecret.name=ambit-oauth-db-15 --wait --timeout 8m
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
K15=$(kids ambit-hydra-0 hydra.jwt.access-token)
[[ "$K15" != "$KA" && "$K15" != *" "* && "$K15" != "-" && "$K15" != "<"* && "$(kids ambit-hydra-1 hydra.jwt.access-token)" == "$K15" ]] || fail "7 on the new PostgreSQL 15 database both pods must see one new key, got $K15 and $(kids ambit-hydra-1 hydra.jwt.access-token) (old $KA)"
wait_for 60 '[[ $(auth "$PA" GET /.well-known/jwks.json) == 200 ]]' || fail "7 no JWKS on PostgreSQL 15"
grep -q "$K15" "$W/au" || fail "7 the JWKS does not hold the new key"
oaset --wait --timeout 8m
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
[[ "$(kids ambit-hydra-0 hydra.jwt.access-token)" == "$KA" && "$(kids ambit-hydra-1 hydra.jwt.access-token)" == "$KA" ]] || fail "7 back on PostgreSQL 17 the first key must be there again"
pass "7 on PostgreSQL 15 (empty database): migrated, one new key, JWKS served; back on 17: the first key is there again"

# --- 8 · a second key in a signing set takes the pods out of service; removing it brings them back
[[ $(adm ambit-hydra-0 POST /admin/keys/hydra.jwt.access-token '{"alg":"RS256","use":"sig"}') == 201 ]] || fail "8 could not add a key: $(head -c 200 "$W/adm")"
wait_for 90 '[[ "$(hydra_ready ambit-hydra-0)" != True && "$(hydra_ready ambit-hydra-1)" != True ]]' || fail "8 a set with two keys must make both pods NotReady"
kubectl -n "$NS" logs ambit-hydra-0 -c hydra-keys --tail=5 | grep -q 'holds 2 keys' || fail "8 the log must say the set holds 2 keys"
c=$(auth "$PA" GET /.well-known/jwks.json); [[ "$c" != 200 ]] || fail "8 the JWKS is still served with two signing keys"
K2=$(kids ambit-hydra-0 hydra.jwt.access-token); EXTRA=$(tr ' ' '\n' <<<"$K2" | grep -vx "$KA")
[[ $(adm ambit-hydra-0 DELETE "/admin/keys/hydra.jwt.access-token/$EXTRA") == 204 ]] || fail "8 could not remove the extra key"
wait_for 90 '[[ "$(hydra_ready ambit-hydra-0)" == True && "$(hydra_ready ambit-hydra-1)" == True ]]' || fail "8 the pods are not Ready again after the extra key was removed"
wait_for 60 '[[ $(auth "$PA" GET /.well-known/jwks.json) == 200 ]]' || fail "8 no JWKS after recovery"
pass "8 a second signing key: both pods NotReady and the JWKS not served; removed: Ready again"

# --- 9 · restarts keep the key
kubectl -n "$NS" delete pod ambit-hydra-0 --wait=true >/dev/null
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
[[ "$(kids ambit-hydra-0 hydra.jwt.access-token)" == "$KA" ]] || fail "9 pod 0 came back with another key"
kubectl -n "$NS" delete pod -l app.kubernetes.io/name=ambit-hydra --wait=true >/dev/null
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
[[ "$(kids ambit-hydra-0 hydra.jwt.access-token)" == "$KA" && "$(kids ambit-hydra-1 hydra.jwt.access-token)" == "$KA" ]] || fail "9 after both pods restarted the key changed"
for p in ambit-hydra-0 ambit-hydra-1; do
  n=$(kubectl -n "$NS" logs "$p" -c hydra | grep -c 'does not exist yet' || true); [[ "$n" == 0 ]] || fail "9 Hydra of $p created a key set by itself"
done
pass "9 one pod restarted, then both: the signing key is the same, and Hydra created none itself"

# --- 10 · no secret outside a Secret: not in any object of the chart, not in a log
{
  helm get manifest ambit -n "$NS"
  # not the test's own pods: check pods of step 4 carry a DSN as a plain value on purpose
  kubectl -n "$NS" get configmap,statefulset,deployment,pod -l '!ambitproject.dev/oauth-test' -o yaml
  for p in ambit-hydra-0 ambit-hydra-1; do for c in check-database migrate hydra hydra-keys; do kubectl -n "$NS" logs "$p" -c "$c" 2>&1 || true; done; done
} > "$W/everything"
for s in "$HP" "$SU" "$SP" "$SYS"; do ! grep -q "$s" "$W/everything" || fail "10 a password or key shows in the chart's objects or in a log"; done
pass "10 the database password, the superuser password and the system secret are in no ConfigMap, StatefulSet, pod spec, Helm manifest or log"

# --- 12 · the sign-in itself: discovery, registration, the code flow at the company's identity provider, consent, tokens, refresh,
#         stop and offboarding of a refresh, two sign-ins in one browser, what is refused, the limits, a restart (test/oauthtest/flow.py)
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
PODS=($(pubpods)); PA=$(forward "${PODS[0]}")
IDPP=$(freeport); kubectl -n "$NS" port-forward svc/ambit-oauth-idp "$IDPP:443" >/dev/null 2>&1 & echo $! >> "$W/pids"
CONP=$(freeport); kubectl -n "$NS" port-forward svc/ambit-oauth-console "$CONP:8085" >/dev/null 2>&1 & echo $! >> "$W/pids"
wait_for 60 '(exec 3<>"/dev/tcp/127.0.0.1/$IDPP") 2>/dev/null && (exec 3<>"/dev/tcp/127.0.0.1/$CONP") 2>/dev/null' || fail "12 port-forwards"
cat "$W/ca.pem" "$W/dbca.pem" > "$W/allca.pem"
flow() { GW_PORT=$PA IDP_PORT=$IDPP CA_FILE="$W/allca.pem" AUTH_HOST=$AUTH MCP_HOST=$PUB IDP_HOST=$IDPH CONSOLE_URL="http://127.0.0.1:$CONP" KUBECTL="kubectl -n $NS" KUBECTL_CONSOLE="kubectl -n $NS --as=system:serviceaccount:$NS:ambit-console --as-user-extra=authentication.kubernetes.io/pod-name=oauth-test" \
  "${PY[@]}" test/oauthtest/flow.py "$@" || fail "12 the sign-in checks failed (see FAIL lines above)"; }
# the registration limit first, at its default of 10 per hour and address (the counter is per hour: nothing registered before)
flow limits
# then the limit is raised, so that the many registrations of the other checks pass
oaset --set oauth.registration.dynamic.perSourcePerHour=1000 --wait --timeout 8m
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
flow
pass "12 the sign-in flow and its refusals"

# --- 14 · tokens at the public Gateway, with the real console behind ambit-auth (test/oauthtest/gateway.py)
# The 401 challenge, the metadata, the consented scope and nothing else, changed and forged tokens, offboarding, the emergency block (also with
# Hydra down), API keys on the same route, a restart of the console. A server on the front door, an AccessRule for everyone, and a key.
G=ambitproject.dev
oaset_real --set oauth.registration.dynamic.perSourcePerHour=1000 --wait --timeout 8m
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: oauth-time, labels: { ambitproject.dev/oauth-test: "true" } }
spec: { allSignedIn: true, servers: [{ name: time }, { name: everything }] }
EOF
kubectl -n "$NS" rollout status deploy/ambit-guard --timeout=300s >/dev/null || fail "14 ambit-guard is not running"
kubectl -n "$NS" wait "servers.$G" time everything --for=condition=Ready --timeout=300s >/dev/null || fail "14 servers not Ready"
kubectl -n "$NS" wait "servers.$G" time everything --for=condition=ToolsPinned --timeout=300s >/dev/null || fail "14 tools not pinned"
kubectl -n "$NS" patch "servers.$G" time --type merge -p '{"spec":{"frontDoor":{"enabled":true,"acceptUnmaskedServerMessages":true}}}' >/dev/null
wait_for 120 "kubectl -n '$NS' get agentgatewaybackend ambit-mcp-public >/dev/null && kubectl -n '$NS' get agentgatewaypolicy ambit-mcp-public-access >/dev/null" || fail "14 no public backend"
kubectl -n "$NS" wait agentgatewaybackend/ambit-mcp-public --for=condition=Accepted --timeout=120s >/dev/null || fail "14 public backend not accepted"
accepted() { kubectl -n "$NS" get agentgatewaypolicy "$1" -o jsonpath='{.status.ancestors[*].conditions[?(@.type=="Accepted")].status}' 2>/dev/null; }
wait_for 120 '[[ "$(accepted ambit-mcp-public-oauth)" == *True* && "$(accepted ambit-mcp-public-auth)" == *True* ]]' || fail "14 the token or key policy is not accepted: oauth '$(accepted ambit-mcp-public-oauth)' keys '$(accepted ambit-mcp-public-auth)': $(kubectl -n "$NS" get agentgatewaypolicy ambit-mcp-public-oauth ambit-mcp-public-auth -o jsonpath='{range .items[*]}{.metadata.name}: {.status.ancestors[*].conditions[*].message}{"\n"}{end}')"
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW"
mkkey oauth-key mcp; OKEY=$KEY; OKID=$KID
cleanup_keys() { api DELETE "/api/keys/$OKID" >/dev/null 2>&1 || true; kubectl -n "$NS" delete "accessrules.$G" oauth-time --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" patch "servers.$G" time --type json -p '[{"op":"remove","path":"/spec/frontDoor"}]' >/dev/null 2>&1 || true; }
trap 'rc=$?; cleanup_keys; cleanup; on_exit "$rc"' EXIT
PODS=($(pubpods)); [[ ${#PODS[@]} -eq 2 ]] || fail "14 want 2 public gateway pods, have: ${PODS[*]}"
PA=$(forward "${PODS[0]}"); PB=$(forward "${PODS[1]}")
IDPP=$(freeport); kubectl -n "$NS" port-forward svc/ambit-oauth-idp "$IDPP:443" >/dev/null 2>&1 & echo $! >> "$W/pids"
CONP=$(freeport); kubectl -n "$NS" port-forward svc/ambit-oauth-console "$CONP:8085" >/dev/null 2>&1 & echo $! >> "$W/pids"
wait_for 60 '(exec 3<>"/dev/tcp/127.0.0.1/$IDPP") 2>/dev/null && (exec 3<>"/dev/tcp/127.0.0.1/$CONP") 2>/dev/null' || fail "14 port-forwards"
GW_PORT=$PA GW_PORTS="$PA,$PB" IDP_PORT=$IDPP CA_FILE="$W/allca.pem" AUTH_HOST=$AUTH MCP_HOST=$PUB IDP_HOST=$IDPH CONSOLE_URL="http://127.0.0.1:$CONP" KUBECTL="kubectl -n $NS" \
  STOPCTL="$PWD/test/oauthtest/stopctl.sh" ADMIN_PW="$PW" HTTP_TIMEOUT=90 KEY="$OKEY" ONE_TOOL=time_get_current_time "${PY[@]}" test/oauthtest/gateway.py || fail "14 the gateway checks failed (see FAIL lines above)"
pass "14 tokens at the public Gateway"

# --- 16 · PostgreSQL operations (docs/oauth.md, "Operations"): TLS only, the pool, a dump, a restore into another instance and a name moved
#         to it, a crash, a 30 second outage, and what a rollback by `hydra migrate sql down` does. Names P1 to P6 are those of the design.
#         What the people see is checked with real grants (test/oauthtest/dbops.py). Needs the real console (check 14).
oaset_real --set oauth.registration.dynamic.perSourcePerHour=1000 --wait --timeout 8m
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=300s >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
reforward
[[ ${#PODS[@]} -eq 2 && "${PODS[0]}" != "${PODS[1]}" ]] || fail "16 want two different public gateway pods, have: ${PODS[*]}"
dbo() { GW_PORT=$PA GW_PORTS="$PA,$PB" IDP_PORT=$IDPP CA_FILE="$W/allca.pem" AUTH_HOST=$AUTH MCP_HOST=$PUB IDP_HOST=$IDPH CONSOLE_URL="http://127.0.0.1:$CONP" KUBECTL="kubectl -n $NS" \
  HTTP_TIMEOUT=60 KEY="$OKEY" ONE_TOOL=time_get_current_time "${PY[@]}" test/oauthtest/dbops.py "$@"; }
now() { "${PY[@]}" -c 'import time;print("%.2f" % time.time())'; }
live() { echo "$(kubectl -n "$NS" get svc ambit-pg17 -o jsonpath='{.spec.selector.app}')-0"; }  # the pod the name ambit-pg17 points to
psq() { kubectl -n "$NS" exec "$(live)" -c postgres -- psql -q -At -U postgres -d hydra -c "$1"; }
hydra_both_ready() { [[ "$(hydra_ready ambit-hydra-0)" == True && "$(hydra_ready ambit-hydra-1)" == True ]]; }
jwks_ok() { [[ $(auth "$PA" GET /.well-known/jwks.json) == 200 ]]; }
dbo grants 4 "$W/g.json" 0 >/dev/null || fail "16 could not make four connections (grants) to keep"
for i in 0 1 2 3; do [[ "$(dbo refresh "$W/g.json" $i)" == "200 " ]] || fail "16 grant $i does not refresh before the tests"; done

# P1: TLS only. A connection without TLS is refused by the server; every connection of Hydra uses TLS.
out=$(kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- env PGPASSWORD="$HP" psql "host=127.0.0.1 sslmode=disable user=hydra dbname=hydra" -c 'select 1' 2>&1 || true)
grep -qE 'pg_hba.conf (rejects|entry)' <<<"$out" || fail "16 P1 a connection without TLS must be refused by the server, got: $(head -c 200 <<<"$out")"
! grep -q "$HP" <<<"$out" || fail "16 P1 the refusal repeats the password"
TLSV=$(kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- env PGPASSWORD="$HP" psql -q -At "host=127.0.0.1 sslmode=require user=hydra dbname=hydra" -c 'select version from pg_stat_ssl where pid = pg_backend_pid()')
[[ "$TLSV" == TLSv1.* ]] || fail "16 P1 the session must use TLS, got '$TLSV'"
SSLC=$(psq "select count(*) filter (where s.ssl), count(*) filter (where not s.ssl) from pg_stat_activity a join pg_stat_ssl s on s.pid = a.pid where a.usename = 'hydra'")
[[ "$SSLC" == [1-9]*"|0" ]] || fail "16 P1 every connection of Hydra must use TLS (with TLS | without): $SSLC"
pass "16 P1 PostgreSQL 17: a connection without TLS is refused; a session with TLS uses $TLSV; Hydra's connections (with TLS | without) = $SSLC"

# P3: the pool. 60 concurrent clients straight to both Hydra pods for 15 s: the database sees at most pods x max_conns connections of the role.
HPA=$(freeport); HPB=$(freeport)
kubectl -n "$NS" port-forward pod/ambit-hydra-0 "$HPA:4444" >/dev/null 2>&1 & echo $! >> "$W/pids"
kubectl -n "$NS" port-forward pod/ambit-hydra-1 "$HPB:4444" >/dev/null 2>&1 & echo $! >> "$W/pids"
wait_for 30 '(exec 3<>"/dev/tcp/127.0.0.1/$HPA") 2>/dev/null && (exec 3<>"/dev/tcp/127.0.0.1/$HPB") 2>/dev/null' || fail "16 P3 port-forwards to Hydra"
( for _ in $(seq 1 12); do psq "select count(*) from pg_stat_activity where usename = 'hydra'"; sleep 1; done > "$W/conns" 2>/dev/null ) & SAMPLER=$!
dbo load 15 60 "$W/load.json" "$HPA" "$HPB" >/dev/null
wait "$SAMPLER" 2>/dev/null || true
MAXC=$(sort -n "$W/conns" | tail -1)
"${PY[@]}" -c 'import json,sys
r=json.load(open(sys.argv[1])); c=r["codes"]
print("   %d requests, p50 %d ms, p95 %d ms, statuses %s" % (r["requests"], r["p50_ms"], r["p95_ms"], c))
assert set(c) <= {"400", "401"}, c' "$W/load.json" || fail "16 P3 under load Hydra must answer 4xx for a made-up token and nothing else: $(cat "$W/load.json")"
[[ -n "$MAXC" && "$MAXC" -le 16 ]] || fail "16 P3 the role used $MAXC connections, more than 2 pods x max_conns 8"
pass "16 P3 60 concurrent clients for 15 s on both pods: at most $MAXC database connections (limit 2 x 8 = 16), no error answer"

# P4: a logical dump of the live database.
kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- rm -f /tmp/hydra.dump
t0=$(now)
kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- pg_dump -Fc -U postgres -d hydra -f /tmp/hydra.dump
t1=$(now)
DUMPB=$(kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- stat -c %s /tmp/hydra.dump)
CL0=$(psq "select count(*) from hydra_client"); RF0=$(psq "select count(*) from hydra_oauth2_refresh"); FL0=$(psq "select count(*) from hydra_oauth2_flow")
[[ "$DUMPB" -gt 1000 && "$CL0" -gt 3 && "$RF0" -gt 3 ]] || fail "16 P4 dump $DUMPB bytes, clients $CL0, refresh tokens $RF0"
DUMPS=$("${PY[@]}" -c "print(round($t1 - $t0, 1))")
pass "16 P4 pg_dump -Fc of the live database: $DUMPB bytes in $DUMPS s ($CL0 clients, $RF0 refresh tokens, $FL0 login and consent records)"

# A grant that refreshes AFTER the backup: its new refresh token is not in the dump.
[[ "$(dbo refresh "$W/g.json" 1)" == "200 " ]] || fail "16 grant 1 does not refresh after the dump"

# P5: restore into a new instance, then move the name ambit-pg17 to it and stop the old one, while probes run for 80 s.
kubectl -n "$NS" scale sts/ambit-pg17r --replicas=1 >/dev/null
kubectl -n "$NS" rollout status sts/ambit-pg17r --timeout=300s >/dev/null
kubectl -n "$NS" exec ambit-pg17-0 -c postgres -- cat /tmp/hydra.dump | kubectl -n "$NS" exec -i ambit-pg17r-0 -c postgres -- sh -c 'cat > /tmp/hydra.dump'
kubectl -n "$NS" exec ambit-pg17r-0 -c postgres -- psql -q -U postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE hydra LOGIN PASSWORD '$HP' NOSUPERUSER CONNECTION LIMIT 40" -c "CREATE DATABASE hydra OWNER hydra" >/dev/null
t0=$(now)
kubectl -n "$NS" exec ambit-pg17r-0 -c postgres -- pg_restore -U postgres -d hydra --exit-on-error /tmp/hydra.dump
t1=$(now)
RESTS=$("${PY[@]}" -c "print(round($t1 - $t0, 1))")
r() { kubectl -n "$NS" exec ambit-pg17r-0 -c postgres -- psql -q -At -U postgres -d hydra -c "$1"; }
[[ "$(r 'select count(*) from hydra_client')" == "$CL0" && "$(r 'select count(*) from hydra_oauth2_refresh')" == "$RF0" && "$(r 'select count(*) from hydra_oauth2_flow')" == "$FL0" ]] \
  || fail "16 P5 the restored database must hold what the dump held (clients $CL0, refresh tokens $RF0, flows $FL0)"
dbo probe 80 "$W/probe-failover.json" >/dev/null & PRB=$!; echo $PRB >> "$W/pids"
sleep 8
T_SWITCH=$(now)
kubectl -n "$NS" patch svc ambit-pg17 --type merge -p '{"spec":{"selector":{"app":"ambit-pg17r"}}}' >/dev/null
kubectl -n "$NS" scale sts/ambit-pg17 --replicas=0 >/dev/null
wait "$PRB" || true
REP=$(dbo report "$W/probe-failover.json"); echo "   fail-over probes: $REP"
probes_ok() { [[ "$(sed -E 's/^probes=([0-9]+) .*/\1/' <<<"$1")" -ge "$2" ]]; }  # a probe that died early must not pass as "no errors"
probes_ok "$REP" 40 || fail "16 P5 the probe ran too few times: $REP"
[[ "$REP" == *"end_ok=True"* ]] || fail "16 P5 Hydra did not recover after the name moved to the restored instance: $REP"
wait_for 60 'hydra_both_ready && jwks_ok' || fail "16 P5 Hydra pods are not Ready on the restored instance"
FIRST=$(sed -E 's/.* first=([0-9.]+) .*/\1/' <<<"$REP"); LAST=$(sed -E 's/.* last=([0-9.]+) .*/\1/' <<<"$REP"); NERR=$(sed -E 's/.* errors=([0-9]+) .*/\1/' <<<"$REP")
FO=$("${PY[@]}" -c "f,l,s=$FIRST,$LAST,$T_SWITCH; print('no error' if $NERR==0 else f'{$NERR} error probes between {f-s:.1f} s and {l-s:.1f} s after the switch')")
# What the people see: the grant that was not used since the dump refreshes; the one that refreshed after it must connect again.
[[ "$(dbo refresh "$W/g.json" 0)" == "200 " ]] || fail "16 P5 a grant that was not used since the backup must refresh on the restored database"
[[ "$(dbo refresh "$W/g.json" 1)" == "400 invalid_grant" ]] || fail "16 P5 a grant that refreshed after the backup must be refused with invalid_grant, got: $(dbo refresh "$W/g.json" 1)"
dbo grants 1 "$W/g-new.json" 10 >/dev/null || fail "16 P5 a new sign-in must work on the restored database"
pass "16 P5 restore of the dump into a new PostgreSQL 17 in $RESTS s, the name ambit-pg17 moved to it and the old one stopped: $FO; a grant not used since the backup refreshes (200), one refreshed after it is refused (invalid_grant) and connects again, a new sign-in works"

# P6: the database pod is deleted (its volume stays): Hydra recovers by itself, the data is there.
dbo probe 45 "$W/probe-crash.json" >/dev/null & PRB=$!; echo $PRB >> "$W/pids"
sleep 5
T_DEL=$(now)
kubectl -n "$NS" delete pod ambit-pg17r-0 --wait=false >/dev/null
wait "$PRB" || true
REP=$(dbo report "$W/probe-crash.json"); echo "   after deleting the database pod: $REP"
probes_ok "$REP" 25 || fail "16 P6 the probe ran too few times: $REP"
[[ "$REP" == *"end_ok=True"* ]] || fail "16 P6 Hydra did not recover after the database pod was deleted: $REP"
wait_for 60 'hydra_both_ready && jwks_ok' || fail "16 P6 Hydra pods are not Ready after the database pod came back"
[[ "$(dbo refresh "$W/g.json" 0)" == "200 " ]] || fail "16 P6 the data must be there after the pod came back (a grant refreshes)"
FIRST=$(sed -E 's/.* first=([0-9.]+) .*/\1/' <<<"$REP"); LAST=$(sed -E 's/.* last=([0-9.]+) .*/\1/' <<<"$REP"); NERR=$(sed -E 's/.* errors=([0-9]+) .*/\1/' <<<"$REP")
P6=$("${PY[@]}" -c "f,l,s=$FIRST,$LAST,$T_DEL; print('no error' if $NERR==0 else f'{$NERR} error probes between {f-s:.1f} s and {l-s:.1f} s after the delete')")
pass "16 P6 database pod deleted with its volume kept: $P6; the data is there (a grant refreshes)"

# 30 second outage: the database is stopped for 30 s. An access token that was issued before keeps working at the gateway; sign-in and
# refresh answer 5xx while no Hydra pod is Ready; recovery is measured from the moment the database accepts connections.
[[ "$(dbo refresh "$W/g.json" 0)" == "200 " ]] || fail "16 outage: grant 0 does not refresh before the outage"
[[ "$(dbo mcp "$W/g.json" 0)" == "200 200" ]] || fail "16 outage: the access token is not accepted at both pods before the outage: $(dbo mcp "$W/g.json" 0)"
gw5xx() { # the sum of the Gateway's 5xx counters of the route to Hydra, on the first public pod (port 15020, read through a port-forward)
  local gm; gm=$(freeport); kubectl -n "$NS" port-forward "pod/${PODS[0]}" "$gm:15020" >/dev/null 2>&1 & local pf=$!
  wait_for 30 "(exec 3<>/dev/tcp/127.0.0.1/$gm) 2>/dev/null" || { kill "$pf" 2>/dev/null; echo -1; return; }
  command curl -s --max-time 10 "http://127.0.0.1:$gm/metrics" | grep '^agentgateway_requests_total{' | grep "route=\"$NS/ambit-auth-hydra\"" | grep -E 'status="5[0-9][0-9]"' \
    | "${PY[@]}" -c 'import re,sys;print(sum(float(l.rsplit(" ",1)[1]) for l in sys.stdin))'
  kill "$pf" 2>/dev/null || true
}
G5_BEFORE=$(gw5xx)
dbo probe 100 "$W/probe-outage.json" >/dev/null & PRB=$!; echo $PRB >> "$W/pids"
sleep 5
T_DOWN=$(now)
kubectl -n "$NS" scale sts/ambit-pg17r --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod/ambit-pg17r-0 --timeout=60s >/dev/null 2>&1 || true
T_NOTREADY=""; MCP_DURING=""
for _ in $(seq 1 40); do
  [[ -z "$T_NOTREADY" && "$(hydra_ready ambit-hydra-0)" != True && "$(hydra_ready ambit-hydra-1)" != True ]] && T_NOTREADY=$(now)
  [[ $("${PY[@]}" -c "print(int($(now) - $T_DOWN))") -ge 30 ]] && break
  sleep 1
done
MCP_DURING=$(dbo mcp "$W/g.json" 0)
T_UP=$(now)
kubectl -n "$NS" scale sts/ambit-pg17r --replicas=1 >/dev/null
kubectl -n "$NS" rollout status sts/ambit-pg17r --timeout=300s >/dev/null
T_DBREADY=$(now)
wait "$PRB" || true
REP=$(dbo report "$W/probe-outage.json"); echo "   30 second outage: $REP"
probes_ok "$REP" 50 || fail "16 outage: the probe ran too few times: $REP"
[[ "$REP" == *"end_ok=True"* && "$REP" != *"errors=0 "* ]] || fail "16 outage: the probes must show errors during the outage and none at the end: $REP"
[[ -n "$T_NOTREADY" ]] || fail "16 outage: both Hydra pods must be NotReady while the database is down"
[[ "$MCP_DURING" == "200 200" ]] || fail "16 outage: an access token that was issued before must keep working at the gateway during the outage, got: $MCP_DURING"
LAST=$(sed -E 's/.* last=([0-9.]+) .*/\1/' <<<"$REP"); FIRST=$(sed -E 's/.* first=([0-9.]+) .*/\1/' <<<"$REP")
OUT=$("${PY[@]}" -c "print(f'first error {$FIRST-$T_DOWN:.1f} s after the stop, last error {$LAST-$T_DBREADY:+.1f} s from the moment the database was Ready; Hydra NotReady {$T_NOTREADY-$T_DOWN:.1f} s after the stop; database down for {$T_UP-$T_DOWN:.0f} s')")
RECOV=$("${PY[@]}" -c "print($LAST-$T_DBREADY)")
"${PY[@]}" -c "import sys;sys.exit(0 if $RECOV < 60 else 1)" || fail "16 outage: sign-in and refresh took more than 60 s to recover after the database was Ready: $OUT"
wait_for 60 'hydra_both_ready && jwks_ok' || fail "16 outage: Hydra pods are not Ready after the database came back"
# The alert AmbitOAuthTokenErrors reads the Gateway's request counter of the route to Hydra: the 5xx of the outage must have raised it.
G5_AFTER=$(gw5xx)
"${PY[@]}" -c "import sys;sys.exit(0 if $G5_AFTER > $G5_BEFORE >= 0 else 1)" || fail "16 outage: the Gateway's 5xx counter for the route ambit-auth-hydra did not rise ($G5_BEFORE before, $G5_AFTER after; the alert AmbitOAuthTokenErrors reads it)"
[[ "$(dbo refresh "$W/g.json" 0)" == "200 " ]] || fail "16 outage: a refresh must work after the outage"
pass "16 30 second outage of the database: $OUT; the access token kept working at both gateway pods during it ($MCP_DURING); refresh works again"

# Rotation of the database password: change the role's password, change the Secret, roll Hydra's pods one at a time. Connections that exist keep
# working (a password is checked when a connection starts), so the probes (made-up refresh requests) show how long the new connections are refused.
NEWPW=$(rnd)
dbo probe 100 "$W/probe-rot.json" >/dev/null & PRB=$!; echo $PRB >> "$W/pids"
sleep 3
T_ROT=$(now)
psq "alter role hydra password '$NEWPW'" >/dev/null
OLDPW=$HP; HP=$NEWPW
sec ambit-oauth-db --from-literal=dsn="$(dsn 17)"
kubectl -n "$NS" rollout restart sts/ambit-hydra >/dev/null
kubectl -n "$NS" rollout status sts/ambit-hydra --timeout=300s >/dev/null
wait_for 60 'hydra_both_ready && jwks_ok' || fail "16 password rotation: Hydra pods are not Ready with the new password"
wait "$PRB" || true
REP=$(dbo report "$W/probe-rot.json"); echo "   password rotation: $REP"
probes_ok "$REP" 50 || fail "16 password rotation: the probe ran too few times: $REP"
[[ "$REP" == *"end_ok=True"* ]] || fail "16 password rotation: Hydra did not recover: $REP"
[[ "$(dbo refresh "$W/g.json" 0)" == "200 " ]] || fail "16 password rotation: a grant must refresh with the new password"
kubectl -n "$NS" logs ambit-hydra-0 -c hydra > "$W/hydra0.log" 2>&1 || fail "16 password rotation: the Hydra log cannot be read"
[[ -s "$W/hydra0.log" ]] && ! grep -qE "$OLDPW|$NEWPW" "$W/hydra0.log" || fail "16 password rotation: a Hydra log is empty or holds a password"
NERR=$(sed -E 's/.* errors=([0-9]+) .*/\1/' <<<"$REP")
pass "16 rotation of the database password (ALTER ROLE, a new Secret, the pods rolled one at a time): $NERR error probes of a 100 s run, Hydra on the new password, a grant refreshes, no log holds a password"
# A file with two CAs (the middle of a CA rotation): the start check and verify-full accept it.
cat "$W/other.pem" "$W/dbca.pem" > "$W/bundle.pem"
sec ambit-oauth-db-ca-bundle --from-file=ca.crt="$W/bundle.pem"
want cabundle ok 'database ok' "$(dsn 17)" 2 ambit-oauth-db-ca-bundle
pass "16 a CA file with two certificates (an unrelated one first, the right one second) passes the database check with sslmode=verify-full"

# Rollback by `hydra migrate sql down`: NOT a rollback for a live database. One step down (the latest Hydra migration of v26.2.0) removes every
# login and consent record, and with them (a cascading rule of the database) every access and refresh token. Clients stay. This is why the runbook
# restores the backup that was taken before the upgrade, and does not run `migrate sql down`.
CL1=$(psq "select count(*) from hydra_client"); RF1=$(psq "select count(*) from hydra_oauth2_refresh"); SM1=$(psq "select count(*) from schema_migration")
hyrun() { # hyrun NAME ARGS...: one run of Hydra's image on the live database (the Secret ambit-oauth-db); prints Succeeded or Failed
  local n=$1; shift
  kubectl -n "$NS" delete pod "$n" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" apply -f - >/dev/null <<EOY
apiVersion: v1
kind: Pod
metadata: { name: $n, labels: { ambitproject.dev/oauth-test: "true" } }
spec:
  restartPolicy: Never
  containers:
    - name: m
      image: $HYIMG
      args: [$(printf '"%s", ' "$@" | sed 's/, $//')]
      env: [{ name: DSN, valueFrom: { secretKeyRef: { name: ambit-oauth-db, key: dsn } } }]
      volumeMounts: [{ name: ca, mountPath: /ca }]
  volumes: [{ name: ca, secret: { secretName: ambit-oauth-db-ca, items: [{ key: ca.crt, path: ca.crt }] } }]
EOY
  await "$n"
}
[[ "$(hyrun mig-down migrate sql down -e --yes --steps 1)" == Succeeded ]] || fail "16 migrate down failed: $(kubectl -n "$NS" logs mig-down | tail -c 300)"
[[ "$(psq 'select count(*) from schema_migration')" == "$((SM1 - 1))" ]] || fail "16 one step down must remove one migration"
RF2=$(psq "select count(*) from hydra_oauth2_refresh"); FL2=$(psq "select count(*) from hydra_oauth2_flow"); AC2=$(psq "select count(*) from hydra_oauth2_access")
[[ "$(hyrun mig-up migrate sql up -e --yes)" == Succeeded ]] || fail "16 migrate up failed: $(kubectl -n "$NS" logs mig-up | tail -c 300)"
[[ "$(psq 'select count(*) from schema_migration')" == "$SM1" && "$(psq 'select count(*) from hydra_client')" == "$CL1" ]] || fail "16 after up again the migrations and the clients must be as before"
[[ "$RF2" == 0 && "$FL2" == 0 && "$AC2" == 0 && "$RF1" -gt 0 ]] || fail "16 one step down: expected the flows, refresh tokens and access tokens to go (flows $FL2, refresh tokens $RF2, access tokens $AC2, before: refresh tokens $RF1)"
[[ "$(dbo refresh "$W/g.json" 0)" == "400 invalid_grant" ]] || fail "16 after migrate down every grant must be refused with invalid_grant, got: $(dbo refresh "$W/g.json" 0)"
dbo grants 1 "$W/g-new2.json" 20 >/dev/null || fail "16 a new sign-in must work after migrate down and up"
pass "16 P2 \`hydra migrate sql down --steps 1\` then \`up\`: the clients stay ($CL1), the login and consent records and all $RF1 refresh tokens are gone, every grant must connect again, a new sign-in works. A rollback is a restore of the backup, not migrate down"

# --- 17 · the clean-up: `hydra janitor` (the CronJob, run once by hand) and the clients that nobody used (ambit-auth)
# Rows are made old by SQL (a token lives 15 minutes and a refresh token 30 days: the test cannot wait): a grant whose tokens are 40 days old, and a
# live grant whose login and consent record is two days old (10 minutes is the janitor's limit for a record): the janitor must remove the tokens of the
# first and must not touch the second (the record of a sign-in holds the refresh token by a cascading rule of the database: if the janitor deleted
# it, the connection would end), nor any grant younger than an hour.
dbo grants 3 "$W/j.json" 30 >/dev/null || fail "17 could not make three connections"
JLIVE=$("${PY[@]}" -c 'import json,sys;print(json.load(open(sys.argv[1]))[0]["cid"])' "$W/j.json")
JOLD=$("${PY[@]}" -c 'import json,sys;print(json.load(open(sys.argv[1]))[2]["cid"])' "$W/j.json")
cnt() { psq "select count(*) from hydra_oauth2_$1 where client_id = '$2'"; }
[[ "$(cnt flow "$JLIVE")" -ge 1 && "$(cnt access "$JOLD")" -ge 1 && "$(cnt refresh "$JOLD")" -ge 1 && "$(cnt refresh "$JLIVE")" -ge 1 ]] || fail "17 the test rows are not there"
for t in access refresh; do psq "update hydra_oauth2_$t set requested_at = now() - interval '40 days', expires_at = now() - interval '39 days' where client_id = '$JOLD'" >/dev/null; done
psq "update hydra_oauth2_flow set requested_at = now() - interval '2 days' where client_id = '$JLIVE'" >/dev/null
TOTAL_BEFORE=$(psq "select (select count(*) from hydra_oauth2_access) + (select count(*) from hydra_oauth2_refresh) + (select count(*) from hydra_oauth2_flow)")
kubectl -n "$NS" delete job ambit-hydra-janitor-e2e --ignore-not-found >/dev/null
kubectl -n "$NS" create job ambit-hydra-janitor-e2e --from=cronjob/ambit-hydra-janitor >/dev/null
kubectl -n "$NS" wait --for=condition=complete job/ambit-hydra-janitor-e2e --timeout=300s >/dev/null 2>&1 || fail "17 the janitor job did not complete: $(kubectl -n "$NS" logs job/ambit-hydra-janitor-e2e --tail=5 2>&1 | head -c 400)"
kubectl -n "$NS" logs job/ambit-hydra-janitor-e2e > "$W/janitor.log" 2>&1
for what in 'access tokens' 'refresh tokens' 'login-consent requests' 'grants'; do grep -q "Successfully completed Janitor run on $what" "$W/janitor.log" || fail "17 the janitor log must say it completed $what: $(tail -c 300 "$W/janitor.log")"; done
! grep -q "$HP" "$W/janitor.log" || fail "17 the janitor log repeats the database password"
JT=$(kubectl -n "$NS" get job ambit-hydra-janitor-e2e -o jsonpath='{.status.startTime} {.status.completionTime}' | "${PY[@]}" -c 'import sys,datetime as d;a,b=sys.stdin.read().split();f=lambda x:d.datetime.strptime(x,"%Y-%m-%dT%H:%M:%SZ");print(int((f(b)-f(a)).total_seconds()))')
TOTAL_AFTER=$(psq "select (select count(*) from hydra_oauth2_access) + (select count(*) from hydra_oauth2_refresh) + (select count(*) from hydra_oauth2_flow)")
[[ "$(cnt access "$JOLD")" == 0 && "$(cnt refresh "$JOLD")" == 0 ]] || fail "17 the tokens that are 40 days old must be gone"
[[ "$(cnt refresh "$JLIVE")" -ge 1 && "$(cnt access "$JLIVE")" -ge 1 && "$(cnt flow "$JLIVE")" -ge 1 ]] || fail "17 the live grant lost rows to the janitor"
[[ "$(dbo refresh "$W/j.json" 0)" == "200 " ]] || fail "17 the live grant must still refresh after the janitor ran: $(dbo refresh "$W/j.json" 0)"
[[ "$(dbo refresh "$W/j.json" 2)" == "400 invalid_grant" ]] || fail "17 the grant with 40 day old tokens must be refused: $(dbo refresh "$W/j.json" 2)"
[[ "$(dbo refresh "$W/j.json" 1)" == "200 " ]] || fail "17 a young grant must still refresh after the janitor ran"
pass "17 the Hydra janitor (CronJob ambit-hydra-janitor, run once by hand): $TOTAL_BEFORE rows of tokens and records before, $TOTAL_AFTER after, in ${JT} s; the tokens that are 40 days old are gone; a live grant (with a two day old login and consent record) and a young grant still refresh; the log holds no password"

# Unused clients. ambit-auth deletes a client that was registered dynamically and not used for 30 days, when it starts and every 10 minutes: one that was
# used 31 days ago goes, one used 29 days ago stays, and a client that an admin made stays however old it is.
IDS=($(dbo dcr 2)); [[ ${#IDS[@]} -eq 2 ]] || fail "17 two registrations"
setused() { # setused ID DAYS: the last use of a client in Hydra, DAYS days ago
  [[ $(adm ambit-hydra-0 GET "/admin/clients/$1") == 200 ]] || fail "17 client $1 is not in Hydra"
  local body; body=$("${PY[@]}" -c 'import json,sys,datetime as d;c=json.load(open(sys.argv[1]));c.setdefault("metadata",{})["last_used"]=(d.datetime.now(d.timezone.utc)-d.timedelta(days=int(sys.argv[2]))).strftime("%Y-%m-%dT%H:%M:%SZ");print(json.dumps(c))' "$W/adm" "$2")
  [[ $(adm ambit-hydra-0 PUT "/admin/clients/$1" "$body") == 200 ]] || fail "17 could not change client $1: $(head -c 200 "$W/adm")"
}
setused "${IDS[0]}" 31; setused "${IDS[1]}" 29
[[ $(adm ambit-hydra-0 POST /admin/clients '{"client_id":"admin-made-e2e","redirect_uris":["https://chatgpt.com/cb"],"grant_types":["authorization_code"],"response_types":["code"],"token_endpoint_auth_method":"none","metadata":{"registered_via":"admin","last_used":"2020-01-01T00:00:00Z"}}') == 201 ]] || fail "17 could not make the admin client: $(head -c 200 "$W/adm")"
kubectl -n "$NS" rollout restart deploy/ambit-auth >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
wait_for 120 '[[ $(adm ambit-hydra-0 GET "/admin/clients/${IDS[0]}") == 404 ]]' || fail "17 a client that was not used for 31 days is still there after ambit-auth started"
[[ $(adm ambit-hydra-0 GET "/admin/clients/${IDS[1]}") == 200 ]] || fail "17 a client used 29 days ago was deleted"
[[ $(adm ambit-hydra-0 GET /admin/clients/admin-made-e2e) == 200 ]] || fail "17 a client that an admin made was deleted"
adm ambit-hydra-0 DELETE /admin/clients/admin-made-e2e >/dev/null; adm ambit-hydra-0 DELETE "/admin/clients/${IDS[1]}" >/dev/null
pass "17 ambit-auth deletes a dynamically registered client that was not used for 31 days, keeps one used 29 days ago and a client that an admin made (clean-up on start and every 10 minutes)"

# --- 13 · off again: everything of OAuth goes, the front door stays
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/frontdoor/values.yaml \
  --set frontDoor.rateLimit.perCredentialPerMinute=600 --set frontDoor.rateLimit.perSourcePerMinute=1000 --set frontDoor.rateLimit.perPodPerSecond=200 --wait --timeout 5m >/dev/null
wait_for 120 '[[ -z "$(kubectl -n "$NS" get sts ambit-hydra -o name 2>/dev/null)" ]]' || fail "13 Hydra is still there with oauth.enabled=false"
for o in service/ambit-hydra-public service/ambit-hydra-admin networkpolicy/ambit-hydra configmap/ambit-hydra httproute/ambit-auth-hydra httproute/ambit-auth-app deploy/ambit-auth; do
  [[ -z "$(kubectl -n "$NS" get "$o" -o name 2>/dev/null)" ]] || fail "13 $o is still there"
done
l=$(kubectl -n "$NS" get gateway ambit-public -o jsonpath='{.spec.listeners[*].name}'); [[ "$l" == https ]] || fail "13 the sign-in listener is still on the Gateway: $l"
pass "13 oauth.enabled=false removes Hydra, ambit-auth, their Services, policies, routes and the sign-in listener; the front door's own listener stays"
done_step oauth
