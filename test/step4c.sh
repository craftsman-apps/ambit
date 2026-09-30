#!/usr/bin/env bash
# Step 4c acceptance: GitOps mode. The console opens pull requests in the configuration repository and
# Argo CD applies what is merged. Journey A1 under Argo CD, and the GitOps rules (section 08): the
# Application stays Synced and Healthy with prune and self-heal on while the Ambit controller works.
# Needs a GitHub App installed on craftsman-apps/ambit-config-test: its ID in AMBIT_GITHUB_APP_ID (or
# ~/.config/ambit/github-app-id) and its key in ~/.config/ambit/github-app.pem. Skips without them.
set -euo pipefail
NS=${1:-ambit-system}
cd "$(dirname "$0")/.."
source test/lib.sh
G=ambitproject.dev
REPO=craftsman-apps/ambit-config-test
APP_ID=${AMBIT_GITHUB_APP_ID:-$(cat "$HOME/.config/ambit/github-app-id" 2>/dev/null || true)}
KEY=${AMBIT_GITHUB_APP_KEY:-$HOME/.config/ambit/github-app.pem}
SOAK_SECONDS=${SOAK_SECONDS:-600}
IMAGE=ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949
if [[ -z "$APP_ID" || ! -s "$KEY" ]]; then
  FINISHED=1; echo "SKIP: step 4c needs the GitHub App (AMBIT_GITHUB_APP_ID and $KEY)"; exit 0
