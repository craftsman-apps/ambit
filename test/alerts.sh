#!/usr/bin/env bash
# Unit tests for the alert rules (alerts.enabled): renders the chart, then promtool checks the rules and
# runs test/alerts/tests.yaml against them. Needs helm and docker.
set -euo pipefail
cd "$(dirname "$0")/.."
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
helm template ambit charts/ambit -n ambit-system -f charts/ambit/values-kind.yaml \
  --set alerts.enabled=true,evidence.enabled=true,limits.enabled=true,limits.failureMode=FailClosed >"$W/render.yaml"
uv run --no-project --with pyyaml python - "$W" <<'PY'
import sys, yaml
w = sys.argv[1]
rules = [d for d in yaml.safe_load_all(open(f"{w}/render.yaml")) if d and d.get("kind") == "PrometheusRule"]
assert len(rules) == 1, f"{len(rules)} PrometheusRules rendered"
yaml.safe_dump({"groups": rules[0]["spec"]["groups"]}, open(f"{w}/rules.yaml", "w"))
PY
cp test/alerts/tests.yaml "$W/"
docker run --rm --user "$(id -u):$(id -g)" -v "$W:/w" -w /w --entrypoint promtool prom/prometheus:v3.7.3 check rules rules.yaml
docker run --rm --user "$(id -u):$(id -g)" -v "$W:/w" -w /w --entrypoint promtool prom/prometheus:v3.7.3 test rules tests.yaml
