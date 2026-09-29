#!/usr/bin/env bash
# Step 1 acceptance: console sign-in with the bootstrap admin through Dex. Run after `make install`.
set -euo pipefail
NS=${1:-ambit-system}
BASE=http://console.localtest.me:8080
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; exit 1; }

# hop METHOD URL [curl args]: one request with the shared cookie jar; sets CODE and LOC.
hop() {
  local m=$1 u=$2; shift 2
  curl -s -X "$m" -o "$W/body" -D "$W/hdr" -b "$W/jar" -c "$W/jar" "$@" "$u"
  CODE=$(awk 'NR==1{print $2}' "$W/hdr")
  LOC=$(awk 'tolower($1)=="location:"{print $2}' "$W/hdr" | tr -d '\r')
  [[ -z "$LOC" || "$LOC" == http* ]] || LOC="$BASE$LOC"
  cat "$W/hdr" >> "$W/all-headers"
}
# follow: keep following redirects from the last hop until a non-redirect response.
follow() { local n=0; while [[ "$CODE" =~ ^30 && $n -lt 12 ]]; do hop GET "$LOC"; n=$((n+1)); done; }

# sign_in PASSWORD: start at /auth/login, submit Dex's local login form, follow back to the console.
sign_in() {
  rm -f "$W/jar" "$W/all-headers"
  hop GET "$BASE/auth/login?connector=local"; follow
  local action; action=$(grep -o 'action="[^"]*"' "$W/body" | head -1 | cut -d'"' -f2 | sed 's/&amp;/\&/g')
  [[ -n "$action" ]] || fail "Dex login form not found"
  [[ "$action" == http* ]] || action="$BASE$action"
  hop POST "$action" --data-urlencode "login=admin@ambit.local" --data-urlencode "password=$1"; follow
}
me_status() { curl -s -o "$W/me" -w '%{http_code}' -b "$W/jar" "$BASE/api/me"; }

# 1 · every pod Ready (make install already waited up to 5 minutes)
kubectl -n "$NS" wait deploy --all --for=condition=Available --timeout=180s >/dev/null && pass "1 pods ready"

# 2 · no session: pages redirect to /login, API answers 401
r=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/")
[[ "$r" == "302 $BASE/login" ]] || fail "2 GET / without session gave: $r"
[[ $(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/me") == 401 ]] || fail "2 /api/me without session"
pass "2 unauthenticated: 302 /login, API 401"

# 3 · bootstrap admin signs in
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
[[ ${#PW} -eq 24 ]] || fail "3 bootstrap password missing"
sign_in "$PW"
[[ $(me_status) == 200 ]] || fail "3 /api/me after sign-in: $(cat "$W/me")"
grep -q '"email":"admin@ambit.local"' "$W/me" && grep -q '"role":"admin"' "$W/me" && grep -q '"bootstrap":true' "$W/me" \
  || fail "3 wrong identity: $(cat "$W/me")"
pass "3 signed in as admin (bootstrap)"

# 7 · session cookie flags (checked on the Set-Cookie header from this sign-in)
ck=$(grep -i '^set-cookie: ambit_session=' "$W/all-headers" | tail -1)
[[ "$ck" == *HttpOnly* && "$ck" == *"SameSite=Lax"* ]] || fail "7 cookie flags: $ck"
pass "7 session cookie HttpOnly, SameSite=Lax (Secure is added when the URL is https)"

# 6 · sign out, then Back: the page must not come back
r=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -X POST -b "$W/jar" -c "$W/jar" "$BASE/auth/logout")
[[ "$r" == "303 $BASE/login" ]] || fail "6 logout gave: $r"
[[ $(me_status) == 401 ]] || fail "6 /api/me still works after logout"
[[ $(curl -s -o /dev/null -w '%{http_code}' -b "$W/jar" "$BASE/") == 302 ]] || fail "6 dashboard after logout"
pass "6 sign-out ends the session"

# 4 · wrong password: error, no session
sign_in "wrong-password-$RANDOM"
grep -qi 'invalid' "$W/body" || fail "4 no error message shown"
grep -q 'ambit_session' "$W/jar" && fail "4 session cookie set for a wrong password"
pass "4 wrong password rejected, no session"

# 5 · helm upgrade keeps the password
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout restart deploy/ambit-console >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=120s >/dev/null
sign_in "$PW"
[[ $(me_status) == 200 ]] || fail "5 old password rejected after upgrade and console restart"
pass "5 password unchanged after helm upgrade and console restart"

# 8 · the chart renders identically (GitOps rule 6)
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml > "$W/r1"
helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml > "$W/r2"
cmp -s "$W/r1" "$W/r2" || fail "8 two renders differ"
pass "8 chart renders identically"
echo "PASS: step 1"
