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

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
