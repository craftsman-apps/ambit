#!/usr/bin/env bash
# The ambit CLI end to end on kind (spec 9.2 job cli-local, and the redaction and restricted-permission gates of
# 9.2a): local up with the test model, check (with --deep: a test call and its audit record), local up again
# with --verbose (repair; it keeps the test model), the password on a terminal only, check as a ServiceAccount
# bound to `view`, ambit connect and ambit disconnect for the three tools against the new install (step 6a), and
# local down. Every captured stream is searched for the password and the tokens. The fake
# OpenRouter key of 9.2a is tested with mocks in TestUpFakeKeyNeverPrinted (ruling S14), so no run here replaces
# the test model.
# Runs only in CI (CI=true): it creates a kind cluster, and a developer machine has room for one extra cluster at
# most (spec 9.4, ruling S15). Needs bin/ambit built with -tags dev, kind, kubectl, curl, python3 and script(1).
# CLI_ARCHIVES: image archives to load (the branch image).
set -euo pipefail
[[ "${CI:-}" == true ]] || { echo "test/cli-local.sh creates a kind cluster and runs only in CI (CI=true); on a developer machine run the unit tests" >&2; exit 2; }
cd "$(dirname "$0")/.."
NAME=${CLI_NAME:-ambit} PORT=${CLI_PORT:-8080} HTTPS_PORT=${CLI_HTTPS_PORT:-8443}
W=$(mktemp -d)
O=$W/out          # every captured stdout, stderr and JSON output; searched for secrets in step 8
mkdir "$O"
export KUBECONFIG=$W/kubeconfig   # never the runner's default kubeconfig
# A "previous" current context, so the test can see that local up gives it back (spec 4.1 step 2, kubeconfig).
kubectl config set-cluster previous --server=https://127.0.0.1:1 >/dev/null
kubectl config set-context previous --cluster=previous >/dev/null
kubectl config use-context previous >/dev/null
AMBIT=$PWD/bin/ambit
CTX=kind-$NAME
OWNED=0           # 1 once local up has established ownership (state file and node label); only then may cleanup delete
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  if [[ $OWNED == 1 ]]; then
    "$AMBIT" local down --name "$NAME" --yes >/dev/null 2>&1 || true
  else
    echo "cleanup: local up did not establish ownership of $NAME; the cluster is left for the CI runner's teardown" >&2
    kind get clusters >&2 || true
    kubectl --context "$CTX" get nodes -L ambitproject.dev/created-by >&2 2>/dev/null || true
  fi
}
trap cleanup EXIT

cat > "$W/dev-values.yaml" <<'YAML'
console: { image: { repository: ambit-console, tag: dev, pullPolicy: Never } }
YAML
UP=(local up --name "$NAME" --port "$PORT" --https-port "$HTTPS_PORT" --chart-dir charts --no-key --dev-values "$W/dev-values.yaml")
for a in ${CLI_ARCHIVES:-}; do UP+=(--dev-load-archive "$a"); done

# 1 · a fresh local up with the test model: every step passes and the test call answers
"$AMBIT" "${UP[@]}" > "$O/up1.log" 2>&1 || { cat "$O/up1.log"; fail "1 local up"; }
OWNED=1
grep -q "local/echo answered: You said: Say hello in five words." "$O/up1.log" || { cat "$O/up1.log"; fail "1 no test answer"; }
[[ $(kubectl config current-context) == previous ]] || fail "1 local up did not restore the current context (now $(kubectl config current-context))"
pass "1 local up created $NAME and the test call answered"

# 2 · check: health passes on the new install
"$AMBIT" check --context "$CTX" > "$O/check.log" 2> "$O/check.err" || { cat "$O/check.log" "$O/check.err"; fail "2 ambit check exited $?"; }
grep -q '^✓ H1 ' "$O/check.log" && grep -q '^✓ H4 ' "$O/check.log" && ! grep -q '^✗' "$O/check.log" || { cat "$O/check.log"; fail "2 check lines"; }
pass "2 ambit check: health passes"

