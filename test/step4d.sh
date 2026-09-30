#!/usr/bin/env bash
# Step 4d acceptance: the Connect SSO page. An admin checks a provider, the console stores the client
# secret in a Secret and returns values; applying them adds a working sign-in option (decision C).
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
KC_ISSUER=http://keycloak.localtest.me:8080/realms/acme
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
cleanup() {
  helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null || true
  kubectl -n "$NS" delete secret ambit-sso-keycloak2 --ignore-not-found >/dev/null
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" delete secret ambit-sso-keycloak2 --ignore-not-found >/dev/null

# 1 · check: the right issuer passes and names the callback URL; a trailing slash is refused with the reason
sign_in keycloak priya priya-test-pass
[[ $(api POST /api/sso/check "{\"issuer\":\"$KC_ISSUER\"}") == 200 ]] || fail "1 check: $(cat "$W/api")"
[[ $(field 'd["redirectURI"]') == "$BASE/dex/callback" ]] || fail "1 redirect URI: $(cat "$W/api")"
[[ $(api POST /api/sso/check "{\"issuer\":\"$KC_ISSUER/\"}") == 400 && "$(cat "$W/api")" == *"trailing slash"* ]] || fail "1 trailing slash accepted: $(cat "$W/api")"
pass "1 issuer checked from inside the cluster; callback $BASE/dex/callback; '$KC_ISSUER/' refused: $(field 'd["error"]' | cut -c1-70)"

# 2 · only admins
sign_in keycloak alice alice-test-pass
[[ $(api POST /api/sso/check "{\"issuer\":\"$KC_ISSUER\"}") == 403 ]] || fail "2 viewer may check providers"
pass "2 alice (viewer) cannot use the page: 403"

# 3 · connect: the secret goes into a Secret and never comes back
SECRET=$(kubectl -n "$NS" get secret keycloak-dex-client -o jsonpath='{.data.secret}' | base64 -d)
sign_in keycloak priya priya-test-pass
body=$(uv run --no-project python -c 'import json,sys;print(json.dumps({"id":"keycloak2","name":"Acme (second connection)","issuer":sys.argv[1],"clientID":"ambit-dex","clientSecret":sys.argv[2],"groups":True}))' "$KC_ISSUER" "$SECRET")
[[ $(api POST /api/sso/connectors "$body") == 201 ]] || fail "3 connect: $(cat "$W/api")"
grep -qF "$SECRET" "$W/api" && fail "3 the response contains the client secret"
[[ $(kubectl -n "$NS" get secret ambit-sso-keycloak2 -o jsonpath='{.data.clientSecret}' | base64 -d) == "$SECRET" ]] || fail "3 Secret content"
kubectl -n "$NS" logs deploy/ambit-console --since=5m | grep -qF "$SECRET" && fail "3 the console logged the client secret"
field 'd["values"]' >"$W/snippet.yaml"
pass "3 client secret stored in ambit-sso-keycloak2; not in the response or the console log"

# 4 · apply the values as an admin would; Dex restarts and offers the new option; alice signs in through it
uv run --no-project --with pyyaml python test/keycloak/merge_values.py test/keycloak/values-sso.yaml "$W/snippet.yaml" "$W/values-4d.yaml"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f "$W/values-4d.yaml" --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-dex --timeout=180s >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null
for _ in $(seq 1 30); do [[ $(command curl -s "$BASE/api/login-options") == *keycloak2* ]] && break; sleep 2; done
[[ $(command curl -s "$BASE/api/login-options") == *'"id":"keycloak2"'* ]] || fail "4 login page does not offer keycloak2"
sign_in keycloak2 alice alice-test-pass || fail "4 sign-in through keycloak2 did not reach a form"
[[ $(me_status) == 200 && $(me_field email) == '"alice@acme.test"' ]] || fail "4 alice not signed in via keycloak2: $(cat "$W/me")"
pass "4 values applied; the login page offers 'Acme (second connection)'; alice signed in through it as $(me_field role)"
done_step 4d
