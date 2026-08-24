# idp-operator build/test entry points.
ENVTEST_K8S_VERSION ?= 1.36.x
GOBIN ?= $(shell go env GOPATH)/bin
CONTROLLER_GEN ?= $(GOBIN)/controller-gen
SETUP_ENVTEST ?= $(GOBIN)/setup-envtest

.PHONY: all build fmt vet test generate manifests tools

all: build

## Install pinned codegen + the envtest binary fetcher into GOBIN.
tools:
	GOBIN=$(GOBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
	GOBIN=$(GOBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

## Regenerate deepcopy from API markers.
generate:
	$(CONTROLLER_GEN) object paths=./api/...

## Regenerate CRD + RBAC manifests from markers.
manifests:
	$(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=idp-operator paths=./... output:rbac:artifacts:config=config/rbac

build:
	go build ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

## Unit + envtest. Downloads API-server binaries on first run; without
## KUBEBUILDER_ASSETS the envtest cases skip and only unit tests run.
test: vet
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./... -coverprofile cover.out
