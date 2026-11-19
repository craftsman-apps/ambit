#!/usr/bin/env bash
# Tests for highest-tag.sh (#108 W2): only the highest stable tag may become the latest release and go to the channels.
set -euo pipefail
cd "$(dirname "$0")"
fails=0
TAGS="v0.2.0-rc.1 v0.2.0 v0.3.0 v0.4.0 v0.4.1 v0.5.0-rc.1 v0.5.0 v0.10.0-rc.1"

check() {  # check <name> <tag> <want> [tags]
  local got
  got=$(printf '%s\n' ${4:-$TAGS} | ./highest-tag.sh "$2")
  if [ "$got" = "highest=$3" ]; then echo "ok   $1"; else echo "FAIL $1: want highest=$3, got '$got'"; fails=$((fails + 1)); fi
}

check "re-run of an older stable tag" v0.4.1 false
check "the highest stable tag" v0.5.0 true
check "a new patch above it" v0.5.1 true "$TAGS v0.5.1"
check "two-digit minor sorts as a number" v0.10.0 true "$TAGS v0.10.0"
check "v0.9.0 is below v0.10.0" v0.9.0 false "$TAGS v0.9.0 v0.10.0"
check "a prerelease tag is never the highest" v0.10.0-rc.1 false
check "the only tag" v0.1.0 true "v0.1.0"
[ "$fails" -eq 0 ]
