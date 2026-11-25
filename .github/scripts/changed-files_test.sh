#!/usr/bin/env bash
# Tests for changed-files.sh: a merge group (GitHub's merge queue) gets the same jobs as the pull request for the same
# change, built on real commits in a temporary repository, and ci.yml has no pull-request-only expression left
# unguarded for a merge group.
set -euo pipefail
cd "$(dirname "$0")"
S=$PWD
ROOT=$(cd ../.. && pwd)
fails=0
ok()   { echo "ok   $1"; }
bad()  { echo "FAIL $1"; fails=$((fails + 1)); }
jobs() { "$S/changed-files.sh" | "$S/changes.sh" | sed -n 's/=true$//p' | xargs; }  # the jobs that run, from BASE and HEAD

T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cd "$T"
git init -q -b main . && git config user.email t@example.com && git config user.name t
mkdir -p docs internal/controller test
echo 1 > docs/a.md; echo 1 > docs/b.md; echo 1 > internal/controller/x.go; echo 1 > test/step5.sh; echo 1 > README.txt
git add -A && git commit -qm base
BASE0=$(git rev-parse HEAD)

# A pull request branch from BASE0 changing <file>; then main moves on with another change; then the queue's temporary
# commit (the new main plus the pull request). Prints "<pr jobs>|<merge group jobs>".
both() { # both <file the PR changes> <file main changes meanwhile>
  git checkout -q -B pr "$BASE0"; echo pr >> "$1"; git add -A; git commit -qm pr; local PR; PR=$(git rev-parse HEAD)
  git checkout -q -B main "$BASE0"; echo main >> "$2"; git add -A; git commit -qm other; local MAIN; MAIN=$(git rev-parse HEAD)
  git checkout -q -B queue "$MAIN"; git merge -q --no-ff -m "queue" "$PR"; local MG; MG=$(git rev-parse HEAD)
  local pr mg
  pr=$(BASE=$BASE0 HEAD=$PR jobs)
  mg=$(BASE=$MAIN HEAD=$MG jobs)
  git checkout -q -B main "$BASE0"
  echo "$pr|$mg"
}
same() { # same <name> <file the PR changes> <file main changes meanwhile>
  local r; r=$(both "$2" "$3")
  if [ "${r%|*}" = "${r#*|}" ]; then ok "$1: pull request and merge group run '${r%|*}'"; else bad "$1: pull request '${r%|*}', merge group '${r#*|}'"; fi
}
same "documentation change, main moved by a Go change" docs/a.md internal/controller/x.go
same "Go change, main moved by a documentation change" internal/controller/x.go docs/a.md
same "step script change, main moved by another step script" test/step5.sh README.txt
same "documentation change, main moved by other documentation" docs/a.md docs/b.md

# The jobs themselves: documentation only skips every job; a Go change runs them all.
r=$(both docs/a.md docs/b.md)
[ -z "${r%|*}" ] && [ -z "${r#*|}" ] && ok "a documentation-only merge group runs no job" || bad "documentation only runs '$r'"
r=$(both internal/controller/x.go docs/a.md)
case "${r#*|}" in *e2e*quickstart* | *quickstart*e2e*) ok "a Go change in a merge group runs e2e and quickstart" ;; *) bad "Go change runs '${r#*|}'" ;; esac

# Safe by default: no base, an unknown commit, and a push all run every job.
all="build cli_unit interop snapshot cli_kind quickstart e2e"
[ "$(BASE= HEAD= jobs)" = "$all" ] && ok "no base: every job runs" || bad "no base"
[ "$(BASE=0000000000000000000000000000000000000001 HEAD=$BASE0 jobs)" = "$all" ] && ok "a commit that does not exist: every job runs" || bad "unknown base"
[ "$(env -u BASE -u HEAD bash -c "$S/changed-files.sh | $S/changes.sh | sed -n 's/=true\$//p' | xargs")" = "$all" ] && ok "a push (no BASE): every job runs" || bad "push"

# ci.yml: the workflow listens for merge_group, and every use of pull_request data has a merge_group fallback or a guard.
cd "$ROOT"
grep -q '^  merge_group:' .github/workflows/ci.yml && ok "ci.yml runs on merge_group" || bad "ci.yml has no merge_group trigger"
grep -q 'github.event.merge_group.base_sha' .github/workflows/ci.yml && grep -q 'github.event.merge_group.head_sha' .github/workflows/ci.yml &&
  ok "the base and head of a merge group are read" || bad "merge_group base_sha/head_sha not used"
# allowed: the fallbacks, the draft check (empty for a merge group, so quickstart and e2e run), and the event_name guard
stray=$(grep -n 'github.event.pull_request\|github.base_ref\|github.head_ref' .github/workflows/ci.yml |
  grep -v 'pull_request.base.sha || github.event.merge_group.base_sha' | grep -v 'pull_request.head.sha || github.event.merge_group.head_sha' |
  grep -v '!github.event.pull_request.draft' | grep -v "github.event.pull_request.draft && 'true'" || true)
guarded=$(grep -n -B1 'github.base_ref' .github/workflows/ci.yml | grep -c "github.event_name == 'pull_request'" || true)
if [ -z "$(printf '%s\n' "$stray" | grep -v 'github.base_ref' || true)" ] && { ! grep -q 'github.base_ref' <<<"$stray" || [ "$guarded" -ge 1 ]; }; then
  ok "no pull-request-only expression is left unguarded"
else
  bad "unguarded pull-request-only expression in ci.yml: $stray"
fi

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
