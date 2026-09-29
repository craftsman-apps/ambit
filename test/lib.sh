# Shared helpers for the step tests. Source after setting NS.
BASE=${BASE:-http://console.localtest.me:8080}
CURL_CA=${CURL_CA:-}   # set to a CA file when BASE is https with a private CA
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; exit 1; }

# hop METHOD URL [curl args]: one request with the shared cookie jar; sets CODE and LOC.
hop() {
  local m=$1 u=$2; shift 2
  curl -s ${CURL_CA:+--cacert "$CURL_CA"} -X "$m" -o "$W/body" -D "$W/hdr" -b "$W/jar" -c "$W/jar" "$@" "$u"
  CODE=$(awk 'NR==1{print $2}' "$W/hdr")
  LOC=$(awk 'tolower($1)=="location:"{print $2}' "$W/hdr" | tr -d '\r')
  if [[ -n "$LOC" && "$LOC" != http* ]]; then LOC="$(sed -E 's|^(https?://[^/]+).*|\1|' <<<"$u")$LOC"; fi
  cat "$W/hdr" >> "$W/all-headers"
}
# follow: keep following redirects until a non-redirect response.
follow() { local n=0; while [[ "$CODE" =~ ^30 && $n -lt 15 ]]; do hop GET "$LOC"; n=$((n+1)); done; }
# form_action: the first <form action> on the last page, made absolute.
form_action() {
  local a; a=$(grep -o 'action="[^"]*"' "$W/body" | head -1 | cut -d'"' -f2 | sed 's/&amp;/\&/g')
  [[ -z "$a" || "$a" == http* ]] && echo "$a" || echo "$BASE$a"
}
# sign_in CONNECTOR USER PASSWORD: Dex local form (login=) or Keycloak form (username=).
sign_in() {
  rm -f "$W/jar" "$W/all-headers"
  hop GET "$BASE/auth/login?connector=$1"; follow
  local action; action=$(form_action)
  [[ -n "$action" ]] || return 1
  if [[ "$1" == local ]]; then
    hop POST "$action" --data-urlencode "login=$2" --data-urlencode "password=$3"
  else
    hop POST "$action" --data-urlencode "username=$2" --data-urlencode "password=$3"
  fi
  follow
}
# token USER: an access token from the Keycloak test realm (password grant, client ambit-cli-test).
token() { command curl -s -d grant_type=password -d client_id=ambit-cli-test -d username="$1" -d password="$1-test-pass" \
  "http://keycloak.localtest.me:8080/realms/acme/protocol/openid-connect/token" | uv run --no-project python -c 'import json,sys;print(json.load(sys.stdin)["access_token"])'; }
me_status() { curl -s ${CURL_CA:+--cacert "$CURL_CA"} -o "$W/me" -w '%{http_code}' -b "$W/jar" "$BASE/api/me"; }

# ensure_llm_setup NS: Keycloak test IdP, SSO + LLM chart values, provider key Secret, test catalog.
# Every LLM test calls this, so each can run on its own after `make install`.
ensure_llm_setup() {
  local ns=$1 keyfile=${OPENROUTER_KEY_FILE:-$HOME/.config/ambit/openrouter-api-key}
  kubectl -n "$ns" apply -f test/keycloak/keycloak.yaml >/dev/null
  kubectl -n "$ns" rollout status deploy/keycloak --timeout=300s >/dev/null
  helm upgrade ambit charts/ambit -n "$ns" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
  HAVE_KEY=false
  if [[ -s "$keyfile" ]]; then
    kubectl -n "$ns" create secret generic ambit-llm-openrouter --from-file=Authorization="$keyfile" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    HAVE_KEY=true
  else
    kubectl -n "$ns" create secret generic ambit-llm-openrouter --from-literal=Authorization=no-key --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  fi
  kubectl -n "$ns" apply -f test/llm/objects.yaml >/dev/null
  kubectl -n "$ns" wait models.ambitproject.dev --all --for=condition=Ready --timeout=120s >/dev/null
}
me_field() { uv run --no-project python -c "import json,sys;v=json.load(open('$W/me')).get('$1');print(json.dumps(v))"; }