# From here on the script holds secrets (the password and tokens): never trace them.
{ set +x; } 2>/dev/null

# 3 · check --deep --production: one test call per model, and the collector sends that call's record to ClickHouse (P4)
PW=$("$AMBIT" local password --name "$NAME")
TOK=$(curl -s "http://console.localtest.me:$PORT/dex/token" -d grant_type=password -d client_id=ambit-cli \
  -d scope="openid email profile" --data-urlencode username=admin@ambit.local --data-urlencode password="$PW" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
AMBIT_TOKEN=$TOK "$AMBIT" check --context "$CTX" --deep --production --verbose --output json > "$O/deep.json" 2> "$O/deep.err" || true
python3 - "$O/deep.json" <<'PY' || { cat "$O/deep.err"; fail "3 --deep results"; }
import json, sys
r = {x["id"]: x for x in json.load(open(sys.argv[1]))}
for id in ("H8", "P4"):
    assert r[id]["status"] == "pass", (id, r[id])
PY
pass "3 --deep: the models answer and the audit record leaves the collector"

# 4 · local up again, with --verbose: repair mode, idempotent, still the test model
"$AMBIT" "${UP[@]}" --verbose > "$O/up2.log" 2> "$O/up2.err" || { cat "$O/up2.log" "$O/up2.err"; fail "4 second local up"; }
grep -q "(exists; repairing)" "$O/up2.log" || fail "4 the second run did not repair"
grep -q "local/echo answered:" "$O/up2.log" || fail "4 the repair did not keep the test model"
pass "4 local up again repairs the same cluster and keeps the test model"

# 5 · on a terminal (and only there) the summary shows the password. The terminal log holds the password by
# design, so it is kept outside $O and checked on its own.
script -qec "$AMBIT ${UP[*]}" /dev/null > "$W/tty.log" 2>&1 || { fail "5 local up on a terminal (log not printed: it holds the password)"; }
grep -qF "Password: $PW" "$W/tty.log" || fail "5 no password in the terminal summary"
! grep -qF "Password: $PW" "$O/up1.log" "$O/up2.log" || fail "5 the password is in a summary without a terminal"
pass "5 the password is printed on a terminal only"

# 6 · restricted permissions: a ServiceAccount bound to `view` gets only pass, warn, fail, unknown or n/a
kubectl --context "$CTX" -n ambit-system create serviceaccount ambit-view >/dev/null
kubectl --context "$CTX" create clusterrolebinding ambit-view --clusterrole=view --serviceaccount=ambit-system:ambit-view >/dev/null
SATOK=$(kubectl --context "$CTX" -n ambit-system create token ambit-view)
kubectl --context "$CTX" config view --raw --minify > "$W/view.kubeconfig"
kubectl --kubeconfig "$W/view.kubeconfig" config set-credentials view --token="$SATOK" >/dev/null
kubectl --kubeconfig "$W/view.kubeconfig" config set-context --current --user=view >/dev/null
rc=0; "$AMBIT" check --kubeconfig "$W/view.kubeconfig" --production --verbose --output json > "$O/view.json" 2> "$O/view.err" || rc=$?
[[ $rc -le 1 ]] || { cat "$O/view.err"; fail "6 check as view exited $rc"; }
python3 - "$O/view.json" <<'PY' || fail "6 restricted results"
import json, sys
rs = json.load(open(sys.argv[1]))
assert len(rs) == 19, len(rs)                      # H1-H8 and P1-P11
for r in rs:
    assert r["status"] in ("pass", "warn", "fail", "unknown", "n/a"), r
    if r["status"] == "unknown":
        assert "cannot " in r["message"], r       # names the missing permission
assert any(r["status"] == "unknown" for r in rs)   # view cannot read Secrets, so some checks could not look
PY
pass "6 check with view only: every line ends in a valid status; unknown lines name the permission"

# 6a · ambit connect and ambit disconnect (spec 14 item 15): a personal key made through the console API (the only
# way keys are made), the three tools connected in an isolated home with seeded config files, the test calls
# passing through the gateway in each tool's own format (the test model implements only Completions; the gateway
# converts /v1/messages and /v1/responses), ambit key print, no key in any file but the key store, ambit
# disconnect --all giving every file back byte for byte, and the gateway refusing the key once it is revoked.
H=$W/home
mkdir -p "$H/.claude" "$H/.codex" "$H/.config/opencode"
printf '{\n  "theme": "dark",\n  "permissions": { "allow": ["Bash(ls)"] }\n}\n' > "$H/.claude/settings.json"
printf 'approval_policy = "on-request"\n\n[tui]\nnotifications = true\n' > "$H/.codex/config.toml"
printf '{\n  // my opencode setup\n  "theme": "tokyonight",\n}\n' > "$H/.config/opencode/opencode.json"
printf 'export PATH="$HOME/bin:$PATH"\nalias ll="ls -l"\n' > "$H/.bashrc"
mkdir "$W/seed"; cp "$H/.claude/settings.json" "$W/seed/claude"; cp "$H/.codex/config.toml" "$W/seed/codex"
cp "$H/.config/opencode/opencode.json" "$W/seed/opencode"; cp "$H/.bashrc" "$W/seed/bashrc"
cat > "$W/consoleapi.py" <<'PY'
import html, http.cookiejar, json, re, sys, time, urllib.error, urllib.parse, urllib.request
base, pw, mode = sys.argv[1:4]
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
r = op.open(base + "/auth/login?connector=local")
action = urllib.parse.urljoin(r.geturl(), html.unescape(re.search(r'action="([^"]*)"', r.read().decode()).group(1)))
op.open(action, urllib.parse.urlencode({"login": "admin@ambit.local", "password": pw}).encode()).read()
def call(method, path, body=None):
    req = urllib.request.Request(base + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with op.open(req) as r: return r.status, r.read()
    except urllib.error.HTTPError as e: return e.code, e.read()
if mode == "create":
    for _ in range(15):  # 503 while the console takes over the key Lease
        code, body = call("POST", "/api/keys", {"name": "cli-e2e", "scope": "llm", "days": 30})
        if code != 503: break
        time.sleep(2)
    assert code == 201, (code, body[:200])
    d = json.loads(body)
    open(sys.argv[4], "w").write(d["key"]); open(sys.argv[5], "w").write(d["view"]["id"])
else:
    code, body = call("DELETE", "/api/keys/" + open(sys.argv[4]).read())
    assert code in (200, 204), (code, body[:200])
PY
python3 "$W/consoleapi.py" "http://console.localtest.me:$PORT" "$PW" create "$W/e2e-key" "$W/e2e-key-id" || fail "6a creating a key through the console API"
KEY=$(cat "$W/e2e-key")
LLM=http://llm.localtest.me:$PORT
AM() { env HOME="$H" XDG_CONFIG_HOME="$H/.config" CODEX_HOME="$H/.codex" CLAUDE_CONFIG_DIR="$H/.claude" SHELL=/bin/bash AMBIT_API_KEY="${AM_KEY:-}" "$AMBIT" "$@"; }
[[ $(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"local/echo","messages":[{"role":"user","content":"hi"}]}' "$LLM/v1/chat/completions") == 200 ]] || fail "6a the new key does not work at the gateway"
AM_KEY=$KEY AM connect claude-code --url "$LLM" --model local/echo --console-url "http://console.localtest.me:$PORT" --yes > "$O/connect-claude.log" 2>&1 || { cat "$O/connect-claude.log"; fail "6a connect claude-code"; }
for tool in codex opencode; do
  AM connect "$tool" --url "$LLM" --model local/echo --yes > "$O/connect-$tool.log" 2>&1 || { cat "$O/connect-$tool.log"; fail "6a connect $tool"; }
done
for tool in claude codex opencode; do grep -q '^✓ test call:' "$O/connect-$tool.log" || fail "6a no test call line for $tool"; done
grep -q 'key print --url' "$H/.claude/settings.json" && grep -q 'model_provider = "ambit"' "$H/.codex/config.toml" \
  && grep -q 'opencode() { AMBIT_API_KEY=' "$H/.bashrc" && grep -q '"ambit"' "$H/.config/opencode/opencode.json" || fail "6a the tool files lack Ambit's entries"
[[ $(AM key print --url "$LLM") == "$KEY" ]] || fail "6a ambit key print did not print the stored key"
holders=$(grep -rlF -- "$KEY" "$H" || true)
[[ $(wc -l <<<"$holders" | tr -d ' ') == 1 && $holders == "$H/.config/ambit/keys/"* ]] || fail "6a the key is in files other than the key store: $holders"
[[ $(stat -c %a "$holders") == 600 ]] || fail "6a the key file is not mode 600"
# A dry run with another model plans a real change, and must write nothing: every file stays as it is, byte for byte.
mkdir -p "$O/before-dry"
cp "$H/.claude/settings.json" "$O/before-dry/settings.json"; cp "$H/.config/ambit/connect.json" "$O/before-dry/connect.json"
ls -A "$H/.claude" > "$O/before-dry/claude.ls"
AM connect claude-code --url "$LLM" --model local/other-model --dry-run > "$O/dryrun.log" 2>&1 || { cat "$O/dryrun.log"; fail "6a dry run"; }
grep -q 'local/other-model' "$O/dryrun.log" || fail "6a the dry run did not plan a change"
cmp "$O/before-dry/settings.json" "$H/.claude/settings.json" && cmp "$O/before-dry/connect.json" "$H/.config/ambit/connect.json" \
  && [[ $(ls -A "$H/.claude") == $(cat "$O/before-dry/claude.ls") ]] || fail "6a the dry run changed a file"
AM disconnect --all --yes > "$O/disconnect.log" 2>&1 || { cat "$O/disconnect.log"; fail "6a disconnect --all"; }
cmp "$W/seed/claude" "$H/.claude/settings.json" && cmp "$W/seed/codex" "$H/.codex/config.toml" \
  && cmp "$W/seed/opencode" "$H/.config/opencode/opencode.json" && cmp "$W/seed/bashrc" "$H/.bashrc" || fail "6a disconnect did not give the files back"
[[ -z $(ls "$H/.config/ambit/keys" 2>/dev/null) ]] || fail "6a the stored key was not deleted"
grep -q 'next: revoke the key in the console: http://console.localtest.me:' "$O/disconnect.log" || fail "6a no revoke line"
# Deleting the stored key does not revoke it: the key still works until the console revokes it.
[[ $(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"local/echo","messages":[{"role":"user","content":"hi"}]}' "$LLM/v1/chat/completions") == 200 ]] || fail "6a the key stopped working before it was revoked"
python3 "$W/consoleapi.py" "http://console.localtest.me:$PORT" "$PW" revoke "$W/e2e-key-id" || fail "6a revoking the key"
refused=
for _ in $(seq 1 45); do
  c=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"model":"local/echo","messages":[{"role":"user","content":"hi"}]}' "$LLM/v1/chat/completions")
  [[ $c == 401 || $c == 403 ]] && { refused=1; break; }; sleep 2
done
[[ -n $refused ]] || fail "6a the gateway still accepts the revoked key (HTTP $c)"
pass "6a connect and disconnect: three tools, test calls, no key in any file, files restored, revoked key refused"

# 7 · local down deletes the cluster (its output is captured in $O too, so step 8 scans it)
"$AMBIT" local down --name "$NAME" --yes > "$O/down.log" 2>&1 || { cat "$O/down.log"; fail "7 local down"; }
OWNED=0
! kind get clusters | grep -qx "$NAME" || fail "7 the cluster still exists"
pass "7 local down deleted $NAME"

# 8 · redaction (spec 9.2a): no captured stream holds the password, the Dex token or the ServiceAccount token
for f in "$O"/*; do
  for s in "$PW" "$TOK" "$SATOK" "$KEY"; do
    ! grep -qF -- "$s" "$f" || fail "8 a secret appears in $(basename "$f")"
  done
done
pass "8 no password or token in $(find "$O" -type f | wc -l | tr -d ' ') captured streams"
