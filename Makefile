NS ?= ambit-system
KIND_CLUSTER ?= ambit
IMAGE ?= ambit-console:dev

.PHONY: kind-up kind-down deps lint web test-go image install test
kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config kind/cluster.yaml --wait 120s
	./kind/coredns-rewrite.sh $(NS)

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

deps:
	helm dependency update charts/ambit-crds
	helm dependency update charts/ambit

lint: deps
	helm lint charts/ambit-crds
	helm lint charts/ambit -f charts/ambit/values-kind.yaml

web:
	cd web && npm ci --no-fund --no-audit && npm run build

test-go:
	go vet ./... && go test ./...

image:
	docker build -t $(IMAGE) .
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

install: deps
	helm upgrade --install ambit-crds charts/ambit-crds -n $(NS) --create-namespace --wait
	helm upgrade --install ambit charts/ambit -n $(NS) -f charts/ambit/values-kind.yaml --wait --timeout 5m

test:
	./test/step0.sh $(NS)
	./test/step1.sh $(NS)
	./test/step2.sh $(NS)
	./test/step3.sh $(NS)
