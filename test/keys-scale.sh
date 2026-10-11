#!/usr/bin/env bash
# 1,000 personal API keys on a throwaway kind cluster (own kubeconfig, host port 18080, image ambit-console:scale built
# from this tree). Prints: agentgateway memory before/after, time until 1,000 keys are accepted, revocation latency,
# repair time of one person's 20 keys, and time from proxy-pod deletion to the first 200.
#   test/keys-scale.sh > keys-scale.txt        # QS_KEEP=1 keeps the cluster
# Never touches the shared ambit cluster. The cluster is deleted at the end, and on failure.
# To write 1,000 keys faster than the console's limits (20 per person, 10 per hour) it removes the three
# ambit-keys-writer bindings on this cluster, writes the Secrets itself, and restores the bindings before it revokes.
set -euo pipefail
cd "$(dirname "$0")/.."
NS=ambit-system CL=${QS_CLUSTER:-ambit-keyscale} PORT=${QS_PORT:-18080} IMG=${QS_IMAGE:-ambit-console:scale}
export BASE=http://console.localtest.me:$PORT
source test/lib.sh
LLM=http://llm.localtest.me:$PORT
export KUBECONFIG=$W/kubeconfig   # never the shared cluster's context
ks_exit() {
  local rc=$?
  [[ -n "${QS_KEEP:-}" ]] || kind delete cluster --name "$CL" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
  on_exit "$rc"
}
trap ks_exit EXIT
now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }
ms() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.0f", (b-a)*1000}'; }
say() { echo "$*"; }

# --- cluster, install (as test/quickstart.sh) ---
[[ -n "${QS_IMAGE:-}" ]] || docker build -q -t "$IMG" . >/dev/null
uv run --no-project --with pyyaml python - "$CL" "$PORT" > "$W/cluster.yaml" <<'PY'
import sys, yaml
c = yaml.safe_load(open("kind/cluster.yaml"))
c["name"] = sys.argv[1]
m = c["nodes"][0]["extraPortMappings"]
c["nodes"][0]["extraPortMappings"] = [dict(p, hostPort=int(sys.argv[2])) for p in m if p["containerPort"] == 30080]
print(yaml.safe_dump(c))
PY
kind create cluster --config "$W/cluster.yaml" --kubeconfig "$KUBECONFIG" --wait 120s >/dev/null
kind load docker-image "$IMG" --name "$CL" >/dev/null
kubectl apply --server-side -f deploy/crds/ >/dev/null
command helm install ambit-crds charts/ambit-crds -n "$NS" --create-namespace --wait >/dev/null
command helm install ambit charts/ambit -n "$NS" -f deploy/quickstart.yaml --set console.url="$BASE" \
  --set console.image.repository="${IMG%:*}" --set console.image.tag="${IMG##*:}" --set console.image.pullPolicy=Never \
  --wait --timeout 10m >/dev/null
# metrics-server as the Makefile target (kind kubelets use self-signed certificates)
command helm upgrade --install metrics-server metrics-server --repo https://kubernetes-sigs.github.io/metrics-server/ --version 3.14.0 \
  -n kube-system --set 'args={--kubelet-insecure-tls}' --wait --timeout 5m >/dev/null

say "== versions"
say "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)  git: $(git rev-parse --short HEAD)  kubernetes: $(kubectl version -o json | jq -r .serverVersion.gitVersion)  kind: $(kind version | cut -d' ' -f2)"
say "agentgateway chart: $(grep -A2 'name: agentgateway' charts/ambit/Chart.yaml | grep version | tr -d ' ')  proxy image: $(kubectl -n "$NS" get pod -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].spec.containers[0].image}')"
say "console: $(kubectl -n "$NS" get deploy ambit-console -o jsonpath='{.spec.template.spec.containers[0].image}')"

# --- a model and an access rule for everyone ---
kubectl -n "$NS" create secret generic ambit-llm-mock --from-literal=Authorization=not-used >/dev/null
kubectl -n "$NS" apply -f test/llm/mock.yaml >/dev/null
kubectl -n "$NS" rollout status deploy/mock-llm --timeout=180s >/dev/null
kubectl -n "$NS" wait models.ambitproject.dev/mock-echo --for=condition=Ready --timeout=120s >/dev/null
kubectl -n "$NS" apply -f - >/dev/null <<'YAML'
apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: everyone-mock }
spec: { allSignedIn: true, models: [mock-echo] }
YAML
PW=$(kubectl -n "$NS" get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d)
code() { command curl -s -m 5 -o /dev/null -w '%{http_code}' "$LLM/v1/chat/completions" -H 'Content-Type: application/json' -H "x-api-key: $1" \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"hello"}]}' || true; }

