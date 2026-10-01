#!/usr/bin/env bash
# Upgrade verification: does Ambit still work with a new version of one component it ships?
#   test/upgrade.sh COMPONENT VERSION [STEPS]
#   COMPONENT  agentgateway | toolhive (VERSION = chart version, CRDs included)
#              clickhouse | collector | ratelimit | valkey (VERSION = image, pinned by digest if you can)
#              dex (VERSION = image tag)
#   STEPS      acceptance steps to run on the candidate (default "4a 4b 5 7 8 9 10")
# Changes the kind cluster for the run and puts the pinned versions back at the end, also when interrupted.
# Exit code 0 only when the upgrade lost no request, every contract held and every step passed.
set -euo pipefail
COMP=${1:?component}; VER=${2:?version}; STEPS=${3:-"4a 4b 5 7 8 9 10"}
NS=ambit-system
cd "$(dirname "$0")/.."
source test/lib.sh
CAND=$W/candidate; mkdir -p "$CAND"
REPORT=${UPGRADE_REPORT:-$PWD/upgrade-report-$COMP.txt}
results=()
note() { results+=("$1"); echo "$1"; }
crds() { kubectl apply --server-side --force-conflicts -f "$1" >/dev/null; }
proxy_image() { kubectl -n "$NS" get deploy -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].spec.template.spec.containers[0].image}'; }

# 1 · prepare the candidate (the repository is not changed)
case "$COMP" in
  agentgateway|toolhive)
    sub=$([[ "$COMP" == agentgateway ]] && echo agentgateway || echo toolhive-operator)
    FROM=$(uv run --no-project --with pyyaml python -c "import yaml;print([d['version'] for d in yaml.safe_load(open('charts/ambit/Chart.yaml'))['dependencies'] if d['name']=='$sub'][0])")
    cp -R charts/ambit "$CAND/chart"; rm -rf "$CAND/chart/charts"
    uv run --no-project --with pyyaml python - "$CAND/chart/Chart.yaml" "$sub" "$VER" <<'PY'
import sys, yaml
p, sub, ver = sys.argv[1:4]
c = yaml.safe_load(open(p))
for d in c["dependencies"]:
    if d["name"] == sub:
        d["version"] = ver
yaml.safe_dump(c, open(p, "w"), sort_keys=False)
PY
    command helm dependency update "$CAND/chart" >/dev/null
    mkdir -p "$CAND/crds"
    if [[ "$COMP" == agentgateway ]]; then
      command helm template agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds --version "$VER" > "$CAND/crds/agentgateway.yaml"
    else
      command helm template toolhive-operator-crds oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds --version "$VER" > "$CAND/crds/toolhive.yaml"
    fi
    export AMBIT_CHART="$CAND/chart" ;;
  clickhouse|collector|ratelimit|valkey|dex)
    key=evidence.clickhouse.image
    [[ "$COMP" == collector ]] && key=evidence.collector.image
    [[ "$COMP" == ratelimit ]] && key=limits.ratelimit.image
    [[ "$COMP" == valkey ]] && key=limits.valkey.image
    [[ "$COMP" == dex ]] && key=dex.image.tag
    FROM=$(uv run --no-project --with pyyaml python -c "
import yaml; v = yaml.safe_load(open('charts/ambit/values.yaml'))
for k in '$key'.split('.'): v = v[k]
print(v)")
    uv run --no-project --with pyyaml python - "$CAND/values.yaml" "$key" "$VER" <<'PY'
import sys, yaml
p, key, ver = sys.argv[1:4]
out = cur = {}
parts = key.split(".")
for k in parts[:-1]:
    cur = cur.setdefault(k, {})
cur[parts[-1]] = ver
yaml.safe_dump(out, open(p, "w"))
PY
    export AMBIT_EXTRA_VALUES="$CAND/values.yaml" ;;
  *) fail "unknown component $COMP" ;;
esac
note "component     $COMP   $FROM → $VER"

restore() {
  unset AMBIT_CHART AMBIT_EXTRA_VALUES
  crds deploy/crds/ || true
  command helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 6m >/dev/null 2>&1 || true
  if [[ "$COMP" == agentgateway ]]; then
    for _ in $(seq 1 60); do [[ $(proxy_image) == *":$FROM" ]] && break; sleep 3; done
  fi
  echo "restored $COMP $FROM"
}
trap 'rc=$?; { printf "%s\n" "${results[@]}"; } > "$REPORT"; restore; on_exit $rc' EXIT

# 2 · baseline: the pinned versions run and every contract holds (otherwise a result would mean nothing)
( unset AMBIT_CHART AMBIT_EXTRA_VALUES; ensure_llm_setup "$NS"; ./test/contracts.sh "$NS" >/dev/null ) || fail "baseline: the pinned install does not pass the contract checks"
note "baseline      PASS  pinned versions healthy"

# 3 · upgrade in place under steady MCP traffic
M=$(token mira)
( ok=0; bad=0; tok=$M; t0=$SECONDS
  while [[ ! -f "$CAND/stop" ]]; do
    (( SECONDS - t0 > 60 )) && { tok=$(token mira); t0=$SECONDS; }
    c=$(command curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"upgrade","version":"1"}}}' "$MCP")
    if [[ "$c" == 200 ]]; then ok=$((ok + 1)); else bad=$((bad + 1)); echo "$c" >> "$CAND/bad"; fi
    sleep 0.3
  done
  echo "$ok $bad" > "$CAND/traffic" ) &
traffic=$!
sleep 3
[[ -d "$CAND/crds" ]] && crds "$CAND/crds/"
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 8m >/dev/null
if [[ "$COMP" == agentgateway ]]; then
  for _ in $(seq 1 80); do [[ $(proxy_image) == *":$VER" ]] && break; sleep 3; done
  [[ $(proxy_image) == *":$VER" ]] || fail "upgrade: the gateway pods never moved to $VER ($(proxy_image))"
  kubectl -n "$NS" rollout status deploy -l gateway.networking.k8s.io/gateway-name=ambit --timeout=300s >/dev/null
fi
for d in $(kubectl -n "$NS" get deploy -o name); do kubectl -n "$NS" rollout status "$d" --timeout=300s >/dev/null; done
sleep 5; touch "$CAND/stop"; wait $traffic
read -r ok bad < "$CAND/traffic"
if [[ "$bad" == 0 ]]; then note "upgrade       PASS  $ok requests during the upgrade, 0 failed"
else note "upgrade       FAIL  $ok requests, $bad failed ($(sort "$CAND/bad" | uniq -c | tr -s ' ' | tr '\n' ' '))"; fi

# 4 · contracts, 5 · acceptance steps — all on the candidate; failures are collected, not fatal
failed=$([[ "$bad" == 0 ]] && echo 0 || echo 1)
if out=$(./test/contracts.sh "$NS" 2>&1); then note "contracts     PASS  $(grep -o '^PASS C[0-9]' <<<"$out" | cut -d' ' -f2 | tr '\n' ' ') ($(grep '^PASS C3' <<<"$out" | sed 's/.*(\(.*\))/\1/'))"
else failed=$((failed + 1)); note "contracts     FAIL  $(grep '^FAIL' <<<"$out" | head -1)"; fi
for s in $STEPS; do
  if out=$(./test/step$s.sh "$NS" 2>&1); then note "step $(printf %-8s "$s") PASS"
  else failed=$((failed + 1)); note "step $(printf %-8s "$s") FAIL  $(grep '^FAIL' <<<"$out" | head -1)"; fi
done
if (( failed == 0 )); then note "verdict       READY: $COMP $VER passed every check"; FINISHED=1
else note "verdict       NOT READY: $failed check(s) failed"; exit 1; fi
