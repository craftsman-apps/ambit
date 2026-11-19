#!/usr/bin/env bash
# Evidence exports (issue #89, Settings → Evidence):
#   1 the export queries on this ClickHouse (go test in the ClickHouse pod, scratch database): only gateway records in
#     calls.csv, its rows equal count() of the same period and kinds, the one-person filter, the export's settings
#   2 the audit export through the console for a closed period with a model call and a wrong-key call: the ZIP opens,
#     manifest.json is last, each file's rows and SHA-256 equal a recomputation, calls.csv has exactly the gateway
#     records of SELECT count() for the same period and kinds, the wrong-key row has its client address, daily-counts
#     adds up; the export history has the ZIP's SHA-256
#   3 a viewer gets 403 on both endpoints, with no ZIP
#   4 the offboarding export: keytest calls a model, is deactivated through SCIM and calls again (refused); the export's
#     row for keytest has a last accepted call before, 0 accepted calls after and a first refused call after
#   5 the access export for today: grants.csv equals the AccessRules (kubectl), with the published rules version; after
#     a rule change the manifest lists both snapshots of the day
# Run as one locked unit (CLAUDE.md): lockf /tmp/ambit-kind.lock sh -c 'make image install && test/stepevidence.sh'
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
LLM=http://llm.localtest.me:8080/v1/chat/completions
CH_POD=ambit-clickhouse-0
G=ambitproject.dev
SCIM_ID=""; DEACTIVATED=""
PATCH_ON='{"Operations":[{"op":"replace","path":"active","value":true}]}'
PATCH_OFF='{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}'
scim() { # scim METHOD PATH [JSON]: as the identity provider; body in $W/api
  local -a a=(-s -o "$W/api" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $SCIM_TOKEN" -H 'Content-Type: application/scim+json')
  [[ $# -ge 3 ]] && a+=(-d "$3")
  command curl "${a[@]}" "$BASE/scim/v2$2"
}
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
mock() { command curl -s -o "$W/llm" -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"ping"}]}' "$LLM"; }
ch() { kubectl -n "$NS" exec "$CH_POD" -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
cleanup() {
  [[ -n "$DEACTIVATED" ]] && { wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]]'; } >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "accessrules.$G" evidence-everyone-mock evidence-snapshot --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" exec "$CH_POD" -- rm -f /tmp/console.test /tmp/otel_logs.sql >/dev/null 2>&1 || true
  ch "DROP DATABASE IF EXISTS evidence_test SYNC" >/dev/null 2>&1 || true
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" rollout status statefulset/ambit-clickhouse --timeout=300s >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-otel-collector --timeout=180s >/dev/null

# 1 · the queries on the pinned ClickHouse. The test binary runs in the pod, which has the password in its environment.
command -v go >/dev/null || fail "1 needs go on the PATH"
arch=$(kubectl -n "$NS" exec "$CH_POD" -- uname -m); case $arch in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; esac
GOOS=linux GOARCH=$arch CGO_ENABLED=0 go test -c -o "$W/console.test" ./internal/console || fail "1 cannot build the console tests"
ch "SHOW CREATE TABLE otel.otel_logs FORMAT TSVRaw" > "$W/otel_logs.sql"
kubectl -n "$NS" exec -i "$CH_POD" -- sh -c 'cat > /tmp/console.test && chmod +x /tmp/console.test' < "$W/console.test"
kubectl -n "$NS" exec -i "$CH_POD" -- sh -c 'cat > /tmp/otel_logs.sql' < "$W/otel_logs.sql"
ch "DROP DATABASE IF EXISTS evidence_test SYNC" >/dev/null; ch "CREATE DATABASE evidence_test" >/dev/null
out=$(kubectl -n "$NS" exec "$CH_POD" -- sh -c "cd /tmp && env AMBIT_TEST_CLICKHOUSE_ADDR=127.0.0.1:8123 AMBIT_TEST_CLICKHOUSE_USER=ambit AMBIT_TEST_CLICKHOUSE_PASSWORD=\"\$CLICKHOUSE_PASSWORD\" AMBIT_TEST_OTEL_LOGS_DDL=/tmp/otel_logs.sql ./console.test -test.run '^(TestEvidenceOnClickHouse|TestOffboardingEvidenceOnClickHouse|TestAccessEvidenceOnClickHouse)\$' -test.v" 2>&1) ||
  { echo "$out" | tail -30; fail "1 the export queries on ClickHouse"; }
grep -q -- "--- PASS: TestEvidenceOnClickHouse" <<<"$out" && grep -q -- "--- PASS: TestOffboardingEvidenceOnClickHouse" <<<"$out" && grep -q -- "--- PASS: TestAccessEvidenceOnClickHouse" <<<"$out" || fail "1 tests did not run: $(tail -5 <<<"$out")"
pass "1 export queries on ClickHouse $(ch 'SELECT version()'): only gateway records, count matches, one-person filter, offboarding grace and accepted statuses, snapshot lookup, $(grep -o 'retention [^ ]* from [a-z]*' <<<"$out" || true)"

# 2 · a closed period [from, to) around two requests: user09's model call and a wrong key
from=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 1
M=$(token user09)
code=$(command curl -s -o "$W/llm" -w '%{http_code}' -H "Authorization: Bearer $M" -H 'Content-Type: application/json' \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"ping"}]}' "$LLM")
[[ "$code" == 200 ]] || fail "2 user09's model call answered $code: $(head -c 200 "$W/llm")"
code=$(command curl -s -o "$W/llm" -w '%{http_code}' -H "Authorization: Bearer ambit_evid1evid1evid1evid1evid1evid1evid1evid1" -H 'Content-Type: application/json' \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"ping"}]}' "$LLM")
[[ "$code" == 403 ]] || fail "2 the wrong key was answered $code"
inperiod() { echo "Timestamp >= parseDateTime64BestEffort('$from', 9) AND Timestamp < parseDateTime64BestEffort('$1', 9)"; }
for _ in $(seq 1 60); do
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  got=$(ch "SELECT countIf(LogAttributes['jwt.email'] = 'user09@example.com'), countIf(LogAttributes['ambit.identity'] = 'invalid key') FROM otel.otel_logs WHERE $(inperiod "$now")")
  [[ "$got" == $'1\t1' ]] && break; sleep 2
