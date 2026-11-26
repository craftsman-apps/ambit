#!/usr/bin/env bash
# stopctl.sh stop|start|state AGENT, or rotate|keys (the signing keys): the console's own API as the bootstrap admin, for test/oauthtest/gateway.py (the production stop path:
# console, stop list, controller, gateway policy). Needs ADMIN_PW. stop and start print the HTTP status, state prints the agent's state.
# It talks to the console at BASE (test/lib.sh): this test cluster in CI only.
set -euo pipefail
cd "$(dirname "$0")/../.."
source test/lib.sh
: "${ADMIN_PW:?}"
sign_in local admin@ambit.local "$ADMIN_PW" >/dev/null || { echo "no-sign-in"; exit 1; }
case "$1" in
  stop) api POST "/api/agents/$2/stop" '{}'; echo ;;
  start) api POST "/api/agents/$2/start" '{}'; echo ;;
  state) api GET "/api/agents/$2" >/dev/null; python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("state",""))' "$W/api" ;;
  rotate) api POST /api/admin/oauth/signing-key/emergency-rotation '{"confirm":"every connection must connect again"}'; echo; cat "$W/api" ;;
  keys) api GET /api/admin/oauth/signing-key; echo; cat "$W/api" ;;
  *) echo "usage: stopctl.sh stop|start|state AGENT | rotate | keys" >&2; exit 2 ;;
esac
done_step stopctl >/dev/null
