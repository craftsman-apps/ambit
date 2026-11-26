#!/usr/bin/env bash
# Wait for a pull request and say, in one line, why the wait ended. Needs gh and jq.
#
#   hack/wait-pr.sh NUMBER [--timeout 3h] [--interval 60] [--checks]
#
# Default: wait until the pull request is merged. Use it after `gh pr merge --auto`.
# --checks: wait only until the pull request's own checks have all finished (green or failed).
#
# Exit codes (the first event that happens ends the wait):
#   0 merged (with --checks: every check is green)   2 closed without a merge
#   3 removed from the merge queue (or never queued) 4 merge conflict
#   5 a check failed (the check name and run link are printed)
#   6 timeout                                        1 wrong usage or gh keeps failing
set -uo pipefail

usage() { echo "usage: $0 NUMBER [--timeout 3h] [--interval 60] [--checks]" >&2; exit 1; }
secs() { # 3h, 90m, 45s or plain seconds -> seconds
  case $1 in
    *h) echo $((${1%h} * 3600)) ;; *m) echo $((${1%m} * 60)) ;; *s) echo "${1%s}" ;; *) echo "$1" ;;
  esac
}

[ $# -ge 1 ] || usage
number=$1; shift
timeout=3h; interval=60; checks_only=false
while [ $# -gt 0 ]; do
  case $1 in
    --timeout) timeout=${2:-}; shift 2 || usage ;;
    --interval) interval=${2:-}; shift 2 || usage ;;
    --checks) checks_only=true; shift ;;
    *) usage ;;
  esac
done
[[ $number =~ ^[0-9]+$ ]] || usage
timeout_s=$(secs "$timeout"); interval_s=$(secs "$interval")
[[ $timeout_s =~ ^[0-9]+$ && $interval_s =~ ^[0-9]+$ ]] || usage

# Why the queue dropped the pull request: the last timeline event that says so (best effort).
removal_reason() {
  gh api --paginate "repos/{owner}/{repo}/issues/$number/timeline" 2>/dev/null |
    jq -rs '[.[][] | select(.event | test("removed_from_merge_queue|auto_merge_disabled|head_ref_force_pushed|head_ref_deleted"))]
            | last // empty | "\(.event)" + (if .reason then ": \(.reason)" else "" end)' 2>/dev/null
}

fields=state,mergedAt,mergeable,mergeStateStatus,autoMergeRequest,isInMergeQueue,statusCheckRollup,url
start=$SECONDS; errors=0
while :; do
  if ! pr=$(gh pr view "$number" --json "$fields" 2>&1) || ! jq -e . >/dev/null 2>&1 <<<"$pr"; then
    errors=$((errors + 1))
    [ "$errors" -lt 5 ] || { echo "ERROR #$number: gh failed 5 times in a row: $pr"; exit 1; }
  else
    errors=0
    state=$(jq -r .state <<<"$pr")
    if [ "$state" = MERGED ] || [ "$(jq -r '.mergedAt // ""' <<<"$pr")" != "" ]; then
      [ "$checks_only" = true ] || { echo "MERGED #$number"; exit 0; }
    fi
    [ "$state" != CLOSED ] || { echo "CLOSED #$number without a merge"; exit 2; }

    # A failed check: ended with failure, timeout, cancel or startup failure (check run), or failure or error (status).
    failed=$(jq -r '[.statusCheckRollup[]? | select((.conclusion // "") as $c | (.state // "") as $s
        | ($c | IN("FAILURE","TIMED_OUT","CANCELLED","STARTUP_FAILURE")) or ($s | IN("FAILURE","ERROR")))]
        | first // empty | "\(.name // .context)" + (if .workflowName then " (\(.workflowName))" else "" end)
        + " " + (.detailsUrl // .targetUrl // "no link")' <<<"$pr")
    if [ -n "$failed" ]; then echo "CHECK FAILED #$number: $failed"; exit 5; fi

    if [ "$checks_only" = true ]; then
      pending=$(jq '[.statusCheckRollup[]? | select((.status // "COMPLETED") != "COMPLETED" or (.state // "") == "PENDING")] | length' <<<"$pr")
      total=$(jq '.statusCheckRollup | length' <<<"$pr")
      if [ "$pending" = 0 ] && [ "$total" -gt 0 ]; then echo "CHECKS GREEN #$number ($total checks)"; exit 0; fi
    else
      if [ "$(jq -r '.mergeable' <<<"$pr")" = CONFLICTING ] || [ "$(jq -r '.mergeStateStatus' <<<"$pr")" = DIRTY ]; then
        echo "CONFLICT #$number: the branch conflicts with its base; update from main and resolve"; exit 4
      fi
      if [ "$(jq -r '(.autoMergeRequest != null) or .isInMergeQueue' <<<"$pr")" != true ]; then
        reason=$(removal_reason)
        echo "REMOVED FROM QUEUE #$number: no auto-merge request and not in the queue (${reason:-reason not found in the timeline})"; exit 3
      fi
    fi
  fi
  [ $((SECONDS - start + interval_s)) -le "$timeout_s" ] || { echo "TIMEOUT #$number after $timeout"; exit 6; }
  sleep "$interval_s"
done
