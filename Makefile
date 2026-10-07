# Image URL to use all building/pushing image targets
IMG ?= liqo-dynamic-offloader:latest
CONTROLLER_GEN ?= ./bin/controller-gen
CHART ?= charts/liqo-dynamic-offloader
HELM_RELEASE ?= liqo-dynamic-offloader
HELM_NAMESPACE ?= liqo-system

.PHONY: all
all: build

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./..." output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: test
test: manifests fmt vet ## Run tests
	go test -v ./...

.PHONY: build
build: fmt vet
	go build -a -o bin/manager main.go

.PHONY: run
run: manifests fmt vet
	go run ./main.go \
		--target-cluster-ids="cluster-remote" \
		--excluded-namespaces="kube-system,liqo-system,local-path-storage" \
		--trap-blacklist-labels="dynamic-offloader.liqo.io/ignore-trap=true" \
		--cleanup-blacklist-labels="dynamic-offloader.liqo.io/ignore-cleanup=true" \
		--trap-backoff="2s" \
		--cleanup-delay="10s"

.PHONY: docker-build
docker-build: test
	docker build -t ${IMG} .

.PHONY: helm-lint
helm-lint: ## Run helm lint to verify chart correctness
	helm lint charts/liqo-dynamic-offloader

.PHONY: helm-template
helm-template: ## Run helm template to render the manifests locally
	helm template $(HELM_RELEASE) $(CHART) --namespace $(HELM_NAMESPACE) --debug

.PHONY: helm-install
helm-install: helm-lint ## Install or upgrade the Helm release
	helm upgrade --install $(HELM_RELEASE) $(CHART) --namespace $(HELM_NAMESPACE) --create-namespace