#!/usr/bin/env bash
# Step 3c acceptance: hardening found in the post-3b review. Runs on its own after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM_VALUES=(-f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml)
ensure_llm_setup "$NS"
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null

# 1 · no CRD is lost on helm uninstall: Ambit's own are kept; third-party ones are not owned by Helm at all
bad=$(kubectl get crd -o json | uv run --no-project python -c '
import json,sys
out=[]
for c in json.load(sys.stdin)["items"]:
    g=c["spec"]["group"]; md=c["metadata"]
    helm_owned=md.get("labels",{}).get("app.kubernetes.io/managed-by")=="Helm"
    keep=md.get("annotations",{}).get("helm.sh/resource-policy")=="keep"
    if g=="ambitproject.dev" and not keep: out.append(md["name"]+"(not kept)")
    if g in ("agentgateway.dev","gateway.networking.k8s.io") and helm_owned: out.append(md["name"]+"(Helm-owned)")
print(" ".join(out))')
[[ -z "$bad" ]] || fail "1 CRDs that helm uninstall would delete: $bad"
pass "1 Ambit CRDs carry keep; Gateway API and agentgateway CRDs are server-side applied, not owned by Helm"

# 2 · security headers on pages and API
for p in /login /api/me; do
  h=$(curl -s -D - -o /dev/null "$BASE$p")
  grep -qi "content-security-policy: default-src 'self'; script-src 'self'" <<<"$h" && grep -qi 'x-frame-options: DENY' <<<"$h" \
    && grep -qi 'x-content-type-options: nosniff' <<<"$h" || fail "2 $p headers: $h"
done
pass "2 CSP (no inline script, frame-ancestors none), X-Frame-Options, nosniff on pages and API"

# 3 · sign-out ends the session even for a copied cookie
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
sign_in local admin@ambit.local "$PW"
[[ $(me_status) == 200 ]] || fail "3 sign-in"
cp "$W/jar" "$W/stolen"
curl -s -o /dev/null -X POST -b "$W/jar" -c "$W/jar" "$BASE/auth/logout"
[[ $(curl -s -o /dev/null -w '%{http_code}' -b "$W/stolen" "$BASE/api/me") == 401 ]] || fail "3 a cookie copied before sign-out still works"
pass "3 cookie copied before sign-out -> 401 afterwards"

# 4 · edits record who made them
sign_in local admin@ambit.local "$PW"
c=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -b "$W/jar" -H 'Content-Type: application/json' \
  -d '{"allSignedIn":true,"models":["gemma-4-31b"]}' "$BASE/api/access-rules/everyone-gemma")
[[ "$c" == 200 ]] || fail "4 update: $c"
[[ $(kubectl -n "$NS" get "accessrules.$G" everyone-gemma -o jsonpath='{.metadata.annotations.ambitproject\.dev/updated-by}') == admin@ambit.local ]] \
  || fail "4 updated-by not recorded"
pass "4 console edit records updated-by=admin@ambit.local"

# 5 · a Model cannot use an arbitrary Secret as provider credentials
if out=$(kubectl -n "$NS" apply -f - 2>&1 <<YAML
apiVersion: $G/v1alpha1
kind: Model
metadata: { name: exfil }
spec: { provider: Openrouter, model: x/y:free, credentialsRef: { name: ambit-console-keys } }
YAML
); then kubectl -n "$NS" delete "models.$G" exfil >/dev/null; fail "5 Model pointing at ambit-console-keys was accepted"; fi
grep -q 'ambit-llm-' <<<"$out" || fail "5 rejected without the reason: $out"
pass "5 Model naming ambit-console-keys rejected by the API server: credentials must be ambit-llm-*"

# 6 · HTTPS with a private CA: redirect, HSTS, Secure cookie, LLM route on https
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=Ambit test CA" -keyout "$W/ca.key" -out "$W/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=console.localtest.me" -keyout "$W/tls.key" -out "$W/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:console.localtest.me,DNS:llm.localtest.me\n' > "$W/san.ext"
openssl x509 -req -in "$W/tls.csr" -CA "$W/ca.crt" -CAkey "$W/ca.key" -CAcreateserial -days 1 -extfile "$W/san.ext" -out "$W/tls.crt" 2>/dev/null
kubectl -n "$NS" create secret tls ambit-tls --cert="$W/tls.crt" --key="$W/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic ambit-test-ca --from-file=ca.crt="$W/ca.crt" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade ambit charts/ambit -n "$NS" "${LLM_VALUES[@]}" -f test/tls/values-tls.yaml --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-dex --timeout=180s >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null
restore() { helm upgrade ambit charts/ambit -n "$NS" "${LLM_VALUES[@]}" --wait --timeout 5m >/dev/null; kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null; }
trap 'rc=$?; restore; on_exit $rc' EXIT
HTTPS=https://console.localtest.me:8443
r=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' http://console.localtest.me:8080/login)
[[ "$r" == "301 $HTTPS/login" ]] || fail "6 http did not redirect to https: $r"
h=$(curl -s --cacert "$W/ca.crt" -D - -o /dev/null "$HTTPS/login")
grep -qi '^HTTP/[0-9.]* 200' <<<"$h" && grep -qi 'strict-transport-security: max-age=31536000' <<<"$h" || fail "6 https /login: $h"
BASE=$HTTPS CURL_CA="$W/ca.crt"
sign_in local admin@ambit.local "$PW" || fail "6 sign-in over https"
ck=$(grep -i '^set-cookie: ambit_session=' "$W/all-headers" | tail -1)
[[ "$ck" == *Secure* && "$ck" == *HttpOnly* ]] || fail "6 session cookie over https lacks Secure: $ck"
[[ $(me_status) == 200 ]] || fail "6 /api/me over https"
tok=$(command curl -s -d grant_type=password -d client_id=ambit-cli-test -d username=alice -d password=alice-test-pass \
  http://keycloak.localtest.me:8080/realms/acme/protocol/openid-connect/token | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
c=$(curl -s --cacert "$W/ca.crt" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"x"}]}' https://llm.localtest.me:8443/v1/chat/completions)
[[ "$c" == 403 ]] || fail "6 LLM route over https answered $c (want 403 for alice on the restricted model)"
pass "6 HTTPS: http->https 301, HSTS, Secure cookie, Dex sign-in over https, LLM route policy on https"
done_step 3c
