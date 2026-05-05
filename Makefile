# Copyright 2026 Tenstorrent USA, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CONTAINER_TOOL ?= docker
MKDIR    ?= mkdir
DIST_DIR ?= $(CURDIR)/dist

export IMAGE_GIT_TAG ?= $(shell git describe --tags --always --dirty --match 'v*' 2>/dev/null || echo "v0.0.0-dev")
export CHART_GIT_TAG ?= $(shell git describe --tags --always --dirty --match 'chart/*' 2>/dev/null || echo "chart/v0.0.0-dev")

include $(CURDIR)/common.mk

BUILDIMAGE_TAG ?= golang$(GOLANG_VERSION)
BUILDIMAGE ?= $(IMAGE_NAME)-build:$(BUILDIMAGE_TAG)

CMDS := $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := assert-fmt vet lint
MAKE_TARGETS := binaries build check fmt test cmds coverage $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS) $(CMD_TARGETS)

DOCKER_TARGETS := $(patsubst %,docker-%, $(TARGETS))
.PHONY: $(TARGETS) $(DOCKER_TARGETS) all

GOOS ?= linux

all: check test build

binaries: cmds
ifneq ($(PREFIX),)
cmd-%: COMMAND_BUILD_OPTIONS = -o $(PREFIX)/$(*)
endif
cmds: $(CMD_TARGETS)
$(CMD_TARGETS): cmd-%:
	CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' GOOS=$(GOOS) \
		go build -ldflags "-s -w -X main.version=$(VERSION)" $(COMMAND_BUILD_OPTIONS) $(MODULE)/cmd/$(*)

build:
	GOOS=$(GOOS) go build ./...

check: $(CHECK_TARGETS)

fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l -w

assert-fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l > fmt.out
	@if [ -s fmt.out ]; then \
		echo "\nERROR: The following files are not formatted:\n"; \
		cat fmt.out; \
		rm fmt.out; \
		exit 1; \
	else \
		rm fmt.out; \
	fi

lint:
	golangci-lint run ./...

vet:
	go vet $(MODULE)/...

COVERAGE_FILE := coverage.out
test: build cmds
	go test -v -coverprofile=$(COVERAGE_FILE) $(MODULE)/...

coverage: test
	go tool cover -func=$(COVERAGE_FILE)

# Generate an image for containerized builds.
# Note: this image is local only.
.PHONY: .build-image
.build-image: docker/Dockerfile.devel
	if [ x"$(SKIP_IMAGE_BUILD)" = x"" ]; then \
		$(CONTAINER_TOOL) build \
			--progress=plain \
			--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
			--tag $(BUILDIMAGE) \
			-f $(^) \
			docker; \
	fi

ifeq ($(CONTAINER_TOOL),podman)
CONTAINER_TOOL_OPTS=-v $(PWD):$(PWD):Z
else
CONTAINER_TOOL_OPTS=-v $(PWD):$(PWD):z --user $$(id -u):$$(id -g)
endif

$(DOCKER_TARGETS): docker-%: .build-image
	@echo "Running 'make $(*)' in container $(BUILDIMAGE)"
	$(CONTAINER_TOOL) run \
		--rm \
		-e HOME=$(PWD) \
		-e GOCACHE=$(PWD)/.cache/go \
		-e GOPATH=$(PWD)/.cache/gopath \
		$(CONTAINER_TOOL_OPTS) \
		-w $(PWD) \
		$(BUILDIMAGE) \
			make $(*)

# Start an interactive shell using the development image.
.PHONY: .shell
.shell:
	$(CONTAINER_TOOL) run \
		--rm \
		-ti \
		-e HOME=$(PWD) \
		-e GOCACHE=$(PWD)/.cache/go \
		-e GOPATH=$(PWD)/.cache/gopath \
		$(CONTAINER_TOOL_OPTS) \
		-w $(PWD) \
		$(BUILDIMAGE)
