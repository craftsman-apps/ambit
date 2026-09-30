NS ?= ambit-system
CONTROLLER_GEN_VERSION ?= v0.22.0
GATEWAY_API_VERSION ?= v1.6.2
AGENTGATEWAY_VERSION ?= v1.5.0
TOOLHIVE_VERSION ?= 0.51.4
ARGOCD_CHART_VERSION ?= 10.9.4
# API server and etcd binaries for the integration tests (controller-runtime envtest).
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST ?= sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2-0.20260929093216-dc84738577d1
CRDS := charts/ambit-crds
KIND_CLUSTER ?= ambit
IMAGE ?= ambit-console:dev

.PHONY: kind-up kind-down deps lint web test-go envtest-assets argocd image install install-crds test test-ui generate vendor-crds

# Browser tests (Playwright) against the running kind install; needs the LLM test catalog (make test STEPS=3).
test-ui:
	cd web && AMBIT_ADMIN_PASSWORD=$$(kubectl -n $(NS) get secret ambit-initial-admin -o jsonpath='{.data.password}' | base64 -d) npx playwright test

# Ambit's own CRDs and deepcopy code, from api/v1alpha1.
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object paths=./api/... crd:crdVersions=v1 output:crd:dir=$(CRDS)/templates/ambit
	uv run --no-project --with pyyaml python hack/keep_crds.py $(CRDS)/templates/ambit/*.yaml
	cp $(CRDS)/templates/ambit/*.yaml internal/validate/crds/  # embedded by `ambit validate`

# Third-party CRDs, vendored at pinned versions and applied outside Helm (see deploy/crds/README.md).
vendor-crds:
	rm -f deploy/crds/*.yaml
	curl -sSfL -o deploy/crds/gateway-api-standard-$(GATEWAY_API_VERSION).yaml https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml
	helm template agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds --version $(AGENTGATEWAY_VERSION) > deploy/crds/agentgateway-crds-$(AGENTGATEWAY_VERSION).yaml
	helm template toolhive-operator-crds oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds --version $(TOOLHIVE_VERSION) > deploy/crds/toolhive-crds-$(TOOLHIVE_VERSION).yaml

install-crds:
	kubectl apply --server-side --force-conflicts -f deploy/crds/ >/dev/null
kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config kind/cluster.yaml --wait 120s
	./kind/coredns-rewrite.sh $(NS)

# Argo CD v3.5.3 with Ambit's health check, for GitOps mode (step 4c).
argocd:
	helm upgrade --install argocd argo-cd --repo https://argoproj.github.io/argo-helm --version $(ARGOCD_CHART_VERSION) \
	  -n argocd --create-namespace -f deploy/argocd/values-kind.yaml \
	  $(foreach k,Model Server AccessRule,--set-file 'configs.cm.resource\.customizations\.health\.ambitproject\.dev_$(k)=deploy/argocd/health.lua') \
	  --wait --timeout 5m

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

deps:
	helm dependency update charts/ambit

lint: deps
	helm lint charts/ambit-crds
	helm lint charts/ambit -f charts/ambit/values-kind.yaml

web:
	cd web && npm ci --no-fund --no-audit && npm run build

# The console embeds web/dist, so Go builds need the front end built first.
# Unit and integration tests; envtest-assets prints where the API server binaries are.
test-go: web
	go vet ./... && KUBEBUILDER_ASSETS="$$($(MAKE) -s envtest-assets)" go test -count=1 ./...

envtest-assets:
	@go run $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

image:
	docker build -t $(IMAGE) .
	docker build -t ambit-whoami:dev -f test/whoami/Dockerfile .  # test-only MCP server (step 5)
	kind load docker-image $(IMAGE) ambit-whoami:dev --name $(KIND_CLUSTER)

install: deps install-crds
	helm upgrade --install ambit-crds charts/ambit-crds -n $(NS) --create-namespace --wait
	helm upgrade --install ambit charts/ambit -n $(NS) -f charts/ambit/values-kind.yaml --wait --timeout 5m

STEPS ?= 0 1 2 3 3b 3c 4a 4b 4c 4d 5 6 7
# make test            -> every step; make test STEPS=3b -> one step (each sets up what it needs)
test:
	@for s in $(STEPS); do ./test/step$$s.sh $(NS) || exit 1; done
