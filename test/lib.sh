# Shared helpers for the step tests. Source after setting NS.
BASE=http://console.localtest.me:8080
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; exit 1; }

# hop METHOD URL [curl args]: one request with the shared cookie jar; sets CODE and LOC.
hop() {
  local m=$1 u=$2; shift 2
  curl -s -X "$m" -o "$W/body" -D "$W/hdr" -b "$W/jar" -c "$W/jar" "$@" "$u"
  CODE=$(awk 'NR==1{print $2}' "$W/hdr")
  LOC=$(awk 'tolower($1)=="location:"{print $2}' "$W/hdr" | tr -d '\r')
  if [[ -n "$LOC" && "$LOC" != http* ]]; then LOC="$(sed -E 's|^(https?://[^/]+).*|\1|' <<<"$u")$LOC"; fi
  cat "$W/hdr" >> "$W/all-headers"
}
# follow: keep following redirects until a non-redirect response.
follow() { local n=0; while [[ "$CODE" =~ ^30 && $n -lt 15 ]]; do hop GET "$LOC"; n=$((n+1)); done; }
# form_action: the first <form action> on the last page, made absolute.
form_action() {
  local a; a=$(grep -o 'action="[^"]*"' "$W/body" | head -1 | cut -d'"' -f2 | sed 's/&amp;/\&/g')
  [[ -z "$a" || "$a" == http* ]] && echo "$a" || echo "$BASE$a"
}
# sign_in CONNECTOR USER PASSWORD: Dex local form (login=) or Keycloak form (username=).
sign_in() {
  rm -f "$W/jar" "$W/all-headers"
  hop GET "$BASE/auth/login?connector=$1"; follow
  local action; action=$(form_action)
  [[ -n "$action" ]] || return 1
  if [[ "$1" == local ]]; then
    hop POST "$action" --data-urlencode "login=$2" --data-urlencode "password=$3"
  else
    hop POST "$action" --data-urlencode "username=$2" --data-urlencode "password=$3"
  fi
  follow
}
me_status() { curl -s -o "$W/me" -w '%{http_code}' -b "$W/jar" "$BASE/api/me"; }
me_field() { python3 -c "import json,sys;v=json.load(open('$W/me')).get('$1');print(json.dumps(v))"; }
