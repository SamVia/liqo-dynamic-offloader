# Image URL to use all building/pushing image targets
IMG ?= liqo-dynamic-offloader:latest
CONTROLLER_GEN ?= ./bin/controller-gen
GOLANGCILINT ?= ./bin/golangci-lint
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

.PHONY: lint
lint: ## Run golangci-lint against code
	@if [ ! -f $(GOLANGCILINT) ]; then \
		echo "Installing golangci-lint..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b ./bin latest; \
	fi
	$(GOLANGCILINT) run ./...

.PHONY: test
test: manifests fmt vet lint ## Run tests
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

# E2E tests (Ginkgo). The suite creates two kind clusters, peers them with Liqo,
# deploys the operator built from the working tree and deletes the clusters at
# the end. See test/e2e/e2e_suite_test.go for the environment variables, e.g.
#   make e2e E2E_KEEP_CLUSTERS=true          keep the clusters for inspection
#   make e2e E2E_SKIP_CLUSTER_SETUP=true     rerun the specs on kept clusters
.PHONY: e2e
e2e: ## Run the E2E tests on fresh kind clusters
	go test -tags e2e -v -count=1 -timeout 40m ./test/e2e/... -ginkgo.v

.PHONY: e2e-clean
e2e-clean: ## Delete the kind clusters kept by the E2E suite
	kind delete cluster --name $${E2E_LOCAL_CLUSTER:-e2e-local}
	kind delete cluster --name $${E2E_REMOTE_CLUSTER:-e2e-remote}