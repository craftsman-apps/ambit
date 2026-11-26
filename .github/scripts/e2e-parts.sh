#!/usr/bin/env bash
# The e2e matrix of ci.yml as a JSON array: e2e-parts.sh [PARTS]. PARTS is "all" (the default, also for an empty value)
# or a space-separated list of part names such as "f" or "a e". Pull requests and merge groups always pass "all".
# An unknown part name is an error, so a typo never runs an empty matrix. This list is the one place where the parts
# are defined; .github/scripts/merge-gate_test.sh compares it with the legs the gate expects.
set -euo pipefail
parts=${1:-all}; [ -n "${parts// /}" ] || parts=all
all='[
  {"name":"a","steps":"0 1 2 3 3b 3c args","ui":true},
  {"name":"b","steps":"8 fallback","ui":false},
  {"name":"c","steps":"10 keys 9 compress sa","ui":false},
  {"name":"f","steps":"agents agentconn","ui":false},
  {"name":"d","steps":"12 ext savings evidence","ui":false},
  {"name":"e","steps":"4a 4b 4c 4d 5 6 7 guard fd platforms","ui":false},
  {"name":"g","steps":"oauth","ui":false}
]'
if [ "$parts" = all ]; then jq -c . <<<"$all"; exit 0; fi
# shellcheck disable=SC2086
jq -c --arg want "$parts" '
  ($want | split(" ") | map(select(. != ""))) as $w
  | ([.[].name] | . as $known | $w | map(select(. as $p | $known | index($p) | not))) as $bad
  | if ($bad | length) > 0 then error("unknown e2e part: \($bad | join(" ")); known: \([.[].name] | join(" "))")
    else map(select(.name as $n | $w | index($n))) end' <<<"$all"
