#!/usr/bin/env bash
# Which ci.yml jobs a change needs. Reads the changed file names (one per line) on stdin and prints one line per job
# group, `name=true|false`, for $GITHUB_OUTPUT. Safe by default: a file no rule below names runs every job, and so
# does an empty list (a push, or no pull request base). Only files named as unrelated skip a job.
#   build       build (and the image the jobs below download)
#   cli_unit    cli-unit on three systems
#   interop     cli-helm-interop
#   snapshot    cli-snapshot (GoReleaser, npm and Homebrew packaging)
#   cli_kind    cli-local, cli-partial
#   quickstart  quickstart
#   e2e         e2e (a), e2e (b)
set -euo pipefail

ALL="build cli_unit interop snapshot cli_kind quickstart e2e"
IMAGE="build cli_kind quickstart e2e"  # jobs that run the branch image and the charts on kind
on=" "
add() { on+="$* "; }

n=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  n=$((n + 1))
  case "$f" in
    # Read by Go tests or the CLI's tests, although they look like documentation.
    site/static/install.sh | site/static/install.ps1) add build cli_unit ;;  # internal/installscript
    docs/presets.md) add build ;;                                            # internal/presets test
    # Unrelated to every job here.
    docs/* | site/* | *.md | .gitignore | .github/dependabot.yml) ;;
    .github/workflows/site.yml | .github/workflows/release.yml | .github/workflows/codeql.yml) ;;
    .github/workflows/presets-contract.yml | .github/workflows/ci-cache.yml) ;;
    # Release packaging: GoReleaser puts LICENSE and NOTICE in every archive.
    LICENSE | NOTICE | .goreleaser.yaml) add snapshot ;;
    scripts/npm/* | scripts/homebrew/*) add build snapshot ;;
    # The console embeds web/; internal/cli/connect's test reads tool-setup.ts.
    web/src/lib/tool-setup.ts) add $IMAGE cli_unit ;;
    web/*) add $IMAGE ;;
    # internal/cli/checks tests read the charts.
    charts/*) add $IMAGE cli_unit ;;
    Dockerfile | .dockerignore | Makefile | hack/*) add $IMAGE ;;
    test/cli-local.sh | test/cli-partial.sh) add build cli_kind ;;
    test/quickstart.sh) add build quickstart ;;
    test/lib.sh) add build quickstart e2e ;;
    test/chart.sh | test/alerts.sh | test/alerts/*) add build ;;
    test/llm/objects.yaml | test/mcp/objects.yaml) add build cli_unit e2e ;;  # internal/validate test
    test/*) add build e2e ;;
    # Go code, and deploy/ and kind/ (embedded in the binaries), run everything. So do ci.yml, this script, and any
    # file not named above.
    *) add $ALL ;;
  esac
done
[ "$n" -gt 0 ] || add $ALL

for j in $ALL; do
  case "$on" in *" $j "*) echo "$j=true" ;; *) echo "$j=false" ;; esac
done
