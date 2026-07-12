# Build + install the flywheel CLI from source.
#
# `make install` compiles the binary with a real version stamp — so
# `flywheel.BuildVersion`, and the `flywheel.version` that `init` records into
# flywheel.yaml, is a meaningful git ref instead of the default `v0.0.0-dev` —
# builds the four runtime images, and installs the shell-completion script for
# your $SHELL, cleanly replacing any older copy. The binary on its own can't do
# much useful work without the images it runs in-cluster, so they build together.

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -X github.com/cobr-io/flywheel.BuildVersion=$(VERSION)

# The four runtime images, built from Dockerfile.<name> in the repo root and
# tagged flywheel-dev/<name>:$(IMAGE_TAG). This mirrors schema.ImageNames;
# adding image N+1 is a documented checklist (docs/dev/add-controller-image.md),
# and converge's image-agreement test fails if the bootstrap templates and
# schema.ImageNames disagree. `make images` is the single build recipe — the CI
# e2e recipe (.github/workflows/e2e-recipe.yml) and scripts/e2e.sh call it too.
# IMAGE_TAG defaults to `dogfood` to match the `flywheel-dev/<img>:dogfood`
# pins in a per-developer flywheel.yaml.local. `flywheel up` content-
# addresses the imported image by its build digest at deploy time, so the
# Deployment rolls when content changes without per-build tag bookkeeping here.
IMAGES := git-server git-auto-sync image-builder-controller git-deploy-controller
IMAGE_TAG ?= dogfood

# Resolve the install dir the same way `go install` does: $GOBIN, else
# $GOPATH/bin. Used only for the confirmation message.
GOBIN := $(shell go env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell go env GOPATH)/bin
endif

.DEFAULT_GOAL := build
.PHONY: build install uninstall images push-local completions completions-all e2e help

## build: compile + install the version-stamped binary into GOBIN
build:
	go install -ldflags "$(LDFLAGS)" ./cmd/flywheel
	@echo "installed $(GOBIN)/flywheel ($(VERSION))"

## install: build the binary + runtime images, then install shell completions
install: build images completions

## uninstall: remove the installed binary ($(GOBIN)/flywheel) + shell completions
# The inverse of `make install`'s binary + completions steps. Delegates to
# uninstall.sh so the completion paths live in one place; INSTALL_DIR=$(GOBIN)
# targets the source-install location and USE_SUDO=false keeps it in your own
# dirs (never touches ~/.config/flywheel or the embed cache — use uninstall.sh
# --purge / --purge-config for those).
uninstall:
	@INSTALL_DIR="$(GOBIN)" USE_SUDO=false bash uninstall.sh

## images: build the four runtime images as flywheel-dev/<name>:$(IMAGE_TAG)
# The two controller images COPY a host-built binary instead of compiling Go
# in-image (issue #46 — the in-image build compiled under QEMU at release). We
# cross-compile them for GOOS=linux/$(host arch) — matching docker's default
# build platform here — into a throwaway context dir and build from that; the
# script-only images (git-server, git-auto-sync) still build from the repo root.
images:
	@tag="$(IMAGE_TAG)"; \
	ctx="$$(mktemp -d)"; trap 'rm -rf "$$ctx"' EXIT; \
	for c in image-builder-controller git-deploy-controller; do \
		CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o "$$ctx/$$c" "./cmd/$$c"; \
	done; \
	for img in $(IMAGES); do \
		echo "==> building flywheel-dev/$$img:$$tag"; \
		case "$$img" in \
			image-builder-controller|git-deploy-controller) bctx="$$ctx" ;; \
			*) bctx="." ;; \
		esac; \
		docker build -q -t "flywheel-dev/$$img:$$tag" -f "Dockerfile.$$img" "$$bctx" >/dev/null; \
	done; \
	printf 'built:'; for i in $(IMAGES); do printf ' flywheel-dev/%s:%s' "$$i" "$$tag"; done; echo

## push-local: push the built dogfood images into a cluster's local registry (REGISTRY_PORT=<port> required)
# Dogfood images are now served from the cluster's LOCAL REGISTRY (not a per-node
# `k3d image import` side-load), under a content-addressed `dogfood-<id>` tag —
# matching imagepin.dogfoodTag, so a node pulls on demand and a content change
# forces a re-pull. Find the host port with `k3d registry list` (it's the
# client's cluster.registry_port, e.g. 50001).
#
# NOTE: this only pre-populates the registry. The usual path is `flywheel up`,
# which pushes AND rolls the Deployment to the new content-addressed ref. Use
# push-local only when you want the bits in the registry without a reconcile.
push-local:
	@test -n "$(REGISTRY_PORT)" || { \
		echo "REGISTRY_PORT is required, e.g. 'make push-local REGISTRY_PORT=50001'" >&2; \
		echo "(your cluster.registry_port; see flywheel.yaml or 'k3d registry list')" >&2; \
		exit 1; }
	@for img in $(IMAGES); do \
		id=$$(docker inspect --type=image --format '{{.Id}}' "flywheel-dev/$$img:$(IMAGE_TAG)" | sed 's/^sha256://' | cut -c1-12); \
		ref="localhost:$(REGISTRY_PORT)/$$img:dogfood-$$id"; \
		echo "==> pushing flywheel-dev/$$img:$(IMAGE_TAG) → $$ref"; \
		docker tag "flywheel-dev/$$img:$(IMAGE_TAG)" "$$ref"; \
		docker push "$$ref"; \
	done

## completions: (re)install the shell-completion script for $SHELL
completions:
	@FLYWHEEL="$(GOBIN)/flywheel" bash scripts/install-completions.sh

## completions-all: install completions for bash + zsh + fish
completions-all: build
	@FLYWHEEL="$(GOBIN)/flywheel" bash scripts/install-completions.sh all

## e2e: run the k3d-e2e suite locally (build images, init/up/scenarios/down)
e2e: build
	bash scripts/e2e.sh

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
