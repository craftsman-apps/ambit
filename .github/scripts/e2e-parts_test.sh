#!/usr/bin/env bash
# Tests for e2e-parts.sh: which parts of the e2e matrix a run gets.
set -uo pipefail
cd "$(dirname "$0")"
fails=0
names() { jq -r '[.[].name] | join(" ")' ; }
check() { # check <name> <want names> <parts argument...>
  local name=$1 want=$2; shift 2
  local got; got=$(./e2e-parts.sh "$@" 2>/dev/null | names)
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: want '$want', got '$got'"; fails=$((fails + 1)); fi
}
check "no argument: every part" "a b c f d e g"
check "all: every part" "a b c f d e g" all
check "empty value (a pull request): every part" "a b c f d e g" ""
check "one part" "f" f
check "two parts, in matrix order" "a e" "e a"
check "a repeated part runs once" "f" "f f"
rc=0; ./e2e-parts.sh "z" >/dev/null 2>&1 || rc=$?
if [ "$rc" != 0 ]; then echo "ok   an unknown part is an error"; else echo "FAIL an unknown part gave exit 0"; fails=$((fails + 1)); fi
rc=0; ./e2e-parts.sh "f z" >/dev/null 2>&1 || rc=$?
if [ "$rc" != 0 ]; then echo "ok   one unknown part among known ones is an error"; else echo "FAIL f z gave exit 0"; fails=$((fails + 1)); fi
[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
