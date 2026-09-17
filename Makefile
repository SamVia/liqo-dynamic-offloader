# Image URL to use all building/pushing image targets
IMG ?= liqo-dynamic-offloader:latest
CONTROLLER_GEN ?= ./bin/controller-gen

.PHONY: all
all: build

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./..." output:rbac:artifacts:config=config/rbac

.PHONY: build
build:
	go build -a -o bin/manager main.go

.PHONY: run
run: manifests
	go run ./main.go

.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .