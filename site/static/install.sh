#!/bin/sh
# Installs the ambit CLI on macOS or Linux. https://ambitproject.dev/docs/quick-start
#   curl -fsSL https://ambitproject.dev/install.sh | sh
# AMBIT_VERSION=0.2.0 pins a version (default: the latest release). AMBIT_INSTALL_DIR sets the directory
# (default: ~/.local/bin). AMBIT_DOWNLOAD_BASE replaces https://github.com/craftsman-apps/ambit/releases (for a mirror;
# $AMBIT_DOWNLOAD_BASE/latest must redirect to $AMBIT_DOWNLOAD_BASE/tag/<tag>, as GitHub does). AMBIT_LATEST_TIMEOUT sets
# the seconds to wait for that answer (default: 30).
set -eu

fail() {
  printf '✗ %s\n' "$1" >&2
  if [ -n "${2:-}" ]; then printf '  next: %s\n' "$2" >&2; fi
  exit 1
}

base=${AMBIT_DOWNLOAD_BASE:-https://github.com/craftsman-apps/ambit/releases}
dir=${AMBIT_INSTALL_DIR:-$HOME/.local/bin}

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "this script is for macOS and Linux, not $(uname -s)" "on Windows: irm https://ambitproject.dev/install.ps1 | iex" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "there is no ambit build for the processor $(uname -m)" "build it from source: go install github.com/craftsman-apps/ambit/cmd/ambit@latest" ;;
esac
archive="ambit_${os}_${arch}.tar.gz"

# fetch URL FILE: downloads, and sets $status to the HTTP status (000 when there was no answer).
fetch() {
  if command -v curl >/dev/null 2>&1; then
    status=$(curl -sSL -o "$2" -w '%{http_code}' "$1") || status=000
  elif command -v wget >/dev/null 2>&1; then
    status=$(wget -S -q -O "$2" "$1" 2>&1 | awk '/^ *HTTP\// { c = $2 } END { print c }')
    [ -n "$status" ] || status=000
  else
    fail "neither curl nor wget is installed" "install curl"
  fi
  [ "$status" = 200 ]
}

# latest_tag: asks once which release is the latest and sets $tag. The archive and checksums.txt are then both read
# from that tag; two requests to "latest" could reach two different releases.
latest_tag() {
  t=${AMBIT_LATEST_TIMEOUT:-30}
  if command -v curl >/dev/null 2>&1; then
    code=0
    reply=$(curl -sS --connect-timeout 10 --max-time "$t" -o /dev/null -w '%{http_code} %{redirect_url}' "$base/latest") || code=$?
    # curl exit 28: the time ran out. The status is 000 then, as for any other failure.
    [ "$code" = 28 ] && reply="timeout "
    [ "$code" = 0 ] || [ "$code" = 28 ] || reply="000 "
  elif command -v wget >/dev/null 2>&1; then
    reply=$(wget -S -q -T "$t" -t 1 -O /dev/null --max-redirect=0 "$base/latest" 2>&1 | tr -d '\r' |
      awk '/^ *HTTP\// { c = $2 } tolower($1) == "location:" { l = $2 } END { print c " " l }')
  else
    fail "neither curl nor wget is installed" "install curl"
  fi
  case $reply in
    *"/tag/"*) tag=${reply##*/tag/} ;;
    "timeout "* | 403" "* | 429" "* | 5??" "*) fail "GitHub did not answer; try again" "run the command again in a few minutes" ;;
    "000 "* | " "*) fail "cannot reach $base/latest" "check your network" ;;
    *) fail "No Ambit CLI release is published yet (the latest release has no ambit_<os>_<arch> file). See https://github.com/craftsman-apps/ambit/releases" ;;
  esac
}

# missing URL: a 404 on a latest download means that no CLI release exists yet.
missing() {
  if [ "$status" = 404 ] && [ -z "${AMBIT_VERSION:-}" ]; then
    fail "No Ambit CLI release is published yet (the latest release has no ambit_<os>_<arch> file). See https://github.com/craftsman-apps/ambit/releases"
  fi
  fail "cannot download $1 (HTTP $status)" "check AMBIT_VERSION and your network"
}

if [ -n "${AMBIT_VERSION:-}" ]; then
  tag="v${AMBIT_VERSION#v}"
else
  latest_tag
fi
url="$base/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fetch "$url/$archive" "$tmp/$archive" || missing "$url/$archive"
fetch "$url/checksums.txt" "$tmp/checksums.txt" || missing "$url/checksums.txt"

want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "checksums.txt has no line for $archive"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | cut -d ' ' -f 1)
else
  got=$(shasum -a 256 "$tmp/$archive" | cut -d ' ' -f 1)
fi
[ "$got" = "$want" ] || fail "the checksum of $archive does not match checksums.txt (got $got, want $want)" \
  "download again; if it happens again, report it at https://github.com/craftsman-apps/ambit/issues"

tar -xzf "$tmp/$archive" -C "$tmp" ambit
mkdir -p "$dir"
mv "$tmp/ambit" "$dir/ambit"
chmod 755 "$dir/ambit"
printf '✓ installed ambit to %s\n' "$dir/ambit"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) printf 'Add the directory to your PATH: export PATH="%s:$PATH"\n' "$dir" ;;
esac
printf 'Next: ambit local up\n'
