#!/usr/bin/env bash
# Tests that the Scoop and winget steps of .github/workflows/release.yml skip with a notice, and exit 0, while the
# owner actions O1, O2 and O4 are not done (spec D2 8.4), so a release without those secrets does not fail. Each
# step's script is taken from release.yml and run as GitHub runs it; git, node and curl are stubs, so nothing is
# written or downloaded. The pwsh case runs only where pwsh is installed (it is on GitHub's Ubuntu runners).
set -euo pipefail
cd "$(dirname "$0")/../.."
fails=0
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT

# step <name prefix>: the run script of that step of release.yml.
step() {
  python3 - "$1" <<'EOF'
import sys, yaml
for job in yaml.safe_load(open(".github/workflows/release.yml"))["jobs"].values():
    for s in job.get("steps", []):
        if s.get("name", "").startswith(sys.argv[1]):
            print(s["run"]); sys.exit(0)
sys.exit("no step " + sys.argv[1])
EOF
}

# Stubs: anything that would write or download fails the case, except curl's status-code probe (which answers $PROBE).
mkdir "$W/bin"
for c in git node tar sha256sum; do printf '#!/bin/sh\necho "unexpected: %s $*" >&2; exit 97\n' "$c" > "$W/bin/$c"; done
cat > "$W/bin/curl" <<'EOF'
#!/bin/sh
case "$*" in *"%{http_code}"*) printf '%s' "$PROBE"; exit 0;; esac
echo "unexpected: curl $*" >&2; exit 97
EOF
chmod +x "$W/bin"/*

# check <name> <want notice> <script file> [env...]: the script exits 0 and prints the notice.
check() {
  local name=$1 want=$2 file=$3 out rc; shift 3
  out=$(cd "$W" && env PATH="$W/bin:$PATH" GITHUB_REF_NAME=v0.5.0 RUNNER_TEMP="$W" "$@" bash --noprofile --norc -eo pipefail "$file" 2>&1) && rc=0 || rc=$?
  if [ "$rc" = 0 ] && [[ $out == *"$want"* ]]; then echo "ok   $name"; else echo "FAIL $name: exit $rc, output: $out"; fails=$((fails + 1)); fi
}

step "Scoop bucket" > "$W/scoop.sh"
step "winget pull request" > "$W/winget.sh"
check "Scoop without SCOOP_BUCKET_TOKEN" "::notice::SCOOP_BUCKET_TOKEN is not set: Scoop skipped (owner actions O1, O2)" "$W/scoop.sh" BUCKET_TOKEN=
check "winget without WINGET_TOKEN" "::notice::WINGET_TOKEN is not set: winget skipped (owner action O4)" "$W/winget.sh" GITHUB_TOKEN=
check "winget before the first submission (O3)" "::notice::CraftsmanApps.Ambit is not in microsoft/winget-pkgs yet" "$W/winget.sh" GITHUB_TOKEN=test PROBE=404

if command -v pwsh >/dev/null; then
  # pwsh: Invoke-RestMethod is replaced by a function (functions win over cmdlets), so no request is sent.
  pcheck() {  # pcheck <name> <want notice> <Invoke-RestMethod body>
    local out rc
    { echo "function Invoke-RestMethod { $3 }"; step "scoop install ambit/ambit"; } > "$W/scoop.ps1"
    out=$(cd "$W" && GITHUB_REF_NAME=v0.5.0 GH_TOKEN=test pwsh -NoProfile -NonInteractive -Command ". '$W/scoop.ps1'" 2>&1) && rc=0 || rc=$?
    if [ "$rc" = 0 ] && [[ $out == *"$2"* ]]; then echo "ok   $1"; else echo "FAIL $1: exit $rc, output: $out"; fails=$((fails + 1)); fi
  }
  pcheck "Windows scoop install without the bucket (O1)" "has no bucket/ambit.json of version 0.5.0 (found: none)" "throw 'Not Found'"
  pcheck "Windows scoop install with an older manifest (O2)" "has no bucket/ambit.json of version 0.5.0 (found: 0.4.0)" \
    "@{ content = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('{\"version\":\"0.4.0\"}')) }"
else
  echo "skip the pwsh cases: pwsh is not installed"
fi
[ "$fails" -eq 0 ]
