# Helpers of the public front door's tests (test/stepfd.sh, test/stepplatforms.sh). Source after test/lib.sh. The caller sets
# NS, W (from test/lib.sh), PUB (the public host name), PA and PB (local ports of two public gateway pods, see `forward`),
# and calls stop_forwards when it ends (the port-forward processes are in $W/forward.pids).
PY=(uv run --no-project python)
past() { date -u -v-1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ; }
# one local port per gateway pod
forward() { # forward POD -> prints the local port. kubectl port-forward ends when its connection idles for a while or a connection
  # to it is refused, so a small loop starts it again. Its process id goes to a file: this runs in a command substitution.
  local port; port=$("${PY[@]}" -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
  bash -c 'while :; do kubectl -n "$0" port-forward "pod/$1" "$2:8443" >/dev/null 2>&1; sleep 0.3; done' "$NS" "$1" "$port" >/dev/null 2>&1 &
  echo $! >> "$W/forward.pids"
  for _ in $(seq 1 30); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.5; done
  echo "$port"
}
stop_forwards() { # each id is a loop: note its kubectl, stop the loop (or it starts another), then stop the kubectl
  local p c kids; for p in $(cat "$W/forward.pids" 2>/dev/null); do
    kids=$(pgrep -P "$p" 2>/dev/null || true)
    kill "$p" 2>/dev/null || true
    for c in $kids; do kill "$c" 2>/dev/null || true; done
  done
  : > "$W/forward.pids"
}
# public_pods: PODS = the running public gateway pods that are not being deleted (an old pod of a rollout is still "Running")
public_pods() {
  PODS=($(kubectl -n "$NS" get pods -l gateway.networking.k8s.io/gateway-name=ambit-public -o json | "${PY[@]}" -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") == "Running" and not p["metadata"].get("deletionTimestamp") and all(c.get("ready") for c in p["status"].get("containerStatuses", [])):
        print(p["metadata"]["name"])'))
}
# reforward: new forwards to the running public gateway pods; sets PODS, PA and PB
reforward() {
  stop_forwards
  public_pods
  PA=$(forward "${PODS[0]}"); PB=$(forward "${PODS[${#PODS[@]}-1]}")
}
pub() { # pub PORT PATH [curl args]: HTTP status of one request to the public Gateway; body in $W/pub, headers in $W/pubh
  local port=$1 path=$2; shift 2
  local c try
  for try in 1 2; do # 000: the port-forward was just restarted
    c=$(command curl -sS -o "${PUBOUT:-$W/pub}" -D "${PUBHDR:-$W/pubh}" -w '%{http_code}' --max-time 15 --cacert "$W/ca.pem" --resolve "$PUB:$port:127.0.0.1" "$@" "https://$PUB:$port$path" 2>"$W/pub.err" || true)
    [[ "$c" != 000 ]] && break
    sleep 1
  done
  echo "$c"
}
MCPH=(-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stepfd","version":"1"}}}'
pinit() { # pinit PORT [curl args]: opens an MCP session on the public Gateway; sets PCODE (the status) and PSID. No subshell: PSID must survive.
  local port=$1; shift
  PCODE=$(pub "$port" /mcp -X POST "${MCPH[@]}" -d "$INIT" "$@")
  PSID=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$W/pubh" | tr -d '\r')
}
psend() { # psend PORT JSON [curl args]: one request in the open session; prints the status; the answer's JSON is in $W/pubj
  local port=$1 json=$2; shift 2
  local c; c=$(pub "$port" /mcp -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Mcp-Session-Id: $PSID" -d "$json" "$@")
  awk '/^data: /{print substr($0, 7); next} /^\{/{print}' "$W/pub" > "$W/pubj"
  echo "$c"
}
ptools() { # ptools PORT [curl args]: the tool names of a new session, space separated; "<status>" when it cannot be opened
  pinit "$@"
  local c=$PCODE
  [[ "$c" == 200 && -n "$PSID" ]] || { echo "<$c>"; return; }
  psend "$1" '{"jsonrpc":"2.0","method":"notifications/initialized"}' "${@:2}" >/dev/null
  c=$(psend "$1" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' "${@:2}")
  [[ "$c" == 200 ]] || { echo "<$c>"; return; }
  "${PY[@]}" -c 'import json,sys;print(" ".join(sorted(t["name"] for t in json.load(open(sys.argv[1]))["result"]["tools"])))' "$W/pubj"
}
# pubk PORT PATH [curl args]: one request, classified: ok | limited | refused | other:<status>. A limit answers 429, or (agentgateway
# maps an error of an MCP request to a JSON-RPC error) 200 with an "error" in the body.
pubk() {
  local out c; out=$(mktemp "$W/pk.XXXXXX")
  c=$(PUBOUT=$out PUBHDR=/dev/null pub "$@")
  case $c in
    429) echo limited ;;
    403) echo refused ;;
    200) if grep -qi 'rate limit' "$out"; then echo limited; elif grep -q '"error"' "$out"; then echo "other:200-error"; else echo ok; fi ;;
    *) echo "other:$c" ;;
  esac
  rm -f "$out"
}
