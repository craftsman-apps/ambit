#!/usr/bin/env bash
# The files a CI run must judge, one per line, for .github/scripts/changes.sh. The same list for the same change on both
# events that run the full check:
#   pull_request  BASE is the base of the pull request, HEAD its head.
#   merge_group   HEAD is the queue's temporary commit (the branch plus the queued pull request, and the pull requests
#                 ahead of it in the queue). MERGE_GROUP_BASE_REF is the branch the queue merges into (refs/heads/main).
#                 BASE is where HEAD left that branch: the merge base of HEAD and the branch. It is NOT the base_sha of
#                 the event: with several entries in the queue, that can be the commit of the entry ahead, and a docs-only
#                 entry behind a Go entry would then skip the jobs the Go change needs. From the merge base, the list is
#                 everything the queue adds to the branch, the entries ahead of this one included.
# Any other event (a push), no base, or a diff that fails: an empty list, which runs every job (changes.sh is safe by
# default: it never skips a job it cannot judge).
# Needs the commits of the branch, BASE and HEAD (checkout with fetch-depth 0). Environment: BASE, HEAD,
# MERGE_GROUP_BASE_REF.
set -u
if [ -n "${MERGE_GROUP_BASE_REF:-}" ]; then
  BASE=$(git merge-base "${HEAD:-HEAD}" "origin/${MERGE_GROUP_BASE_REF#refs/heads/}" 2>/dev/null) || exit 0
fi
[ -n "${BASE:-}" ] && [ -n "${HEAD:-}" ] || exit 0
# --no-renames: a moved file lists both paths (test/quickstart.sh to docs/x.sh must still run the job of the old path)
git diff --name-only --no-renames "$BASE...$HEAD" 2>/dev/null || true
