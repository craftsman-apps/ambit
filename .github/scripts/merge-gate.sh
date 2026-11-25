#!/usr/bin/env bash
# The one check that says "every job a merge needs ran and passed" (job `merge-gate` in ci.yml). A job that was
# skipped counts only when .github/scripts/changes.sh skipped it on purpose (its output is false), or, for quickstart
# and e2e, when the pull request is a draft. A skipped job whose output is true, a failed job and a cancelled job all fail
# the gate. This is the merge rule "skipped never counts unless the path filter skips it on purpose", in code.
#
# A matrix job (cli-unit, e2e) that reports success also has every one of its legs checked in JOBS_FILE: GitHub has
# reported a matrix as passed when one leg was cancelled before it got a runner.
#
# Environment: for each job J: R_J is its result (success, failure, cancelled, skipped) and W_J what changes.sh said
# it needs (true or false; "always" for a job that always runs). DRAFT is "true" for a draft pull request. EVENT is the
# event of the run (pull_request, merge_group, workflow_dispatch) and QUEUE the repository variable MERGE_QUEUE ("true"
# when the merge queue is on): only then may e2e be skipped on a pull request. JOBS_FILE is
# a file with one "<job name>|<conclusion>" line per job of the run (leave it unset only in tests).
# Prints one line per job and exits 1 when any job does not count.
set -u
bad=0
# the legs of the matrix jobs in ci.yml, one per line (a test compares the e2e parts with ci.yml); no bash 4 features: macOS has 3.2
legs_of() {
  case "$1" in
    cli-unit) printf '%s\n' "cli-unit (ubuntu-latest)" "cli-unit (macos-latest)" "cli-unit (windows-latest)" ;;
    e2e) printf '%s\n' "e2e (a)" "e2e (b)" "e2e (c)" "e2e (d)" "e2e (e)" ;;
  esac
}

legs_ok() { # legs_ok <job>: every leg of a matrix job succeeded
  [ -n "${JOBS_FILE:-}" ] || return 0
  [ -r "$JOBS_FILE" ] || { echo "     the list of jobs $JOBS_FILE cannot be read"; return 1; }
  local leg ok=0
  while IFS= read -r leg; do
    grep -qxF "$leg|success" "$JOBS_FILE" || { echo "     leg '$leg' did not succeed"; ok=1; }
  done <<LEGS
$(legs_of "$1")
LEGS
  return $ok
}

check() { # check <job> <result> <wanted>
  local job=$1 result=$2 wanted=$3 why=""
  case "$result" in
    success)
      if { [ "$job" = e2e ] || [ "$job" = cli-unit ]; } && ! legs_ok "$job"; then
        echo "FAIL $job: reported success, but not every leg did"; bad=1
      else
        echo "ok   $job: success"
      fi
      return ;;
    skipped)
      if [ "$wanted" = false ]; then echo "ok   $job: skipped on purpose (changes.sh: not needed)"; return; fi
      if [ "${DRAFT:-false}" = true ] && { [ "$job" = quickstart ] || [ "$job" = e2e ]; }; then
        echo "ok   $job: skipped (draft pull request)"; return
      fi
      # The full e2e runs in the merge queue, not on the pull request, once MERGE_QUEUE is "true" (ci.yml). The gate of
      # the queue (event merge_group) and of a manual run (workflow_dispatch) never accepts that skip.
      if [ "$job" = e2e ] && [ "${EVENT:-}" = pull_request ] && [ "${QUEUE:-}" = true ]; then
        echo "ok   $job: not run on a pull request; it runs in the merge queue"; return
      fi
      why="skipped, but changes.sh says it is needed ($wanted)" ;;
    *) why="$result" ;;
  esac
  echo "FAIL $job: $why"; bad=1
}

check changes "${R_CHANGES:-missing}" always
check changes-test "${R_CHANGES_TEST:-missing}" always
check build "${R_BUILD:-missing}" "${W_BUILD:-missing}"
check image "${R_IMAGE:-missing}" "${W_BUILD:-missing}"
check cli-helm-interop "${R_INTEROP:-missing}" "${W_INTEROP:-missing}"
check cli-unit "${R_CLI_UNIT:-missing}" always
check cli-local "${R_CLI_LOCAL:-missing}" "${W_CLI_KIND:-missing}"
check cli-install "${R_CLI_INSTALL:-missing}" "${W_CLI_KIND:-missing}"
check cli-gitops-init "${R_CLI_GITOPS:-missing}" "${W_CLI_KIND:-missing}"
check cli-install-k3s "${R_CLI_K3S:-missing}" "${W_CLI_KIND:-missing}"
check cli-partial "${R_CLI_PARTIAL:-missing}" "${W_CLI_KIND:-missing}"
check cli-snapshot "${R_CLI_SNAPSHOT:-missing}" "${W_SNAPSHOT:-missing}"
check quickstart "${R_QUICKSTART:-missing}" "${W_QUICKSTART:-missing}"
check e2e "${R_E2E:-missing}" "${W_E2E:-missing}"
exit $bad