done
[[ "$got" == $'1\t1' ]] || fail "2 the two requests did not reach otel_logs: '$got'"
sleep 2
to=$(date -u +%Y-%m-%dT%H:%M:%SZ)
want=$(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['ambit.kind'] = '' AND $(inperiod "$to")")
sign_in keycloak user10 user10-test-pass
code=$(command curl -s -b "$W/jar" -D "$W/zip-headers" -o "$W/audit.zip" -w '%{http_code}' "$BASE/api/evidence/audit.zip?from=$from&to=$to")
[[ "$code" == 200 ]] || fail "2 audit export answered $code: $(head -c 300 "$W/audit.zip")"
grep -qi '^content-type: application/zip' "$W/zip-headers" || fail "2 not a ZIP: $(cat "$W/zip-headers")"
sum=$(uv run --no-project python - "$W/audit.zip" "$want" <<'PY'
import csv, hashlib, io, json, sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
names = z.namelist()
assert names[-1] == "manifest.json", names
m = json.loads(z.read("manifest.json"))
for f in m["files"]:
    b = z.read(f["name"])
    rows = list(csv.reader(io.StringIO(b.decode("utf-8"), newline="")))
    assert hashlib.sha256(b).hexdigest() == f["sha256"], ("sha256", f["name"])
    assert len(rows) - 1 == f["rows"], ("rows", f["name"], len(rows) - 1, f["rows"])
calls = list(csv.DictReader(io.StringIO(z.read("calls.csv").decode("utf-8"), newline="")))
assert len(calls) == int(sys.argv[2]), ("calls.csv", len(calls), "count()", sys.argv[2])
assert any(c["email"] == "user09@example.com" and c["model"] == "mock/echo" and c["status"] == "200" for c in calls), "user09's call"
bad = [c for c in calls if c["identity"] == "invalid key"]
assert bad and all(c["client_address"] and c["email"] == "" for c in bad), ("wrong-key row", bad)
assert all(c["client_address"] == "" for c in calls if c["identity"] != "invalid key"), "client address outside wrong-key rows"
days = list(csv.DictReader(io.StringIO(z.read("daily-counts.csv").decode("utf-8"), newline="")))
assert sum(int(d["records"]) for d in days) == len(calls), "daily counts"
assert m["parameters"]["email"] == "" and m["sources"]["auditRetentionFrom"] == "table", m["sources"]
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
PY
) || fail "2 the ZIP does not match its manifest or the store"
[[ $(api GET /api/evidence) == 200 ]] || fail "2 GET /api/evidence: $(cat "$W/api")"
hist=$(uv run --no-project python -c "import json;h=json.load(open('$W/api'))['history'][0];print(h['result'], h['zipSha256'], h['admin'])")
[[ "$hist" == "complete $sum user10@example.com" ]] || fail "2 export history '$hist', ZIP $sum"
pass "2 audit export of $want records: manifest rows and SHA-256 match, calls.csv = count() of gateway records, wrong-key row with its address; history has ZIP $sum"

# 3 · a viewer may not export
sign_in keycloak user01 user01-test-pass
[[ $(api GET /api/evidence) == 403 ]] || fail "3 a viewer read GET /api/evidence"
code=$(command curl -s -b "$W/jar" -o "$W/viewer.zip" -w '%{http_code}' "$BASE/api/evidence/audit.zip?from=$from&to=$to")
[[ "$code" == 403 && $(head -c 2 "$W/viewer.zip") != PK ]] || fail "3 a viewer's export answered $code"
pass "3 a viewer gets 403 on both endpoints"

# 4 · offboarding: a call before, the SCIM deactivation, a refused call after
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: evidence-everyone-mock }
spec: { allSignedIn: true, models: [mock-echo] }
EOF
SCIM_TOKEN=$(kubectl -n "$NS" get secret ambit-console-keys -o jsonpath='{.data.scim-token}' | base64 -d)
c=$(scim POST /Users '{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"keytest@example.com","active":true,"emails":[{"value":"keytest@example.com","primary":true}]}')
if [[ "$c" == 409 ]]; then  # the SCIM user stays after stepkeys or a rerun; make sure it is active
  [[ $(scim GET '/Users?filter=userName%20eq%20%22keytest@example.com%22') == 200 ]] || fail "4 scim lookup"
  SCIM_ID=$(field 'd["Resources"][0]["id"]')
  { wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_OFF") == 200 ]]' && wait_for 60 '[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]]'; } || fail "4 reset keytest"
else
  [[ "$c" == 201 ]] || fail "4 scim create: $c $(cat "$W/api")"
  SCIM_ID=$(field 'd["id"]')
fi
wait_for 60 '[[ $(mock "$(token keytest)") == 200 ]]' || fail "4 keytest's model call before offboarding: $(head -c 200 "$W/llm")"
sleep 1
DEACTIVATED=1
[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_OFF") == 200 ]] || fail "4 deactivate: $(cat "$W/api")"
wait_for 10 '[[ $(mock "$(token keytest)") == 403 ]]' || fail "4 the offboarded person's call was not refused"
sleep 6  # past the 5 s grace time
[[ $(mock "$(token keytest)") == 403 ]] || fail "4 the offboarded person's later call was not refused"
for _ in $(seq 1 60); do
  n=$(ch "SELECT count() FROM otel.otel_logs WHERE LogAttributes['jwt.email'] = 'keytest@example.com' AND Timestamp >= now() - INTERVAL 2 MINUTE AND (LogAttributes['http.status'] = '403' OR LogAttributes['http.response.status_code'] = '403')")
  [[ "$n" -ge 2 ]] && break; sleep 2
done
[[ "$n" -ge 2 ]] || fail "4 the refused calls did not reach otel_logs ($n)"
sign_in keycloak user10 user10-test-pass
day=$(date -u +%Y-%m-%d)
code=$(command curl -s -b "$W/jar" -o "$W/off.zip" -w '%{http_code}' "$BASE/api/evidence/offboarding.zip?from=$day&to=$day")
[[ "$code" == 200 ]] || fail "4 offboarding export answered $code: $(head -c 300 "$W/off.zip")"
row=$(uv run --no-project python - "$W/off.zip" <<'PY'
import csv, hashlib, io, json, sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
assert z.namelist()[-1] == "manifest.json"
m = json.loads(z.read("manifest.json"))
for f in m["files"]:
    assert hashlib.sha256(z.read(f["name"])).hexdigest() == f["sha256"], f["name"]
rows = [r for r in csv.DictReader(io.StringIO(z.read("offboarding.csv").decode("utf-8"), newline="")) if r["email"] == "keytest@example.com"]
assert rows, "offboarding.csv has no row for keytest@example.com"
r = rows[-1]
print(r["gateway_blocked"], "last-before" if r["last_accepted_call_before"] else "no-last-before", r["accepted_calls_after"], "refused-after" if r["first_refused_call_after"] else "no-refused-after")
PY
) || fail "4 the offboarding ZIP"
[[ "$row" == "true last-before 0 refused-after" ]] || fail "4 keytest's offboarding row: '$row'"
[[ $(scim PATCH "/Users/$SCIM_ID" "$PATCH_ON") == 200 ]] || fail "4 reactivate: $(cat "$W/api")"
DEACTIVATED=""
pass "4 offboarding export: keytest's row is blocked, with a call before, 0 accepted calls after and a refused call after"
# 5 · access review for today: grants.csv is the AccessRules in force, with the published rules version; after a rule
#     change the manifest lists both snapshots of the day
published() { kubectl -n "$NS" get configmap ambit-access-mode -o jsonpath='{.data.rulesVersion}'; }
newest_snap() { ch "SELECT LogAttributes['ambit.rulesVersion'] FROM otel.otel_logs WHERE LogAttributes['ambit.kind'] = 'access-snapshot' ORDER BY Timestamp DESC LIMIT 1"; }
access_zip() {
  local code
  code=$(command curl -s -b "$W/jar" -o "$W/access.zip" -w '%{http_code}' "$BASE/api/evidence/access.zip?date=$(date -u +%Y-%m-%d)")
  [[ "$code" == 200 ]] || fail "5 access export answered $code: $(head -c 300 "$W/access.zip")"
}
for _ in $(seq 1 60); do [[ "$(newest_snap)" == "$(published)" ]] && break; sleep 2; done
[[ "$(newest_snap)" == "$(published)" ]] || fail "5 no access snapshot of the rules in force ($(published)): '$(newest_snap)'"
v1=$(published)
sign_in keycloak user10 user10-test-pass
access_zip
kubectl -n "$NS" get accessrules -o json > "$W/rules.json"
uv run --no-project python - "$W/access.zip" "$W/rules.json" "$v1" <<'PY' || fail "5 grants.csv is not the AccessRules in force"
import csv, hashlib, io, json, sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
m = json.loads(z.read("manifest.json"))
for f in m["files"]:
    assert hashlib.sha256(z.read(f["name"])).hexdigest() == f["sha256"], f["name"]
got = {(r["rule"], r["who_type"], r["who"], r["what_type"], r["what"]) for r in csv.DictReader(io.StringIO(z.read("grants.csv").decode("utf-8"), newline=""))}
versions = {r["rules_version"] for r in csv.DictReader(io.StringIO(z.read("grants.csv").decode("utf-8"), newline=""))}
want = set()
for o in json.load(open(sys.argv[2]))["items"]:
    s = o["spec"]
    who = [("group", g) for g in s.get("groups", [])] + [("service account" if u.endswith("@svc.ambit.internal") else "user", u) for u in s.get("users", [])]
    if s.get("allSignedIn"):
        who.append(("all-signed-in", "*"))
    what = [("model", x) for x in s.get("models", [])]
    for sv in s.get("servers", []):
        what += [("tool", sv["name"] + "/" + t) for t in sv["tools"]] if sv.get("tools") else [("server", sv["name"])]
    want |= {(o["metadata"]["name"], a, b, c, d) for a, b in who for c, d in what}
def guard(v):
    return "'" + v if v[:1] in ("=", "+", "-", "@", "\t", "\r") else v
want = {tuple(guard(x) for x in t) for t in want}
assert got == want, ("missing", sorted(want - got)[:5], "extra", sorted(got - want)[:5])
assert versions <= {sys.argv[3]}, versions
assert m["sources"]["snapshot"]["rulesVersion"] == sys.argv[3]
PY
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: AccessRule
metadata: { name: evidence-snapshot }
spec: { groups: [evidence-snapshot], models: [mock-echo] }
EOF
for _ in $(seq 1 60); do [[ "$(published)" != "$v1" && "$(newest_snap)" == "$(published)" ]] && break; sleep 2; done
v2=$(published)
[[ "$v2" != "$v1" && "$(newest_snap)" == "$v2" ]] || fail "5 the rule change wrote no snapshot ($v1 -> $v2, newest '$(newest_snap)')"
access_zip
day=$(uv run --no-project python -c "import json,sys,zipfile;m=json.loads(zipfile.ZipFile(sys.argv[1]).read('manifest.json'));print(' '.join(sorted({s['rulesVersion'] for s in m['sources']['snapshotsOfTheDay']})), m['sources']['snapshot']['rulesVersion'])" "$W/access.zip")
[[ "$day" == *"$v1"* && "$day" == *"$v2"* && "$day" == *" $v2" ]] || fail "5 snapshots of the day: '$day' (want $v1 and $v2, in force $v2)"
kubectl -n "$NS" delete "accessrules.$G" evidence-snapshot --ignore-not-found >/dev/null
pass "5 access export: grants.csv equals the AccessRules (rules version $v1); after a change the day lists $v1 and $v2"
done_step evidence