# can_write_key: a key-labelled Secret created by the cluster admin is accepted (0) or refused by the binding (1)
can_write_key() {
  kubectl -n "$NS" apply -f - >/dev/null 2>&1 <<'YAML' || return 1
apiVersion: v1
kind: Secret
metadata: { name: scale-probe, labels: { ambitproject.dev/key: "true" } }
YAML
  kubectl -n "$NS" delete secret scale-probe --ignore-not-found >/dev/null 2>&1 || true
}

# --- memory ---
proxy_pod() { kubectl -n "$NS" get pod -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].metadata.name}'; }
cp_pod() { kubectl -n "$NS" get pod -l app.kubernetes.io/name=agentgateway -o jsonpath='{.items[0].metadata.name}'; }
mem() { # MiB of one pod, the highest of 6 samples over ~75 s (metrics-server refreshes every 15 s)
  local best=0 v
  for _ in 1 2 3 4 5 6; do
    v=$(kubectl -n "$NS" top pod "$1" --no-headers 2>/dev/null | awk '{gsub("Mi","",$3); print $3}')
    [[ "$v" =~ ^[0-9]+$ && $v -gt $best ]] && best=$v
    sleep 15
  done
  echo "$best"
}
limit() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.spec.containers[0].resources.limits.memory}'; }
oom_check() { # stop when a pod was OOM-killed or restarted
  local p="$1" r
  r=$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].lastState.terminated.reason}')
  [[ "$r" == "0 " ]] || fail "STOP: $p restarted or was killed ($r)"
}
CPP=$(cp_pod); PXP=$(proxy_pod)
[[ -n "$CPP" && -n "$PXP" ]] || fail "no agentgateway pods (control plane '$CPP', proxy '$PXP')"
say "== (a) memory, MiB (limits: control plane $(limit "$CPP"), proxy $(limit "$PXP")); highest of 6 samples"
CP0=$(mem "$CPP"); PX0=$(mem "$PXP")
say "before: control plane $CP0 MiB, proxy $PX0 MiB (0 keys)"

# --- sign in as the bootstrap admin (the console's own owner record exists after this) ---
sign_in local admin@ambit.local "$PW"
[[ $(me_status) == 200 ]] || fail "sign-in: $(cat "$W/me")"

# --- 50 owners x 20 keys, written in Task 5's format; plaintexts known ---
uv run --no-project python - "$NS" "$W" <<'PY'
import sys, json, hashlib, secrets, string, datetime as dt
ns, w = sys.argv[1:3]
B62 = string.digits + string.ascii_uppercase + string.ascii_lowercase
B32 = "abcdefghijklmnopqrstuvwxyz234567"
now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
ts = lambda t: t.strftime("%Y-%m-%dT%H:%M:%SZ")
oh = lambda e: hashlib.sha256(e.encode()).hexdigest()[:32]
items, tsv = [], []
for p in range(50):
    email = f"scale-{p:02d}@example.test"
    items.append({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
        "metadata": {"name": "ambit-key-owner-" + oh(email), "namespace": ns, "labels": {"ambitproject.dev/key-owner-record": "true"}},
        "stringData": {"owner.json": json.dumps({"email": email, "groups": ["scale"], "signedIn": ts(now), "offboarded": False})}})
    for i in range(20):
        plain = "ambit_" + "".join(secrets.choice(B62) for _ in range(43))
        kid = "".join(secrets.choice(B32) for _ in range(12))
        # person 0's keys carry an older sign-in than the owner record, so the start-up repair has 20 keys to refresh
        signed = now - dt.timedelta(hours=1) if p == 0 else now
        until = signed + dt.timedelta(days=30)
        entry = {"keyHash": "sha256:" + hashlib.sha256(plain.encode()).hexdigest(),
                 "metadata": {"keyId": kid, "email": email, "groups": ["scale"], "until": ts(until)}}
        items.append({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
            "metadata": {"name": "ambit-key-" + kid, "namespace": ns,
                "labels": {"ambitproject.dev/key": "true", "ambitproject.dev/key-llm": "true", "ambitproject.dev/key-owner": oh(email)},
                "annotations": {"ambitproject.dev/key-name": f"k{i}", "ambitproject.dev/key-prefix": plain[:10], "ambitproject.dev/key-owner-email": email,
                    "ambitproject.dev/key-created": ts(now), "ambitproject.dev/key-expires": ts(now + dt.timedelta(days=90)),
                    "ambitproject.dev/key-signed-in": ts(signed), "ambitproject.dev/key-ceiling": ts(now + dt.timedelta(days=90)),
                    "ambitproject.dev/key-scope": "llm"}},
            "stringData": {"key": json.dumps(entry, separators=(",", ":"))}})
        tsv.append(f"{p}\t{i}\t{kid}\t{plain}")
