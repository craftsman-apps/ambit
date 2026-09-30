#!/usr/bin/env bash
# Journey S6: "a package you run turns out to be malicious — within an hour, list every running image and
# say whether it contains the named package version".
#   hack/inventory.sh                      every image running in the Ambit namespaces, by digest
#   hack/inventory.sh <package> [version]  which of those images contain the package (and which version)
# Images are scanned with syft (Anchore, Apache-2.0), pinned by digest; SBOMs are cached in .sbom/.
set -euo pipefail
cd "$(dirname "$0")/.."
SYFT=anchore/syft:v1.52.0@sha256:500e2d872ac019436926e8322b4fc1f39441d94d21f6f4046c6ff29b30e8cb02
NAMESPACES=${NAMESPACES:-"ambit-system ambit-system-grants argocd"}
PKG=${1:-}; VER=${2:-}
mkdir -p .sbom

# imageID is the digest actually running (image may be a tag); both are shown.
images=$(for ns in $NAMESPACES; do
  kubectl -n "$ns" get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.image}{"\t"}{.imageID}{"\n"}{end}{end}' 2>/dev/null || true
done | sort -u)
if [[ -z "$PKG" ]]; then
  printf '%s\n' "$images" | awk -F'\t' '{printf "%-95s %s\n", $1, $2}'
  exit 0
fi

hits=0
while IFS=$'\t' read -r image id; do
  [[ -z "$image" ]] && continue
  key=$(printf '%s' "${id:-$image}" | shasum -a 256 | cut -c1-16)
  sbom=.sbom/$key.json
  if [[ ! -s "$sbom" ]]; then
    # Local images (kind) are scanned from the Docker daemon; others are pulled by digest.
    src="registry:${id#*://}"
    [[ "$id" == *"sha256:"* && "$image" != *.local* && "$image" == */* ]] || src="docker:$image"
    docker image inspect "$image" >/dev/null 2>&1 && src="docker:$image"
    docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$SYFT" "$src" -o syft-json -q >"$sbom" 2>/dev/null || { echo "could not scan $image" >&2; rm -f "$sbom"; continue; }
  fi
  found=$(uv run --no-project python - "$sbom" "$PKG" "$VER" <<'PY'
import json, sys
sbom, pkg, ver = sys.argv[1:4]
arts = json.load(open(sbom)).get("artifacts", [])
m = sorted({a["version"] for a in arts if a.get("name") == pkg and (not ver or a.get("version") == ver)})
print(" ".join(m))
PY
)
  if [[ -n "$found" ]]; then
    hits=$((hits + 1))
    echo "CONTAINS $PKG $found  $image  $id"
  fi
done <<<"$images"
echo "$hits image(s) contain $PKG${VER:+ $VER}"