fi
app() { uv run --no-project --with 'pyjwt[crypto]' python test/gitops/github_app.py "$APP_ID" "$KEY" "$REPO" "$@"; }
gh_api() { # gh_api METHOD PATH [JSON]: GitHub REST as the App; body in $W/gh
  local -a a=(-s -o "$W/gh" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json')
  [[ $# -ge 3 ]] && a+=(-d "$3")
  command curl "${a[@]}" "https://api.github.com$2"
}
field() { uv run --no-project python -c "import json;d=json.load(open('$1'));print($2)"; }
app_status() { kubectl -n argocd get application ambit-config -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null; }
refresh() { kubectl -n argocd annotate application ambit-config argocd.argoproj.io/refresh=normal --overwrite >/dev/null; }
wait_for() { # wait_for DESCRIPTION SECONDS CONDITION
  for _ in $(seq 1 "$2"); do eval "$3" && return 0; sleep 1; done
  fail "$1"
}
merge() { # merge PR_URL: merges as the App (the reviewer's click in real use), then asks Argo CD to look now
  local n=${1##*/}
  [[ $(gh_api PUT "/repos/$REPO/pulls/$n/merge" '{"merge_method":"squash"}') == 200 ]] || fail "merge #$n: $(cat "$W/gh")"
  refresh
}
cleanup() {
  # Back to Direct mode for any later step: stop Argo CD managing the objects (no cascade), restore values.
  kubectl -n argocd delete application ambit-config --ignore-not-found --wait=false >/dev/null 2>&1 || true
  helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml --wait --timeout 5m >/dev/null || true
  kubectl -n "$NS" delete "servers.$G" time-b --ignore-not-found >/dev/null
  kubectl -n "$NS" delete "serverrequests.$G" --all >/dev/null
  for k in models servers accessrules; do
    kubectl -n "$NS" annotate "$k.$G" --all argocd.argoproj.io/tracking-id- >/dev/null 2>&1 || true
  done
  kubectl -n "$NS" apply -f test/llm/objects.yaml -f test/mcp/objects.yaml >/dev/null
}
trap 'rc=$?; cleanup; on_exit $rc' EXIT

ensure_llm_setup "$NS"
kubectl -n "$NS" apply -f test/mcp/objects.yaml >/dev/null
make -s argocd >/dev/null
# GitHub occasionally fails a token request; a bad token then surfaces as "Repository not found" on clone.
for _ in 1 2 3; do TOKEN=$(app 2>/dev/null || true); [[ "$TOKEN" == ghs_* ]] && break; sleep 3; done
[[ "$TOKEN" == ghs_* ]] || fail "0 no GitHub App installation token (check the App ID, key and installation)"
INSTALLATION=$(app --installation-id)

# Credentials: the console's key Secret and Argo CD's repository credentials, both from the same App.
kubectl -n "$NS" create secret generic ambit-github-app --from-file=private-key.pem="$KEY" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n argocd create secret generic ambit-config-repo --dry-run=client -o yaml \
  --from-literal=type=git --from-literal=url="https://github.com/$REPO" \
  --from-literal=githubAppID="$APP_ID" --from-literal=githubAppInstallationID="$INSTALLATION" \
  --from-file=githubAppPrivateKey="$KEY" | kubectl label --local -f - argocd.argoproj.io/secret-type=repository -o yaml | kubectl apply -f - >/dev/null

# Seed the repository with the test catalog (one file per object), closing leftover console pull requests.
[[ $(gh_api GET "/repos/$REPO/pulls?state=open&per_page=100") == 200 ]] || fail "0 list pull requests: $(cat "$W/gh")"
for n in $(field "$W/gh" '" ".join(str(p["number"]) for p in d if p["head"]["ref"].startswith("ambit/"))'); do
  gh_api PATCH "/repos/$REPO/pulls/$n" '{"state":"closed"}' >/dev/null
done
rm -rf "$W/repo"
for _ in 1 2 3; do git clone -q "https://x-access-token:$TOKEN@github.com/$REPO.git" "$W/repo" 2>/dev/null && break; rm -rf "$W/repo"; sleep 3; done
[[ -d "$W/repo/.git" ]] || fail "0 could not clone $REPO"
( cd "$W/repo" && git checkout -q -B main && rm -rf ambit )
uv run --no-project --with pyyaml python test/gitops/seed.py "$W/repo"
( cd "$W/repo" && git add -A && { git diff --cached --quiet || git -c user.name=ambit-test -c user.email=ambit-test@users.noreply.github.com commit -qm "Seed the step 4c test catalog"; } && git push -q origin main )
pass "0 config repository seeded: $(cd "$W/repo" && find ambit -name '*.yaml' | wc -l | tr -d ' ') objects in ambit/"

# 1 · GitOps mode on; Argo CD syncs the directory and reports every Ambit object Healthy
helm upgrade ambit charts/ambit -n "$NS" -f charts/ambit/values-kind.yaml -f test/keycloak/values-sso.yaml -f test/gitops/values-gitops.yaml \
  --set gitops.github.appID="$APP_ID" --wait --timeout 5m >/dev/null
kubectl -n "$NS" rollout status deploy/ambit-console --timeout=180s >/dev/null
kubectl apply -f deploy/argocd/application.yaml >/dev/null
wait_for "1 Application not Synced/Healthy: $(app_status)" 300 '[[ $(app_status) == Synced/Healthy ]]'
[[ -n $(kubectl -n "$NS" get "accessrules.$G" eng-time -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/tracking-id}') ]] || fail "1 eng-time not tracked by Argo CD"
pass "1 Argo CD: ambit-config Synced/Healthy; Ambit objects tracked (health check from deploy/argocd/health.lua)"

# 2 · the console cannot write rules or servers to the cluster in GitOps mode
for verb in create update delete; do
  [[ $(kubectl auth can-i "$verb" "accessrules.$G" -n "$NS" --as="system:serviceaccount:$NS:ambit-console") == no ]] || fail "2 console may $verb accessrules"
done
[[ $(kubectl auth can-i create "servers.$G" -n "$NS" --as="system:serviceaccount:$NS:ambit-console") == no ]] || fail "2 console may create servers"
pass "2 console service account cannot create, update or delete rules or create servers"

# 3 · A1 through Git: priya edits a rule -> pull request; nothing changes until it is merged
sign_in keycloak priya priya-test-pass
code=$(api PUT /api/access-rules/eng-time '{"groups":["eng"],"allSignedIn":false,"models":[],"servers":[{"name":"time","tools":["convert_time","get_current_time"]}]}')
[[ "$code" == 202 ]] || fail "3 rule change: $code $(cat "$W/api")"
PR=$(field "$W/api" 'd["pullRequest"]["url"]')
[[ $(kubectl -n "$NS" get "accessrules.$G" eng-time -o jsonpath='{.spec.servers[0].tools}') == '["get_current_time"]' ]] || fail "3 cluster changed before merge"
[[ $(api GET /api/changes) == 200 && "$(cat "$W/api")" == *"$PR"* ]] || fail "3 pending changes do not list $PR: $(cat "$W/api")"
merge "$PR"
wait_for "3 rule not synced after merge" 120 '[[ $(kubectl -n "$NS" get "accessrules.$G" eng-time -o jsonpath="{.spec.servers[0].tools}") == *convert_time* ]]'
wait_for "3 alice does not see convert_time: $TOOLS" 60 'tools "$(token alice)"; [[ $TOOLS == *time_convert_time* ]]'
pass "3 priya's change opened $PR; cluster unchanged until merge; after merge alice sees: $TOOLS"

# 4 · server approval through Git: sam approves -> pull request with the digest; the server runs after merge
sign_in keycloak alice alice-test-pass
[[ $(api POST /api/server-requests "{\"serverName\":\"time-b\",\"image\":\"$IMAGE\",\"reason\":\"step 4c\"}") == 201 ]] || fail "4 request: $(cat "$W/api")"
REQ=$(field "$W/api" 'd["name"]')
sign_in keycloak sam sam-test-pass
[[ $(api POST "/api/server-requests/$REQ/approve" '{"comment":"digest checked"}') == 202 ]] || fail "4 approve: $(cat "$W/api")"
SPR=$(field "$W/api" 'd["pullRequest"]["url"]')
kubectl -n "$NS" get "servers.$G" time-b >/dev/null 2>&1 && fail "4 server exists before merge"
[[ $(kubectl -n "$NS" get "serverrequests.$G" "$REQ" -o jsonpath='{.status.decision} {.status.pullRequest}') == "Approved $SPR" ]] || fail "4 request status"
merge "$SPR"
wait_for "4 time-b not Ready after merge" 300 'kubectl -n "$NS" wait "servers.$G" time-b --for=condition=Ready --timeout=1s >/dev/null 2>&1'
[[ $(kubectl -n "$NS" get "servers.$G" time-b -o jsonpath='{.metadata.annotations.ambitproject\.dev/approved-by}') == sam@acme.test ]] || fail "4 approval record lost"
pass "4 sam's approval opened $SPR; time-b ran only after the merge, with approved-by sam"

# 5 · an invalid object fails `ambit validate`, the pull request check (the repository itself passes)
( cd "$W/repo" && git pull -q origin main )
go run ./cmd/ambit validate "$W/repo/ambit" >"$W/v" || fail "5 repository fails validation: $(cat "$W/v")"
printf 'apiVersion: %s/v1alpha1\nkind: Server\nmetadata: { name: tagged }\nspec: { image: "ghcr.io/x/y:latest" }\n' "$G" >"$W/repo/ambit/servers/tagged.yaml"
if go run ./cmd/ambit validate "$W/repo/ambit" >"$W/v" 2>&1; then fail "5 a tag-only image passed validation"; fi
rm "$W/repo/ambit/servers/tagged.yaml"
pass "5 ambit validate: repository passes; a tag-only image fails ($(grep -o 'spec.image in body should match.*' "$W/v" | head -c 60))"

# 6 · soak: Synced and Healthy the whole time, no sync loop, no generated object pruned
gen() { kubectl -n "$NS" get agentgatewaymodels,mcpservers,agentgatewaybackends -o name | wc -l | tr -d ' '; }
hist() { kubectl -n argocd get application ambit-config -o jsonpath='{.status.history[*].id}' | wc -w | tr -d ' '; }
g0=$(gen); h0=$(hist); end=$((SECONDS + SOAK_SECONDS))
while (( SECONDS < end )); do
  s=$(app_status); [[ "$s" == Synced/Healthy ]] || fail "6 Application became $s after $((SOAK_SECONDS - (end - SECONDS))) s"
  [[ $(gen) == "$g0" ]] || fail "6 generated objects changed: $g0 -> $(gen)"
  sleep 15
done
[[ $(hist) == "$h0" ]] || fail "6 Argo CD synced $(( $(hist) - h0 )) more times with no new commit (a sync loop)"
pass "6 soak ${SOAK_SECONDS} s: Synced/Healthy throughout, prune and self-heal on, $g0 generated objects kept, no extra syncs"
done_step 4c
