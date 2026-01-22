# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: build

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path); \
	go test ./... -coverprofile cover.out

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager cmd/manager/main.go

.PHONY: build-cli
build-cli: fmt vet ## Build kube-etl binary.
	go build -o bin/kube-etl main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run ./cmd/manager/main.go

.PHONY: test manifests generate
# Generate manifests (CRDs, RBAC, etc.)
manifests:
	controller-gen crd rbac:roleName=manager-role paths="./..." output:crd:artifacts:config=config/crd/

# Generate code (deepcopy, etc.)
generate:
	controller-gen object paths="./..."

##@ Integration Testing

.PHONY: test-integration
test-integration: manifests generate
	go test -v ./test/integration/...

##@ Optional Integration Tests via Kind (Requires Kubeconfig)
#.PHONY: kind-up-all
#kind-up-all:
#	$(MAKE) kind-up CLUSTER_NAME=syncer-source KUBECONFIG_PATH=$(shell pwd)/test/integration/source.kubeconfig
#	$(MAKE) kind-up CLUSTER_NAME=syncer-dest KUBECONFIG_PATH=$(shell pwd)/test/integration/dest.kubeconfig
#	# Install CRDs to source cluster
#	kubectl --kubeconfig=$(shell pwd)/test/integration/source.kubeconfig apply -f config/crd/syncer.core.cnrm.cloud.google.com_syncers.yaml
#
#.PHONY: kind-down-all
#kind-down-all:
#	$(MAKE) kind-down CLUSTER_NAME=$(KIND_CLUSTER_SOURCE)
#	$(MAKE) kind-down CLUSTER_NAME=$(KIND_CLUSTER_DEST)
#
#.PHONY: kind-up
#kind-up:
#	@if ! kind get clusters | grep -q "^$(CLUSTER_NAME)$$"; then \
#		kind create cluster --name $(CLUSTER_NAME); \
#	else \
#		echo "Cluster $(CLUSTER_NAME) already exists"; \
#	fi
#	kind export kubeconfig --name $(CLUSTER_NAME) --kubeconfig $(KUBECONFIG_PATH)
#
#.PHONY: kind-down
#kind-down:
#	kind delete cluster --name $(CLUSTER_NAME)
