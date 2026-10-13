#!/usr/bin/env bash
# External databases: Ambit installed against a ClickHouse and a Valkey it does not run, as a customer
# would use ClickHouse Cloud and ElastiCache — TLS from a private CA, passwords, and a non-default
# ClickHouse user and database. The chart then installs neither, and audit and budgets still work.
set -euo pipefail
NS=${1:-ambit-system}
EXT=ambit-ext
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
LLM=http://llm.localtest.me:8080/v1/chat/completions
field() { uv run --no-project python -c "import json;d=json.load(open('$W/api'));print($1)"; }
outcome() { # blocked (the gateway refused on budget) or passed:<code>
  local code; code=$(command curl -s -D "$W/h" -o "$W/llm" -w '%{http_code}' --max-time 60 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d '{"model":"google/gemma-4-31b-it:free","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' "$LLM")
  if [[ "$code" == 429 ]] && grep -q '"code":"budget_exceeded"' "$W/llm" && ! grep -q 'Provider returned error' "$W/llm"; then echo blocked; else echo "passed:$code"; fi
}
# Passwords stay inside the containers (their own environment), never in kubectl's arguments.
ch() { kubectl -n "$EXT" exec deploy/clickhouse -- sh -c 'clickhouse-client --user ambit_ext --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
vk() { kubectl -n "$EXT" exec deploy/valkey -- sh -c 'REDISCLI_AUTH="$VK_PASSWORD" valkey-cli --tls --cacert /tls/ca.crt -p 6380 "$@"' sh "$@"; }
bundled_ch() { kubectl -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "$1"; }
restore() {
  kubectl -n "$NS" delete "budgets.$G" ext-eng --ignore-not-found >/dev/null 2>&1 || true
  helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null 2>&1 || true
  kubectl -n "$NS" delete secret ext-clickhouse ext-valkey ext-ca --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete namespace "$EXT" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap 'rc=$?; restore; on_exit $rc' EXIT

# R · the chart's choices, rendered (no cluster change): settings left behind after switching back to the
#     bundled ClickHouse are ignored, and a CA without TLS never turns TLS on (the collector's exporter
#     does TLS whenever it has a CA file).
render() { helm template ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml "$@"; }
r=$(render --set evidence.clickhouse.external.user=other --set evidence.clickhouse.external.database=other --set evidence.clickhouse.external.caSecret=ext-ca)
[[ "$r" == *"username: ambit"* && "$r" == *"database: otel"* && "$r" != *ca_file* && "$r" != *clickhouse-ca* && "$r" == *"kind: StatefulSet"* ]] ||
  fail "R external ClickHouse settings without a host leak into the bundled setup"
r=$(render --set evidence.clickhouse.external.host=ch.example --set evidence.clickhouse.external.tls=false --set evidence.clickhouse.external.nativePort=9000 \
  --set evidence.clickhouse.external.httpPort=8123 --set evidence.clickhouse.external.passwordSecret=pw --set evidence.clickhouse.external.caSecret=ca)
[[ "$r" == *"tcp://ch.example:9000?dial_timeout=10s"* && "$r" != *"secure=true"* && "$r" != *ca_file* && "$r" != *AMBIT_CLICKHOUSE_TLS* && "$r" != *AMBIT_CLICKHOUSE_CA* ]] ||
  fail "R ClickHouse with tls=false still gets TLS settings"
r=$(render --set limits.valkey.external.address=vk.example:6379 --set limits.valkey.external.tls=false --set limits.valkey.external.caSecret=ca)
[[ "$r" == *'"vk.example:6379"'* && "$r" != *REDIS_TLS* && "$r" != *AMBIT_VALKEY_TLS* && "$r" != *valkey-ca* ]] || fail "R Valkey with tls=false still gets TLS settings"
pass "R rendering: stale external settings are ignored without a host; a CA never turns TLS on by itself"

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null

# The customer's databases: a private CA, a server certificate for both, passwords.
uv run --no-project --with cryptography python - "$W" <<'PY'
import datetime, sys
from cryptography import x509
from cryptography.x509.oid import NameOID
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
d, now = sys.argv[1], datetime.datetime.now(datetime.timezone.utc)
def name(cn): return x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, cn)])
ca_key = ec.generate_private_key(ec.SECP256R1())
ca = (x509.CertificateBuilder().subject_name(name("ambit test CA")).issuer_name(name("ambit test CA")).public_key(ca_key.public_key())
      .serial_number(x509.random_serial_number()).not_valid_before(now).not_valid_after(now + datetime.timedelta(days=2))
      .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True).sign(ca_key, hashes.SHA256()))
key = ec.generate_private_key(ec.SECP256R1())
hosts = [f"{s}.ambit-ext.svc{t}" for s in ("clickhouse", "valkey") for t in ("", ".cluster.local")]
cert = (x509.CertificateBuilder().subject_name(name("ambit-ext")).issuer_name(ca.subject).public_key(key.public_key())
        .serial_number(x509.random_serial_number()).not_valid_before(now).not_valid_after(now + datetime.timedelta(days=2))
        .add_extension(x509.SubjectAlternativeName([x509.DNSName(h) for h in hosts]), critical=False).sign(ca_key, hashes.SHA256()))
