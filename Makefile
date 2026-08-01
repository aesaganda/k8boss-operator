# K8Boss operator — build, codegen and install.
#
# `make install` = CRDs only. `make deploy` = CRDs + RBAC + ServiceAccount +
# Deployment. Both go through config/, which is a self-contained install path
# and deliberately separate from deploy/helm's control-plane chart.
#
# Tooling is expected on PATH or under $(GOBIN); nothing here downloads
# binaries behind your back. Install controller-gen with:
#   go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

SHELL := /usr/bin/env bash
GOBIN ?= $(shell go env GOPATH)/bin
CONTROLLER_GEN ?= $(GOBIN)/controller-gen
KUBECTL ?= kubectl

IMG ?= ghcr.io/aesaganda/k8boss/k8boss-operator:latest

.PHONY: all
all: build

##@ Codegen

.PHONY: manifests
manifests: ## Regenerate CRDs and the RBAC role from +kubebuilder markers.
	$(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=k8boss-operator paths=./... output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: ## Regenerate zz_generated.deepcopy.go.
	# No headerFile: the checked-in generated file carries no license header,
	# and adding one here would show up as a spurious diff on every run.
	$(CONTROLLER_GEN) object paths=./api/...

##@ Build and test

.PHONY: build
build:
	go build ./...

.PHONY: vet
vet:
	go vet ./...

# The controller suite needs a real apiserver+etcd (envtest) and deliberately
# hard-fails rather than skipping when they are missing — a suite that degrades
# to "no control plane, everything passes" is the defect class it exists to
# hunt. So provision the binaries here rather than making every caller do it.
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: envtest-assets
envtest-assets:
	@go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest \
		use $(ENVTEST_K8S_VERSION) -p path

.PHONY: test
test:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest \
		use $(ENVTEST_K8S_VERSION) -p path)" go test ./...

.PHONY: check
check: build vet test

# CI (.github/workflows/build-images.yml) builds backend/frontend/agent only —
# it does not know about this image yet, so `make deploy` pulls a tag nobody
# publishes until that matrix gains a fourth entry. Build and push by hand
# until then.
.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

##@ Deploy

.PHONY: install
install: ## Install the three CRDs.
	$(KUBECTL) apply -k config/crd

# Deleting a CRD deletes every CR of that kind. Those CRs have finalizers that
# close their graph edges on the way out, so uninstall with the operator still
# running — otherwise the edges stay open in the Knowledge Graph forever,
# asserting enforcement by a policy that no longer exists.
.PHONY: uninstall
uninstall: ## Remove the CRDs (deletes all CRs — read the note above first).
	$(KUBECTL) delete --ignore-not-found -k config/crd

.PHONY: deploy
deploy: ## Install CRDs + RBAC + ServiceAccount + the operator Deployment.
	@echo "NOTE: K8BOSS_CLUSTER_ID is blank in config/manager/manager.yaml."
	@echo "      The operator will crash-loop until it is set for this cluster."
	$(KUBECTL) apply -k config/default

.PHONY: undeploy
undeploy:
	$(KUBECTL) delete --ignore-not-found -k config/default

.PHONY: render
render: ## Print the full install without applying it.
	$(KUBECTL) kustomize config/default

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
