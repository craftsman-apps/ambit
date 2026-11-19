#!/usr/bin/env bash
# Whether a tag is the highest stable tag (#108 W2). A re-run of the release workflow of an older stable tag must
# not mark that tag latest again, nor send an old version to Homebrew, npm, Scoop or winget.
# Reads the repository's tags (one per line) on stdin and prints `highest=true|false` for $GITHUB_OUTPUT.
#   git tag -l 'v*' | .github/scripts/highest-tag.sh "$GITHUB_REF_NAME"
set -euo pipefail
tag=${1:?usage: highest-tag.sh <tag>}
case "$tag" in *-*) echo "highest=false"; exit 0 ;; esac  # a prerelease
top=$( { grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; echo "$tag"; } | sort -V | tail -n 1)
if [ "$top" = "$tag" ]; then echo "highest=true"; else echo "highest=false"; fi