open(f"{d}/ca.crt", "wb").write(ca.public_bytes(serialization.Encoding.PEM))
open(f"{d}/tls.crt", "wb").write(cert.public_bytes(serialization.Encoding.PEM))
open(f"{d}/tls.key", "wb").write(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
PY
CH_PW=$(openssl rand -hex 16); VK_PW=$(openssl rand -hex 16)
kubectl create namespace "$EXT" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$EXT" create secret generic ext-tls --from-file="$W/tls.crt" --from-file="$W/tls.key" --from-file="$W/ca.crt" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$EXT" create secret generic ext-passwords --from-literal=clickhouse="$CH_PW" --from-literal=valkey="$VK_PW" \
  --from-literal=valkey.conf="requirepass $VK_PW" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$EXT" apply -f test/external/databases.yaml >/dev/null
kubectl -n "$EXT" rollout status deploy/clickhouse deploy/valkey --timeout=300s >/dev/null
kubectl -n "$NS" create secret generic ext-clickhouse --from-literal=password="$CH_PW" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic ext-valkey --from-literal=password="$VK_PW" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic ext-ca --from-file=ca.crt="$W/ca.crt" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml \
  --set evidence.clickhouse.external.host=clickhouse.$EXT.svc --set evidence.clickhouse.external.user=ambit_ext \
  --set evidence.clickhouse.external.database=audit --set evidence.clickhouse.external.passwordSecret=ext-clickhouse \
  --set evidence.clickhouse.external.caSecret=ext-ca \
  --set limits.valkey.external.address=valkey.$EXT.svc:6380 --set limits.valkey.external.passwordSecret=ext-valkey \
  --set limits.valkey.external.caSecret=ext-ca --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console deploy/ambit-otel-collector deploy/ambit-ratelimit --timeout=300s >/dev/null
for _ in $(seq 1 60); do
  [[ -z $(kubectl -n "$NS" get pods -o name | grep -E 'ambit-clickhouse|ambit-valkey' || true) ]] && break; sleep 2
done
left=$(kubectl -n "$NS" get pods -o name | grep -E 'ambit-clickhouse|ambit-valkey' || true)
[[ -z "$left" ]] || fail "0 the chart still runs its own databases: $left"
pass "0 Ambit uses the external ClickHouse and Valkey (TLS, private CA, passwords); the chart runs neither"

# 1 · the gateway's access log reaches the external ClickHouse, and the console's audit reads it back
A=$(token alice)
tools "$A"
[[ $(call time_get_current_time '{"timezone":"UTC"}') == ok* ]] || fail "1 tool call failed"
for _ in $(seq 1 60); do
  n=$(ch "SELECT count() FROM audit.otel_logs WHERE LogAttributes['jwt.email'] = 'alice@acme.test' AND LogAttributes['gen_ai.tool.name'] = 'get_current_time'" 2>/dev/null || echo 0)
  [[ "$n" -ge 1 ]] && break; sleep 2
done
[[ "$n" -ge 1 ]] || fail "1 no access log in the external ClickHouse"
sign_in keycloak priya priya-test-pass
day=$(date -u +%Y-%m-%d); next=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)
[[ $(api GET "/api/audit?email=alice@acme.test&from=$day&to=$next") == 200 ]] || fail "1 audit search failed: $(cat "$W/api")"
[[ $(field 'len([c for c in d["calls"] if c["tool"]=="get_current_time"])') -ge 1 ]] || fail "1 the console's audit does not show the call"
pass "1 alice's tool call is in the external ClickHouse (audit.otel_logs, user ambit_ext) and in the console's audit"

# 2 · a budget is enforced with counters in the external Valkey, and the console reads them
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: $G/v1alpha1
kind: Budget
metadata: { name: ext-eng }
spec: { group: eng, userRequestsPerDay: 1 }
YAML
for _ in $(seq 1 60); do [[ $(outcome "$A") == blocked ]] && break; sleep 3; done
[[ $(outcome "$A") == blocked ]] || fail "2 over budget but not refused"
keys=$(vk --scan --pattern 'ambit-llm*' | tr '\n' ' ')
[[ "$keys" == *"alice@acme.test"* ]] || fail "2 no counter for alice in the external Valkey: '$keys'"
[[ $(api GET /api/budgets) == 200 ]] || fail "2 budgets page failed: $(cat "$W/api")"
used=$(field '[u["used"] for b in d if b["name"]=="ext-eng" for u in b["users"] if u["email"]=="alice@acme.test"]')
missing=$(field '[b.get("countsMissing", False) for b in d if b["name"]=="ext-eng"]')
[[ "$missing" == "[False]" && "$used" != "[]" ]] || fail "2 console could not read the external counters: used=$used missing=$missing"
pass "2 budget enforced on the external Valkey (TLS, password): alice refused; counter $keys; console shows used $used"

# 3 · back to the bundled databases: the collector writes to the chart's ClickHouse again
kubectl -n "$NS" delete "budgets.$G" ext-eng --ignore-not-found >/dev/null
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status statefulset/ambit-clickhouse deploy/ambit-otel-collector deploy/ambit-console --timeout=300s >/dev/null
since=$(date -u +"%Y-%m-%d %H:%M:%S")
A=$(token alice); tools "$A"; call time_get_current_time '{"timezone":"UTC"}' >/dev/null
for _ in $(seq 1 60); do
  n=$(bundled_ch "SELECT count() FROM otel.otel_logs WHERE Timestamp >= '$since' AND LogAttributes['jwt.email'] = 'alice@acme.test'" 2>/dev/null || echo 0)
  [[ "$n" -ge 1 ]] && break; sleep 2
done
[[ "$n" -ge 1 ]] || fail "3 after switching back, no access log reached the bundled ClickHouse"
pass "3 switched back: the chart's own ClickHouse and Valkey run again and receive new access logs"
done_step ext
