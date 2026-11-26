#!/usr/bin/env bash
# Tests for wait-pr.sh with a fake `gh` that prints canned JSON (the case is in $CASE).
set -uo pipefail
cd "$(dirname "$0")"
fails=0
bin=$(mktemp -d); trap 'rm -rf "$bin"' EXIT
cat >"$bin/gh" <<'GH'
#!/usr/bin/env bash
if [ "$1" = api ]; then
  echo '[{"event":"removed_from_merge_queue","reason":"FAILED_CHECKS"}]'; exit 0
fi
q='"autoMergeRequest":{"mergeMethod":"MERGE"},"isInMergeQueue":true,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"'
run='{"name":"e2e (f)","workflowName":"ci","status":"COMPLETED","conclusion":"%s","detailsUrl":"https://github.com/o/r/actions/runs/1"}'
case $CASE in
  merged) echo '{"state":"MERGED","mergedAt":"2026-11-26T00:00:00Z","statusCheckRollup":[]}' ;;
  closed) echo '{"state":"CLOSED","mergedAt":null,"statusCheckRollup":[]}' ;;
  removed) echo '{"state":"OPEN","mergedAt":null,"autoMergeRequest":null,"isInMergeQueue":false,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[]}' ;;
  conflict) echo '{"state":"OPEN","mergedAt":null,"autoMergeRequest":{},"isInMergeQueue":false,"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","statusCheckRollup":[]}' ;;
  failed) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[$(printf "$run" FAILURE)]}" ;;
  pending) echo "{\"state\":\"OPEN\",\"mergedAt\":null,$q,\"statusCheckRollup\":[{\"name\":\"build\",\"status\":\"IN_PROGRESS\",\"conclusion\":\"\"}]}" ;;
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
[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
