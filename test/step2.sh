#!/usr/bin/env bash
# Step 2 acceptance: company SSO through Dex (Keycloak stands in for Okta), roles from groups,
# and turning the bootstrap admin off. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
KC=http://keycloak.localtest.me:8080
VALUES=(-f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml)
upgrade() {
  helm upgrade ambit charts/ambit -n "$NS" "${VALUES[@]}" "$@" --wait --timeout 5m >/dev/null
  kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null
  kubectl -n "$NS" rollout status deploy/ambit-dex --timeout=180s >/dev/null
}
kc_admin() { # kc_admin METHOD PATH: call the Keycloak admin API
  local tok; tok=$(curl -s -d grant_type=password -d client_id=admin-cli -d username=admin -d password=admin \
    "$KC/realms/master/protocol/openid-connect/token" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  curl -s -X "$1" -H "Authorization: Bearer $tok" "$KC/admin/realms/acme$2"
}
signed_in_as() { # signed_in_as USER PASSWORD ROLE
  sign_in keycloak "$1" "$2" || fail "$1: Keycloak login form not reached"
  [[ $(me_status) == 200 ]] || fail "$1: no session after SSO: $(cat "$W/me")"
  [[ $(me_field role) == "\"$3\"" && $(me_field bootstrap) == false && $(me_field email) == "\"$1@acme.test\"" ]] \
    || fail "$1: expected role $3, got $(cat "$W/me")"
}

# Setup: test IdP, then connect Ambit to it.
kubectl -n "$NS" apply -f test/keycloak/keycloak.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/keycloak --timeout=300s >/dev/null
upgrade --set bootstrapAdmin.enabled=true

# 1 · the login page offers SSO and the bootstrap admin
o=$(curl -s "$BASE/api/login-options")
[[ "$o" == *'"id":"keycloak"'* && "$o" == *'"bootstrap":true'* ]] || fail "1 login options: $o"
pass "1 login page offers Acme (Keycloak) and the bootstrap admin"

# 2-4 · roles follow IdP groups
signed_in_as priya priya-test-pass admin;    pass "2 priya (ambit-admins) is admin through SSO"
signed_in_as sam sam-test-pass approver;     pass "3 sam (ambit-approvers) is approver"
signed_in_as alice alice-test-pass viewer;   pass "4 alice (eng) is viewer"

# 5 · a group change in the IdP changes the role at the next sign-in
uid=$(kc_admin GET "/users?username=priya&exact=true" | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["id"])')
gid=$(kc_admin GET "/groups?search=ambit-admins" | python3 -c 'import json,sys;print([g["id"] for g in json.load(sys.stdin) if g["name"]=="ambit-admins"][0])')
kc_admin DELETE "/users/$uid/groups/$gid" >/dev/null
signed_in_as priya priya-test-pass viewer
kc_admin PUT "/users/$uid/groups/$gid" >/dev/null
signed_in_as priya priya-test-pass admin
pass "5 removing priya from ambit-admins in Keycloak makes her viewer at next sign-in; adding back restores admin"

# 6 · turn the bootstrap admin off
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
upgrade --set bootstrapAdmin.enabled=false
o=$(curl -s "$BASE/api/login-options")
[[ "$o" == *'"bootstrap":false'* ]] || fail "6 login options still offer bootstrap: $o"
kubectl -n "$NS" get secret ambit-initial-admin >/dev/null 2>&1 && fail "6 bootstrap password Secret still exists"
sign_in local admin@ambit.local "$PW" || true
[[ $(me_status) == 401 ]] || fail "6 bootstrap admin can still sign in"
signed_in_as priya priya-test-pass admin
pass "6 bootstrap admin off: no password Secret, local sign-in refused, SSO still works"

# 7 · no secret value in the chart output; renders identically
helm template ambit charts/ambit -n "$NS" "${VALUES[@]}" --set bootstrapAdmin.enabled=false > "$W/r1"
helm template ambit charts/ambit -n "$NS" "${VALUES[@]}" --set bootstrapAdmin.enabled=false > "$W/r2"
cmp -s "$W/r1" "$W/r2" || fail "7 two renders differ"
grep -q 'keycloak-test-client-secret' "$W/r1" && fail "7 client secret value appears in the chart output"
grep -q 'clientSecret: \$KEYCLOAK_CLIENT_SECRET' "$W/r1" || fail "7 Dex config does not reference the secret by env var"
pass "7 chart renders identically; the IdP client secret is referenced, never rendered"

# Leave the cluster as step 1 expects it: bootstrap admin on.
upgrade --set bootstrapAdmin.enabled=true
echo "PASS: step 2"
