#!/usr/bin/env bash
# The one check that says "every job a merge needs ran and passed" (job `merge-gate` in ci.yml). A job that was
# skipped counts only when .github/scripts/changes.sh skipped it on purpose (its output is false), or, for quickstart
# and e2e, when the pull request is a draft. A skipped job whose output is true, a failed job and a cancelled job all fail
# the gate. This is the merge rule "skipped never counts unless the path filter skips it on purpose", in code.
#
# Environment: for each job J: R_J is its result (success, failure, cancelled, skipped) and W_J what changes.sh said
# it needs (true or false; "always" for a job that always runs). DRAFT is "true" for a draft pull request.
# Prints one line per job and exits 1 when any job does not count.
set -u
bad=0
check() { # check <job> <result> <wanted>
  local job=$1 result=$2 wanted=$3 why=""
  case "$result" in
    success) echo "ok   $job: success"; return ;;
    skipped)
      if [ "$wanted" = false ]; then echo "ok   $job: skipped on purpose (changes.sh: not needed)"; return; fi
      if [ "${DRAFT:-false}" = true ] && { [ "$job" = quickstart ] || [ "$job" = e2e ]; }; then
        echo "ok   $job: skipped (draft pull request)"; return
      fi
      why="skipped, but changes.sh says it is needed ($wanted)" ;;
    *) why="$result" ;;
  esac
  echo "FAIL $job: $why"; bad=1
}
while read -r job result wanted; do check "$job" "$result" "$wanted"; done <<LIST
changes ${R_CHANGES:-missing} always
changes-test ${R_CHANGES_TEST:-missing} always
build ${R_BUILD:-missing} ${W_BUILD:-missing}
image ${R_IMAGE:-missing} ${W_BUILD:-missing}
cli-helm-interop ${R_INTEROP:-missing} ${W_INTEROP:-missing}
cli-unit ${R_CLI_UNIT:-missing} always
cli-local ${R_CLI_LOCAL:-missing} ${W_CLI_KIND:-missing}
cli-install ${R_CLI_INSTALL:-missing} ${W_CLI_KIND:-missing}
cli-gitops-init ${R_CLI_GITOPS:-missing} ${W_CLI_KIND:-missing}
cli-install-k3s ${R_CLI_K3S:-missing} ${W_CLI_KIND:-missing}
cli-partial ${R_CLI_PARTIAL:-missing} ${W_CLI_KIND:-missing}
cli-snapshot ${R_CLI_SNAPSHOT:-missing} ${W_SNAPSHOT:-missing}
quickstart ${R_QUICKSTART:-missing} ${W_QUICKSTART:-missing}
e2e ${R_E2E:-missing} ${W_E2E:-missing}
LIST
exit $bad
