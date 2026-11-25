#!/usr/bin/env bash
# Tests for merge-gate.sh: which job results let a merge pass.
set -euo pipefail
cd "$(dirname "$0")"
fails=0

# all_ok: every job succeeded and every job is needed
run() { # run <want rc> <name> [VAR=value...]   (starts from "everything succeeded and is needed")
  local want=$1 name=$2; shift 2
  local rc=0
  env R_CHANGES=success R_CHANGES_TEST=success R_BUILD=success R_IMAGE=success R_INTEROP=success R_CLI_UNIT=success \
    R_CLI_LOCAL=success R_CLI_INSTALL=success R_CLI_GITOPS=success R_CLI_K3S=success R_CLI_PARTIAL=success \
    R_CLI_SNAPSHOT=success R_QUICKSTART=success R_E2E=success \
    W_BUILD=true W_INTEROP=true W_CLI_KIND=true W_SNAPSHOT=true W_QUICKSTART=true W_E2E=true DRAFT=false \
    "$@" ./merge-gate.sh >/dev/null || rc=$?
  if [ "$rc" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: want exit $want, got $rc"; fails=$((fails + 1)); fi
}

run 0 "everything passed"
run 1 "e2e failed" R_E2E=failure
run 1 "e2e cancelled" R_E2E=cancelled
run 1 "e2e skipped although changes.sh says it is needed" R_E2E=skipped
run 1 "quickstart skipped although needed" R_QUICKSTART=skipped
run 0 "e2e skipped on purpose (documentation only)" R_E2E=skipped W_E2E=false
run 0 "a documentation-only change skips everything" R_BUILD=skipped W_BUILD=false R_IMAGE=skipped R_INTEROP=skipped W_INTEROP=false \
  R_CLI_LOCAL=skipped R_CLI_INSTALL=skipped R_CLI_GITOPS=skipped R_CLI_K3S=skipped R_CLI_PARTIAL=skipped W_CLI_KIND=false \
  R_CLI_SNAPSHOT=skipped W_SNAPSHOT=false R_QUICKSTART=skipped W_QUICKSTART=false R_E2E=skipped W_E2E=false
run 0 "draft pull request: quickstart and e2e skipped" DRAFT=true R_QUICKSTART=skipped R_E2E=skipped
run 1 "draft pull request: build still has to pass" DRAFT=true R_BUILD=failure
run 1 "draft pull request: cli-local may not be skipped when needed" DRAFT=true R_CLI_LOCAL=skipped
run 1 "build skipped although needed" R_BUILD=skipped
run 1 "image failed" R_IMAGE=failure
run 1 "cli-unit skipped (it always runs)" R_CLI_UNIT=skipped
run 1 "changes failed" R_CHANGES=failure
run 1 "changes-test skipped" R_CHANGES_TEST=skipped
run 1 "a result that is missing" R_CLI_PARTIAL=
run 1 "an unknown wanted value counts as needed" R_E2E=skipped W_E2E=

# The full e2e runs in the merge queue (MERGE_QUEUE is "true"): a pull request may skip it, nothing else may.
run 0 "pull request, queue on: e2e not run there" EVENT=pull_request QUEUE=true R_E2E=skipped
run 1 "pull request, queue off: e2e skipped by mistake" EVENT=pull_request QUEUE= R_E2E=skipped
run 1 "pull request, QUEUE=false: e2e skipped by mistake" EVENT=pull_request QUEUE=false R_E2E=skipped
run 1 "merge queue run: e2e skipped although needed" EVENT=merge_group QUEUE=true R_E2E=skipped
run 1 "manual run: e2e skipped although needed" EVENT=workflow_dispatch QUEUE=true R_E2E=skipped
run 0 "merge queue run: everything ran" EVENT=merge_group QUEUE=true
run 1 "merge queue run: e2e failed" EVENT=merge_group QUEUE=true R_E2E=failure
run 1 "pull request, queue on: quickstart may not be skipped by mistake" EVENT=pull_request QUEUE=true R_QUICKSTART=skipped
run 1 "pull request, queue on: e2e failure still fails" EVENT=pull_request QUEUE=true R_E2E=failure
run 0 "merge queue run: documentation only skips e2e on purpose" EVENT=merge_group QUEUE=true R_E2E=skipped W_E2E=false
# ci.yml: e2e leaves the pull request only when MERGE_QUEUE is "true", and the gate gets the event and the variable
grep -q "github.event_name != 'pull_request' || vars.MERGE_QUEUE != 'true'" ../workflows/ci.yml && echo "ok   ci.yml: e2e runs on a pull request only without MERGE_QUEUE" || { echo "FAIL ci.yml e2e condition"; fails=$((fails + 1)); }
grep -q 'EVENT: ${{ github.event_name }}' ../workflows/ci.yml && grep -q 'QUEUE: ${{ vars.MERGE_QUEUE }}' ../workflows/ci.yml && echo "ok   ci.yml: the gate gets EVENT and QUEUE" || { echo "FAIL ci.yml gate env"; fails=$((fails + 1)); }
grep -q '^  workflow_dispatch:' ../workflows/ci.yml && echo "ok   ci.yml: a manual run is possible" || { echo "FAIL no workflow_dispatch"; fails=$((fails + 1)); }

# Matrix legs: a matrix that reports success is checked leg by leg (GitHub has reported success for a cancelled leg).
JOBS=$(mktemp); trap 'rm -f "$JOBS"' EXIT
all_legs() { printf '%s\n' "cli-unit (ubuntu-latest)|success" "cli-unit (macos-latest)|success" "cli-unit (windows-latest)|success" \
  "e2e (a)|success" "e2e (b)|success" "e2e (c)|success" "e2e (d)|success" "e2e (e)|success"; }
all_legs > "$JOBS";                                                     run 0 "every leg succeeded" JOBS_FILE="$JOBS"
all_legs | grep -v "e2e (c)" > "$JOBS";                                  run 1 "a leg of e2e is missing although e2e reports success" JOBS_FILE="$JOBS"
all_legs | sed 's/e2e (e)|success/e2e (e)|cancelled/' > "$JOBS";         run 1 "a leg of e2e was cancelled" JOBS_FILE="$JOBS"
all_legs | grep -v "windows" > "$JOBS";                                  run 1 "a leg of cli-unit is missing" JOBS_FILE="$JOBS"
all_legs | grep -v "e2e" > "$JOBS";                                      run 0 "e2e skipped on purpose: its legs are not looked for" JOBS_FILE="$JOBS" R_E2E=skipped W_E2E=false
run 1 "the list of jobs cannot be read" JOBS_FILE=/nonexistent
# the e2e parts the gate expects are the parts of the matrix in ci.yml
parts=$(sed -n 's/^ *- { name: \([a-e]\), steps:.*/\1/p' ../workflows/ci.yml | tr '\n' ' ')
gate=$(grep -o 'e2e ([a-e])' merge-gate.sh | sed 's/e2e (\(.\))/\1/' | tr '\n' ' ')
if [ -n "$parts" ] && [ "$parts" = "$gate" ]; then echo "ok   the e2e parts of the gate are the parts of ci.yml ($parts)"; else echo "FAIL gate parts '$gate', ci.yml parts '$parts'"; fails=$((fails + 1)); fi

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
