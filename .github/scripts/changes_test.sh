#!/usr/bin/env bash
# Tests for changes.sh with real file lists. Each case lists the jobs that must run; all others must not.
set -euo pipefail
cd "$(dirname "$0")"
fails=0

check() {  # check <name> <jobs that run> <file>...
  local name=$1 want=$2 got
  shift 2
  got=$(printf '%s\n' "$@" | ./changes.sh | sed -n 's/=true$//p' | xargs)
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: want '$want', got '$got'"; fails=$((fails + 1)); fi
}

ALL="build cli_unit interop snapshot cli_kind quickstart e2e"

# PR #149: site pages, Markdown and .gitignore.
check "PR 149 (site and docs)" "" .gitignore CHANGELOG.md docs/maintainers/release-checklist.md docs/upgrading.md \
  site/package.json site/scripts/changelog-page.mjs site/scripts/changelog-page.test.mjs site/sidebars.ts
check "PR 146 (Go and a step script)" "$ALL" CHANGELOG.md internal/controller/activation.go \
  internal/controller/activation_envtest_test.go test/stepkeys.sh
check "PR 144 (Go test only)" "$ALL" CHANGELOG.md internal/console/leader_test.go
check "go.mod" "$ALL" go.mod go.sum
check "web only" "build cli_kind quickstart e2e" web/src/pages/Models.tsx web/package.json
check "web file a CLI test reads" "build cli_unit cli_kind quickstart e2e" web/src/lib/tool-setup.ts
check "chart" "build cli_unit cli_kind quickstart e2e" charts/ambit/values.yaml charts/ambit/templates/console.yaml
check "Dockerfile" "build cli_kind quickstart e2e" Dockerfile
check "e2e step script" "build e2e" test/step5.sh test/keycloak/values-sso.yaml
check "quickstart script" "build quickstart" test/quickstart.sh
check "cli-local script" "build cli_kind" test/cli-local.sh
check "alert rules" "build" test/alerts/tests.yaml
check "ci.yml" "$ALL" .github/workflows/ci.yml
check "this script" "$ALL" .github/scripts/changes.sh
check "release.yml" "" .github/workflows/release.yml
check "unknown top-level file" "$ALL" README.md newtool.toml
check "new top-level directory" "$ALL" tools/gen/main.go
check ".gitattributes (line endings on checkout)" "$ALL" .gitattributes
check "install.sh" "build cli_unit" site/static/install.sh
check "install.ps1 with site pages" "build cli_unit" site/static/install.ps1 site/src/pages/index.tsx
check "docs/presets.md" "build" docs/presets.md
check "npm packaging" "build snapshot" scripts/npm/publish.mjs
check "LICENSE" "snapshot" LICENSE
check "empty list" "$ALL"
check "blank lines only" "$ALL" "" ""

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
echo "all passed"
