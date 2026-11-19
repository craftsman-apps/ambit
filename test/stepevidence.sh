#!/usr/bin/env bash
# Evidence exports (issue #89, Settings → Evidence):
#   1 the export queries on this ClickHouse (go test in the ClickHouse pod, scratch database): only gateway records in
#     calls.csv, its rows equal count() of the same period and kinds, the one-person filter, the export's settings
#   2 the audit export through the console for a closed period with a model call and a wrong-key call: the ZIP opens,
#     manifest.json is last, each file's rows and SHA-256 equal a recomputation, calls.csv has exactly the gateway
#     records of SELECT count() for the same period and kinds, the wrong-key row has its client address, daily-counts
#     adds up; the export history has the ZIP's SHA-256
#   3 a viewer gets 403 on both endpoints, with no ZIP
# Run as one locked unit (CLAUDE.md): lockf /tmp/ambit-kind.lock sh -c 'make image install && test/stepevidence.sh'
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
LLM=http://llm.localtest.me:8080/v1/chat/completions
CH_POD=ambit-clickhouse-0
ch() { kubectl -n "$NS" exec "$CH_POD" -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
cleanup() {
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
out=$(kubectl -n "$NS" exec "$CH_POD" -- sh -c "cd /tmp && env AMBIT_TEST_CLICKHOUSE_ADDR=127.0.0.1:8123 AMBIT_TEST_CLICKHOUSE_USER=ambit AMBIT_TEST_CLICKHOUSE_PASSWORD=\"\$CLICKHOUSE_PASSWORD\" AMBIT_TEST_OTEL_LOGS_DDL=/tmp/otel_logs.sql ./console.test -test.run '^TestEvidenceOnClickHouse\$' -test.v" 2>&1) ||
  { echo "$out" | tail -30; fail "1 the export queries on ClickHouse"; }
grep -q -- "--- PASS: TestEvidenceOnClickHouse" <<<"$out" || fail "1 test did not run: $(tail -5 <<<"$out")"
pass "1 export queries on ClickHouse $(ch 'SELECT version()'): only gateway records, count matches, one-person filter, $(grep -o 'retention [^ ]* from [a-z]*' <<<"$out" || true)"

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
done_step evidence