json.dump({"apiVersion": "v1", "kind": "List", "items": items}, open(f"{w}/keys.json", "w"))
open(f"{w}/keys.tsv", "w").write("\n".join(tsv) + "\n")
PY
# the three bindings off (throwaway cluster), saved to restore later
kubectl get validatingadmissionpolicybinding -o json | jq '[.items[] | select(.metadata.name | startswith("ambit-keys-writer-")) | del(.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.generation,.metadata.managedFields)]' > "$W/bindings.json"
[[ $(jq length "$W/bindings.json") == 3 ]] || fail "expected the 3 ambit-keys-writer bindings: $(jq -r '.[].metadata.name' "$W/bindings.json")"
kubectl delete validatingadmissionpolicybinding ambit-keys-writer-0 ambit-keys-writer-1 ambit-keys-writer-2 >/dev/null
# the API server needs a moment to stop enforcing
for _ in $(seq 1 60); do can_write_key && break; sleep 1; done
can_write_key || fail "key-labelled Secrets are still refused after the bindings were removed"
# keys the first-200 sample uses: 20 random lines
sort -R "$W/keys.tsv" > "$W/shuffled.tsv"; SAMPLE=$(head -20 "$W/shuffled.tsv")
# owner records first (not timed), then the key Secrets
jq '.items |= map(select(.metadata.name | startswith("ambit-key-owner-")))' "$W/keys.json" | kubectl apply -f - >/dev/null
jq '.items |= map(select(.metadata.name | startswith("ambit-key-owner-") | not))' "$W/keys.json" > "$W/keys-only.json"

# --- (b) time to accept ---
say "== (b) first key Secret -> accepted"
T0=$(now)
kubectl apply -f "$W/keys-only.json" >/dev/null &
APPLY=$!
declare -a OK=()  # 1 once a sampled key has answered 200
while :; do
  n=0; i=0
  while IFS=$'\t' read -r _ _ _ plain; do
    [[ "${OK[$i]:-}" == 1 ]] || { [[ $(code "$plain") == 200 ]] && OK[$i]=1; }
    [[ "${OK[$i]:-}" == 1 ]] && n=$((n+1)); i=$((i+1))
  done <<<"$SAMPLE"
  [[ $n -eq 20 ]] && break
  [[ $(echo "$(now) - $T0 > 300" | bc) == 1 ]] && fail "(b) sampled keys not accepted after 300 s ($n of 20)"
done
T20=$(now)
wait "$APPLY"; TAPPLY=$(now)
# sweep: all 1,000 keys, 16 requests at a time, until none is refused
sweep() { cut -f4 "$W/keys.tsv" | xargs -n1 -P16 sh -c "curl -s -m 10 -o /dev/null -w '%{http_code}\\n' '$LLM/v1/chat/completions' -H 'Content-Type: application/json' -H \"x-api-key: \$0\" -d '{\"model\":\"mock/echo\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'" | awk '$1!=200{b++} END{print (NR==1000) ? b+0 : 1000}'; }
while :; do
  bad=$(sweep)
  [[ "$bad" == 0 ]] && break
  [[ $(echo "$(now) - $T0 > 600" | bc) == 1 ]] && fail "(b) $bad of 1000 keys still refused after 600 s"
done
TALL=$(now)
say "20 sampled keys all 200 after: $(ms "$T0" "$T20") ms"
say "kubectl apply of 1,000 Secrets finished after: $(ms "$T0" "$TAPPLY") ms"
say "all 1,000 keys answered 200 after: $(ms "$T0" "$TALL") ms (includes one sweep of 1,000 requests)"

# --- (a) after ---
oom_check "$CPP"; oom_check "$PXP"
CP1=$(mem "$CPP"); PX1=$(mem "$PXP")
oom_check "$CPP"; oom_check "$PXP"
say "== (a) memory after 1,000 keys"
say "after: control plane $CP1 MiB (limit $(limit "$CPP")), proxy $PX1 MiB (limit $(limit "$PXP"))"
say "growth: control plane $((CP1 - CP0)) MiB, proxy $((PX1 - PX0)) MiB"
[[ ${PX1} -lt 512 && ${CP1} -lt 256 ]] || fail "STOP: memory at or over a limit"

# --- restore the bindings; wait until the writer policy is enforced again ---
kubectl create -f <(jq '{apiVersion:"v1",kind:"List",items:.}' "$W/bindings.json") >/dev/null
for _ in $(seq 1 60); do can_write_key || break; sleep 1; done
! can_write_key || fail "key-labelled Secrets are still accepted after the bindings were restored"

