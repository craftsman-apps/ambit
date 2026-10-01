#!/usr/bin/env bash
# Requirement 8 · rules on tool arguments ("can it delete the production database?"): an access rule limits
# what a tool may be called with; ambit-guard refuses a call that breaks a limit and says which one.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
restore() { kubectl -n "$NS" delete "accessrules.$G" args-echo args-sum --ignore-not-found >/dev/null 2>&1 || true; }
trap 'rc=$?; restore; on_exit $rc' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
kubectl -n "$NS" wait "servers.$G" --all --for=condition=Ready --timeout=300s >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: args-echo }
spec:
  groups: [eng]
  servers:
    - name: everything
      tools: [echo]
      arguments: [{ name: message, notContains: [secret, "rm -rf"] }]
---
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: args-sum }
spec:
  groups: [eng]
  servers:
    - name: everything
      tools: [get-sum]
      arguments: [{ name: a, max: "100" }]
YAML
kubectl -n "$NS" wait "accessrules.$G" args-echo args-sum --for=condition=Ready --timeout=60s >/dev/null || fail "0 rules not Ready: $(kubectl -n "$NS" get "accessrules.$G" args-echo -o jsonpath='{.status.conditions}')"

# The CRD refuses a limit without a condition, and a malformed maximum.
bad=$(kubectl -n "$NS" apply --dry-run=server -f - 2>&1 <<YAML || true
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: args-bad }
spec: { groups: [eng], servers: [{ name: everything, tools: [echo], arguments: [{ name: message }, { name: a, max: "lots" }] }] }
YAML
)
[[ "$bad" == *"set oneOf, notContains or max"* && "$bad" == *"max"* ]] || fail "0 the CRD accepted invalid limits: $bad"
pass "0 two rules with argument limits are Ready; the CRD refuses a limit without a condition or with a bad maximum"

A=$(token alice)
for _ in $(seq 1 30); do tools "$A"; [[ " $TOOLS " == *" everything_echo "* && " $TOOLS " == *" everything_get-sum "* ]] && break; sleep 2; done
[[ " $TOOLS " == *" everything_echo "* ]] || fail "1 alice does not see everything_echo: $TOOLS"
ok=$(call everything_echo '{"message":"hello"}')
[[ "$ok" == ok* ]] || fail "1 an allowed echo was refused: $ok"
bad=$(call everything_echo '{"message":"my SECRET plan"}')
[[ "$bad" == error* && "$bad" == *"must not contain secret"* ]] || fail "1 a forbidden word passed or the reason is missing: $bad"
pass "1 echo 'hello' allowed; echo with 'SECRET' refused: ${bad#error }"

sum=$(call everything_get-sum '{"a":5,"b":1}')
[[ "$sum" == ok* ]] || fail "2 get-sum within the limit was refused: $sum"
big=$(call everything_get-sum '{"a":500,"b":1}')
[[ "$big" == error* && "$big" == *"a must be a number at most 100"* ]] || fail "2 get-sum over the limit passed: $big"
pass "2 get-sum a=5 allowed; a=500 refused: ${big#error }"

# mira (ml-research) has every tool of everything through another rule, without limits: rules add up.
M=$(token mira); tools "$M"
any=$(call everything_echo '{"message":"secret"}')
[[ "$any" == ok* ]] || fail "3 a rule without limits should still allow mira: $any"
pass "3 limits apply to the rule's own group: mira (another rule, no limits) may echo 'secret'"
done_step args
