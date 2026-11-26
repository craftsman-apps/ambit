#!/usr/bin/env bash
# Tests for wait-pr.sh with a fake `gh` that prints canned JSON (the case is in $CASE).
set -uo pipefail
cd "$(dirname "$0")"
fails=0
bin=$(mktemp -d); trap 'rm -rf "$bin"' EXIT
cat >"$bin/gh" <<'GH'
#!/usr/bin/env bash
if [ "$1" = api ] && [ "$2" = graphql ]; then
  [ "$CASE" = error ] && exit 1
  [ "$CASE" = removed ] || [ "$CASE" = conflict ] && echo false || echo true; exit 0
fi
if [ "$1" = api ]; then
  echo '[{"event":"removed_from_merge_queue","reason":"FAILED_CHECKS"}]'; exit 0
fi
q='"autoMergeRequest":null,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"'
run='{"name":"e2e (f)","workflowName":"ci","status":"COMPLETED","conclusion":"%s","detailsUrl":"https://github.com/o/r/actions/runs/1"}'
case $CASE in
  merged) echo '{"state":"MERGED","mergedAt":"2026-11-26T00:00:00Z","statusCheckRollup":[]}' ;;
  closed) echo '{"state":"CLOSED","mergedAt":null,"statusCheckRollup":[]}' ;;
  removed) echo '{"state":"OPEN","mergedAt":null,"autoMergeRequest":null,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[]}' ;;
  conflict) echo '{"state":"OPEN","mergedAt":null,"autoMergeRequest":{},"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","statusCheckRollup":[]}' ;;
  failed) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[$(printf "$run" FAILURE)]}" ;;
  pending) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[{\"name\":\"build\",\"status\":\"IN_PROGRESS\",\"conclusion\":\"\"}]}" ;;
  action_required) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[$(printf "$run" ACTION_REQUIRED)]}" ;;
  expected) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[{\"context\":\"ci\",\"state\":\"EXPECTED\"}]}" ;;
  superseded) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[{\"name\":\"build\",\"workflowName\":\"ci\",\"status\":\"COMPLETED\",\"conclusion\":\"CANCELLED\",\"detailsUrl\":\"https://github.com/o/r/actions/runs/1/job/9\"},{\"name\":\"build\",\"workflowName\":\"ci\",\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\",\"detailsUrl\":\"https://github.com/o/r/actions/runs/2/job/9\"}]}" ;;
  green) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[$(printf "$run" SUCCESS)]}" ;;
  error) echo "boom" >&2; exit 1 ;;
esac
GH
chmod +x "$bin/gh"

check() { # check <want rc> <want text> <case> [wait-pr args]
  local want=$1 text=$2 case=$3; shift 3
  local out rc=0
  out=$(CASE=$case PATH="$bin:$PATH" ./wait-pr.sh 7 --interval 1 "$@" 2>&1) || rc=$?
  if [ "$rc" = "$want" ] && [[ $out == *"$text"* ]]; then echo "ok   $case $*"; else echo "FAIL $case $*: want $want '$text', got $rc '$out'"; fails=$((fails + 1)); fi
}

check 0 "MERGED #7" merged --timeout 5s
check 2 "CLOSED #7" closed --timeout 5s
check 3 "removed_from_merge_queue: FAILED_CHECKS" removed --timeout 5s
check 4 "CONFLICT #7" conflict --timeout 5s
check 5 "e2e (f) (ci) https://github.com/o/r/actions/runs/1" failed --timeout 5s
check 6 "TIMEOUT #7" pending --timeout 2s
check 6 "TIMEOUT #7" green --timeout 2s
check 0 "CHECKS GREEN #7" green --checks --timeout 5s
check 5 "CHECK FAILED #7" failed --checks --timeout 5s
check 6 "TIMEOUT #7" pending --checks --timeout 2s
check 1 "gh failed 5 times" error --timeout 20s
check 1 "usage" merged --bogus
check 1 "usage" merged --interval 5x
check 6 "TIMEOUT #7" pending --timeout 1s --interval 60   # a short timeout is not stretched by a long interval
check 6 "TIMEOUT #7" pending --timeout 2s --interval 08   # a leading zero is decimal
check 0 "CHECKS GREEN #7" superseded --checks --timeout 5s # a job of an older, cancelled run is not a failure
check 5 "CHECK FAILED #7" action_required --timeout 5s
check 6 "TIMEOUT #7" expected --checks --timeout 2s       # a status that is still expected is not green
[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