# --- (c) revocation through the admin API ---
say "== (c) DELETE /api/admin/keys/{id} -> first non-200 on that key (20 runs, one key each of 20 people)"
LAT=(); CODES=""
for p in $(seq 1 20); do
  IFS=$'\t' read -r _ _ kid plain < <(awk -F'\t' -v p="$p" '$1==p && $2==0' "$W/keys.tsv")
  [[ $(code "$plain") == 200 ]] || fail "(c) key $kid not accepted before revocation"
  t0=$(now)
  dc=$(api DELETE "/api/admin/keys/$kid")
  [[ "$dc" == 204 || "$dc" == 200 ]] || fail "(c) DELETE $kid: $dc $(cat "$W/api")"
  while :; do
    c=$(code "$plain"); [[ "$c" != 200 ]] && break
    [[ $(echo "$(now) - $t0 > 30" | bc) == 1 ]] && fail "(c) key $kid still accepted 30 s after DELETE"
  done
  LAT+=("$(ms "$t0" "$(now)")"); CODES="$CODES $c"
done
SORTED=$(printf '%s\n' "${LAT[@]}" | sort -n)
say "latencies ms: ${LAT[*]}"
say "first refusal codes:$CODES"
say "median: $(echo "$SORTED" | sed -n '10,11p' | awk '{s+=$1} END{print s/2}') ms   max: $(echo "$SORTED" | tail -1) ms   min: $(echo "$SORTED" | head -1) ms"

# --- (d) one person's 20 keys refreshed: console restart, pod start -> last repair event ---
say "== (d) console restart: pod start -> last 'repair' event (person 0: 20 keys with an older sign-in than the owner record; 980 other keys unchanged)"
kubectl -n "$NS" rollout restart deploy/ambit-console >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null
CPOD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=ambit-console -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[[ -n "$CPOD" ]] || CPOD=$(kubectl -n "$NS" get pod -o name | grep 'pod/ambit-console-' | tail -1 | cut -d/ -f2)
PSTART=$(kubectl -n "$NS" get pod "$CPOD" -o jsonpath='{.status.startTime}')
CSTART=$(kubectl -n "$NS" get pod "$CPOD" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}')
sign_in local admin@ambit.local "$PW"
for _ in $(seq 1 60); do
  [[ $(api GET /api/admin/key-events) == 200 ]] && [[ $(jq '[.events[] | select(.action=="repair")] | length' "$W/api") -ge 20 ]] && break
  sleep 1
done
N=$(jq '[.events[] | select(.action=="repair")] | length' "$W/api")
[[ "$N" -ge 20 ]] || fail "(d) only $N repair events"
LAST=$(jq -r '[.events[] | select(.action=="repair") | .at] | max' "$W/api")
FIRST=$(jq -r '[.events[] | select(.action=="repair") | .at] | min' "$W/api")
ep() { uv run --no-project python -c 'import sys,datetime as d;print(d.datetime.fromisoformat(sys.argv[1].replace("Z","+00:00")).timestamp())' "$1"; }
say "repair events: $N; pod startTime $PSTART, container started $CSTART, first repair $FIRST, last repair $LAST"
say "pod start -> last repair event: $(awk -v a="$(ep "$PSTART")" -v b="$(ep "$LAST")" 'BEGIN{printf "%.1f", b-a}') s (pod start has 1 s resolution)"
say "container start -> last repair event: $(awk -v a="$(ep "$CSTART")" -v b="$(ep "$LAST")" 'BEGIN{printf "%.1f", b-a}') s"
say "first -> last repair event: $(awk -v a="$(ep "$FIRST")" -v b="$(ep "$LAST")" 'BEGIN{printf "%.2f", b-a}') s"

# --- (e) proxy pod deleted -> first 200 with a key ---
say "== (e) delete the agentgateway proxy pod -> first 200 with a key"
KEY=$(awk -F'\t' '$1==30 && $2==0 {print $4}' "$W/keys.tsv")
[[ $(code "$KEY") == 200 ]] || fail "(e) key not accepted before the deletion"
PXP=$(proxy_pod)
t0=$(now)
kubectl -n "$NS" delete pod "$PXP" --grace-period=0 --force --wait=false >/dev/null 2>&1   # as a crash: the old pod stops serving at once
TDOWN=
while :; do
  c=$(code "$KEY")
  if [[ -z "$TDOWN" ]]; then [[ "$c" != 200 ]] && TDOWN=$(now); else [[ "$c" == 200 ]] && break; fi
  [[ $(echo "$(now) - $t0 > 300" | bc) == 1 ]] && fail "(e) no 200 after the outage within 300 s (outage seen: ${TDOWN:-no})"
done
say "proxy pod deleted -> first refused request: $(ms "$t0" "$TDOWN") ms "
say "proxy pod deleted -> first 200 with a key (new pod): $(ms "$t0" "$(now)") ms"
sleep 5; NPX=$(proxy_pod)
say "new proxy pod $NPX memory after start: $(mem "$NPX") MiB (limit $(limit "$NPX"))"
oom_check "$NPX"; oom_check "$(cp_pod)"
done_step keys-scale
