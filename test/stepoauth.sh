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
  kubectl -n "$NS" delete configmap ambit-offboarded ambit-agent-stops --ignore-not-found >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit "$rc"' EXIT
rnd() { openssl rand -hex "${1:-16}"; }
oaset() { helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/frontdoor/values.yaml -f test/oauth/values.yaml \
  --set frontDoor.rateLimit.perCredentialPerMinute=600 --set frontDoor.rateLimit.perSourcePerMinute=1000 --set frontDoor.rateLimit.perPodPerSecond=200 \
  --set "oauth.signIn.issuer=https://ambit-oauth-idp.$NS.svc" --set "oauth.auth.consoleURL=http://ambit-oauth-console.$NS.svc:8085" "$@" >/dev/null; }
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
  local v=$1 role=${2:-hydra} pw=${3:-$HP} db=${4:-hydra} host=${5:-.$NS.svc} extra=${6:-sslmode=verify-full&sslrootcert=/ca/ca.crt&max_conns=8&max_idle_conns=4}
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
[[ $(reach np-auth "$HADM") == 200 && $(reach np-auth "$HPUB") == no ]] || fail "3 ambit-auth must reach 4445 and not 4444"
[[ $(reach np-console "$HADM") == 200 && $(reach np-console "$HPUB") == no ]] || fail "3 the console must reach 4445 and not 4444"
kubectl -n "$NS" delete pod -l "$L" --wait=false >/dev/null
pass "3 Hydra's public port is open to the public Gateway and the controller only; the admin port to ambit-auth and the console only"

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
want wrongca refused 'x509' "$(dsn 17)" 2 ambit-oauth-wrong-ca
want wrongname refused 'certificate is valid for' "$(dsn 17 hydra "$HP" hydra '')"          # the short Service name is not in the certificate
want superuser refused 'is a superuser' "$(dsn 17 postgres "$SU")"
want limit refused 'CONNECTION LIMIT' "$(dsn 17 hydra_small "$SP" hydra_small)" 2
want limitok ok 'database ok' "$(dsn 17 hydra_small "$SP" hydra_small)" 1
want badpw refused 'password authentication failed' "$(dsn 17 hydra WRONG$HP)"
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

# --- 6 · migrations: two runs at the same time, and a repeat
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
done_pod() { wait_for 120 "[[ \$(kubectl -n '$NS' get pod $1 -o jsonpath='{.status.phase}') == Succeeded || \$(kubectl -n '$NS' get pod $1 -o jsonpath='{.status.phase}') == Failed ]]" || fail "6 $1 did not finish"; [[ $(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.phase}') == Succeeded ]] || fail "6 $1 failed: $(kubectl -n "$NS" logs "$1" | tail -c 400)"; }
mig mig-a; mig mig-b
done_pod mig-a; done_pod mig-b
mig mig-c; done_pod mig-c
pass "6 two migrations of an empty PostgreSQL 15 database at the same time both succeed, and a third run changes nothing"

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
  kubectl -n "$NS" get configmap,statefulset,pod -o yaml
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
flow() { GW_PORT=$PA IDP_PORT=$IDPP CA_FILE="$W/allca.pem" AUTH_HOST=$AUTH MCP_HOST=$PUB IDP_HOST=$IDPH CONSOLE_URL="http://127.0.0.1:$CONP" KUBECTL="kubectl -n $NS" \
  "${PY[@]}" test/oauthtest/flow.py "$@" || fail "12 the sign-in checks failed (see FAIL lines above)"; }
# the registration limit first, at its default of 10 per hour and address (the counter is per hour: nothing registered before)
flow limits
# then the limit is raised, so that the many registrations of the other checks pass
oaset --set oauth.registration.dynamic.perSourcePerHour=1000 --wait --timeout 8m
kubectl -n "$NS" rollout status deploy/ambit-auth --timeout=300s >/dev/null
flow
pass "12 the sign-in flow and its refusals"

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
