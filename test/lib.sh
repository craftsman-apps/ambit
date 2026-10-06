# Shared helpers for the step tests. Source after setting NS.
BASE=${BASE:-http://console.localtest.me:8080}
CURL_CA=${CURL_CA:-}   # set to a CA file when BASE is https with a private CA
W=$(mktemp -d)
# A step passes only if it reaches done_step. macOS's bash 3.2 exits 0 from an EXIT trap after an
# unset-variable error, which would turn a failed test green; this catches that on every bash.
FINISHED=
done_step() { FINISHED=1; echo "PASS: step $1"; }
on_exit() {
  local rc=$1
  if [[ -z "$FINISHED" && $rc -eq 0 ]]; then echo "FAIL: the script stopped before its last check"; rc=1; fi
  rm -rf "$W"; exit "$rc"
}
trap 'on_exit $?' EXIT
# Upgrade checks (test/upgrade.sh) run the steps against a candidate version: AMBIT_CHART is a copy of
# the chart with candidate subcharts, AMBIT_EXTRA_VALUES a values file with candidate images. Every
# "helm upgrade ambit charts/ambit" in the steps goes through here; unset, nothing changes.
helm() {
  if [[ "${1:-}" == upgrade && "${3:-}" == charts/ambit ]]; then
    local args=("$@")
    args[2]=${AMBIT_CHART:-charts/ambit}
    command helm "${args[@]}" ${AMBIT_EXTRA_VALUES:+-f "$AMBIT_EXTRA_VALUES"}
  else
    command helm "$@"
  fi
}
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; [[ -t 1 ]] || echo "FAIL $1" >&2; exit 1; }  # also stderr: stdout may be captured

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
# api METHOD PATH [JSON]: console API call with the signed-in cookie jar; body in $W/api
api() {
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -b "$W/jar")
  [[ $# -ge 3 ]] && a+=(-H 'Content-Type: application/json' -d "$3")
  command curl "${a[@]}" "$BASE$2"
}
# reached_provider CODE: the gateway allowed an LLM request and OpenRouter answered. 200, or OpenRouter's own
# free-tier 429 ("rate-limited upstream"): both prove the request passed Ambit's checks. Sets VIA for the log.
reached_provider() {
  VIA="$1 from OpenRouter"
  [[ "$1" == 200 ]] && return 0
  if [[ "$1" == 429 ]] && grep -q '"code":429' "$W/llm" && grep -qi 'rate-limited upstream\|Provider returned error' "$W/llm"; then
    VIA="429 from OpenRouter (free-tier limit; the gateway allowed the request)"; return 0
  fi
  return 1
}
me_status() { curl -s ${CURL_CA:+--cacert "$CURL_CA"} -o "$W/me" -w '%{http_code}' -b "$W/jar" "$BASE/api/me"; }

# rlconfig NS: the limits envoy ratelimit has actually loaded, from its debug endpoint (port 6070) through the
# API server's pod proxy. Its image has no shell tools, so the mounted ConfigMap cannot be read with exec.
# Lines look like "ambit-llm.spend_group_eng: unit=DAY requests_per_unit=5, shadow_mode: false, ...".
rlconfig() {
  local pod; pod=$(kubectl -n "$1" get pod -l app.kubernetes.io/name=ambit-ratelimit --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [[ -n "$pod" ]] && kubectl get --raw "/api/v1/namespaces/$1/pods/$pod:6070/proxy/rlconfig" 2>/dev/null
}
# wait_rl NS TEXT: wait until the loaded limits contain TEXT; fail after 3 minutes (ConfigMaps reach pods in up to ~2)
wait_rl() {
  for _ in $(seq 1 60); do [[ "$(rlconfig "$1")" == *"$2"* ]] && return 0; sleep 3; done
  fail "ratelimit did not load '$2' (loaded: $(rlconfig "$1" | head -5 | tr '\n' ' '))"
}
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
  # A fake OpenAI-compatible model with a price (test/llm/mock.yaml), for token counts and cost.
  kubectl -n "$ns" create secret generic ambit-llm-mock --from-literal=Authorization=not-used --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$ns" apply -f test/llm/mock.yaml >/dev/null
  kubectl -n "$ns" rollout restart deploy/mock-llm >/dev/null
  kubectl -n "$ns" rollout status deploy/mock-llm --timeout=180s >/dev/null
  kubectl -n "$ns" wait models.ambitproject.dev --all --for=condition=Ready --timeout=120s >/dev/null
}
me_field() { uv run --no-project python -c "import json,sys;v=json.load(open('$W/me')).get('$1');print(json.dumps(v))"; }

# MCP through the gateway.
MCP=${MCP:-http://mcp.localtest.me:8080/mcp}
TOOLS=""
# mcp_open TOKEN: starts an MCP session; sets TOK and SID.
mcp_open() {
  TOK=$1
  command curl -s -D "$W/mh" -o /dev/null -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"step4a","version":"1"}}}' "$MCP"
  SID=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/mh" | tr -d '\r')
  [[ -n "$SID" ]] || fail "no MCP session: $(head -1 "$W/mh")"
  mcp_send '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
}
# mcp_send JSON: one request in the open session; prints the JSON-RPC response (SSE or plain JSON).
mcp_send() {
  command curl -s -H "Authorization: Bearer $TOK" -H "Mcp-Session-Id: $SID" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -d "$1" "$MCP" | awk '/^data: /{print substr($0, 7); next} /^\{/{print}'
}
# tools TOKEN: opens a session and sets TOOLS to the sorted tool names this user sees, space separated.
tools() {
  mcp_open "$1"
  TOOLS=$(mcp_send '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' |
    uv run --no-project python -c 'import json,sys;print(" ".join(sorted(t["name"] for t in json.loads(sys.stdin.read())["result"]["tools"])))')
}
# try_tools TOKEN: like tools, but an error (e.g. a 500 while the gateway reloads) sets TOOLS to "<error>"
# instead of ending the test; for use inside wait_for.
try_tools() { TOOLS=$( (tools "$1" >/dev/null 2>&1 && echo "$TOOLS") ) || TOOLS="<error>"; }
# call NAME ARGS: calls a tool in the open session; prints "ok <text>" or "error <message>".
call() {
  mcp_send "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}" |
    uv run --no-project python -c 'import json,sys
d=json.loads(sys.stdin.read())
print("error "+d["error"]["message"] if "error" in d else "ok "+d["result"]["content"][0]["text"].replace("\n"," "))'
}

