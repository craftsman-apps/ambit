#!/usr/bin/env bash
# The files a CI run must judge, one per line, for .github/scripts/changes.sh. The same list for the same change on both
# events that run the full check:
#   pull_request  BASE is the base of the pull request, HEAD its head.
#   merge_group   BASE is the branch the queue merges into, HEAD the queue's temporary commit (the branch plus the
#                 queued pull request, and the pull requests ahead of it in the queue). BASE is an ancestor of HEAD, so
#                 the list is everything the queue would add to the branch.
# Any other event (a push), no BASE, or a diff that fails: an empty list, which runs every job (changes.sh is safe by
# default: it never skips a job it cannot judge).
# Needs the commits of BASE and HEAD (checkout with fetch-depth 0). Environment: BASE, HEAD.
set -u
[ -n "${BASE:-}" ] && [ -n "${HEAD:-}" ] || exit 0
git diff --name-only "$BASE...$HEAD" 2>/dev/null || true
