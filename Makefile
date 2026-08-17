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

##@ OLM bundle (OperatorHub)
#
# Packaging for OLM, which is a DIFFERENT install path from `make deploy`:
# config/default installs the operator directly, while the bundle hands OLM a
# ClusterServiceVersion and lets it own placement, RBAC and upgrades. The two
# share config/{crd,rbac,manager} and diverge at config/{default,manifests} —
# see the comment in config/manifests/kustomization.yaml for why the bundle
# cannot just consume config/default.
#
# Tooling, both expected on PATH:
#   brew install operator-sdk kustomize

OPERATOR_SDK ?= operator-sdk
KUSTOMIZE ?= kustomize

# The operator's own semver, and the CSV name stamp (k8boss-operator.vX.Y.Z).
# NOT the same thing as IMG's tag: the image is whatever CI published, the
# bundle version is what users see and upgrade between. Bump on every
# submission — community-operators rejects a re-push of an existing version.
VERSION ?= 0.1.0
CHANNELS ?= alpha
DEFAULT_CHANNEL ?= alpha

# What the CSV tells OLM to run. Override with a DIGEST for a real submission:
#   make bundle OPERATOR_IMG=ghcr.io/aesaganda/k8boss/k8boss-operator@sha256:...
# A floating tag would let the running operator change under a CSV that claims
# to pin it, which is the thing digest pinning exists to prevent.
OPERATOR_IMG ?= ghcr.io/aesaganda/k8boss/k8boss-operator:latest

BUNDLE_IMG ?= ghcr.io/aesaganda/k8boss/k8boss-operator-bundle:v$(VERSION)

.PHONY: bundle
bundle: ## Generate bundle/ (CSV + CRDs + metadata) from config/manifests.
	# config/manifests/bases/*.clusterserviceversion.yaml is hand-maintained,
	# so `operator-sdk generate kustomize manifests` is deliberately NOT run
	# here — it would re-prompt for and flatten fields that were written by
	# hand on purpose (description, installModes rationale, descriptors).
	cd config/manifests && $(KUSTOMIZE) edit set image \
	  ghcr.io/aesaganda/k8boss/k8boss-operator=$(OPERATOR_IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle \
	  --overwrite --version $(VERSION) \
	  --channels=$(CHANNELS) --default-channel=$(DEFAULT_CHANNEL)
	# The containerImage annotation must be byte-identical to the image a
	# container actually runs: community-operators CI greps the CSV for the
	# annotation's value and hard-fails when it appears fewer than twice
	# ("Value of metadata.annotations.containerImage not used in any
	# container"). Stamping it from the same variable that set the Deployment
	# image makes the two consistent by construction rather than by review —
	# otherwise overriding OPERATOR_IMG silently produces a bundle that
	# validates locally and fails in the submission pipeline.
	# -i.bak + rm: the bare -i spelling differs between GNU and BSD sed.
	sed -i.bak 's|^    containerImage: .*|    containerImage: $(OPERATOR_IMG)|' \
	  bundle/manifests/k8boss-operator.clusterserviceversion.yaml
	# Restore createdAt from the base. generate bundle stamps "now" on every
	# run, so without this the bundle differs from itself between two identical
	# builds and the drift gate in ci.yml could never pass. Keeping the base's
	# value also makes the field mean what it says — the date the version was
	# cut, not the date CI last ran.
	@created=$$(grep -m1 '^    createdAt:' \
	    config/manifests/bases/k8boss-operator.clusterserviceversion.yaml \
	  | sed 's|^ *createdAt: *||'); \
	sed -i.bak "s|^    createdAt:.*|    createdAt: $$created|" \
	  bundle/manifests/k8boss-operator.clusterserviceversion.yaml
	rm -f bundle/manifests/k8boss-operator.clusterserviceversion.yaml.bak
	$(MAKE) bundle-validate

.PHONY: bundle-validate
bundle-validate: ## Run the bundle validators community-operators gates on.
	# The default suite only checks the bundle parses. `operatorframework`
	# and `good-practices` are the ones whose failures come back as review
	# comments, so run them here rather than discovering them in a PR.
	$(OPERATOR_SDK) bundle validate ./bundle
	$(OPERATOR_SDK) bundle validate ./bundle --select-optional suite=operatorframework
	$(OPERATOR_SDK) bundle validate ./bundle --select-optional name=good-practices

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	docker build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push:
	docker push $(BUNDLE_IMG)

# End-to-end proof that the thing actually installs, which is the one check
# that catches a CSV that validates but never reaches Succeeded (a missing
# RBAC rule, an unsatisfiable install mode, a crash-looping manager). Needs a
# cluster with OLM: `kind create cluster && operator-sdk olm install`.
.PHONY: bundle-run
bundle-run: ## Install the bundle into the current cluster via OLM.
	$(OPERATOR_SDK) run bundle $(BUNDLE_IMG) --timeout 5m

.PHONY: bundle-cleanup
bundle-cleanup:
	$(OPERATOR_SDK) cleanup k8boss-operator

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