# wait_for SECS CMD: run CMD (eval) every 2 s until it succeeds; non-zero when SECS pass first.
wait_for() {
  local end=$((SECONDS + $1))
  while ! eval "$2" >/dev/null 2>&1; do (( SECONDS < end )) || return 1; sleep 2; done
}
# connect_app NAME USER PASS: the browser flow for Connection NAME — console -> SaaS login -> console callback.
connect_app() {
  hop GET "$BASE/connections/$1/connect"; follow
  local action; action=$(form_action)
  [[ -n "$action" ]] || fail "no saas login form for $1 (HTTP $CODE)"
  hop POST "$action" --data-urlencode "username=$2" --data-urlencode "password=$3"; follow
  grep -q "connected=$1" "$W/all-headers" || fail "connect $1 for $2 did not return to /connections?connected=$1"
}
# pod_curl LABELS URL [CURL ARGS...]: HTTP status of URL seen from a short-lived pod with LABELS (comma separated,
# e.g. "app.kubernetes.io/name=ambit-controller"); always 3 characters, 000 = no connection. Needs NS, CURL_IMAGE.
pod_curl() {
  local out labels=$1 url=$2; shift 2
  out=$(kubectl -n "$NS" run "probe-$RANDOM" --rm -i --restart=Never --quiet --image="$CURL_IMAGE" --labels="$labels" --command -- \
    curl -s -m 3 -o /dev/null -w '%{http_code}' "$@" "$url" 2>/dev/null) || true   # curl prints 000 itself on failure
  out=${out: -3}
  [[ $out =~ ^[0-9]{3}$ ]] && echo "$out" || echo 000
}
