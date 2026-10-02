.PHONY: all build build-server build-noui build-noplugins test test-ui test-ui-unit test-ui-browser test-noui test-race-storage test-browser test-interop test-examples check check-core check-race-partition check-example check-chart check-release-scripts print-contract-matrix clean

VERSION ?= 1.0.0-rc.2
LDFLAGS := -s -w -X github.com/suxen-project/suxen/internal/server.Version=$(VERSION)
SUXENCTL_LDFLAGS := -s -w -X main.version=$(VERSION)
# Plugins are compiled in by default and excluded per plugin with build tags:
#   SUXEN_BUILD_TAGS=suxen_no_gcs,suxen_no_maven,suxen_no_git,suxen_no_go,suxen_no_cargo,suxen_no_npm,suxen_no_pypi make build-server
SUXEN_BUILD_TAGS ?=
SUXEN_OUTPUT ?= bin/suxen
SUXEN_TAGS_ARGUMENT := $(if $(strip $(SUXEN_BUILD_TAGS)),-tags='$(SUXEN_BUILD_TAGS)',)
SUXEN_NO_PLUGIN_TAGS := suxen_no_gcs,suxen_no_maven,suxen_no_git,suxen_no_go,suxen_no_cargo,suxen_no_npm,suxen_no_pypi

# These packages exercise the PostgreSQL, S3, and GCS backends and race in the
# storage-backends CI job with those services present. The source race set is
# derived as `go list` minus these, so a new package is raced by default and the
# two sets cannot overlap; check-race-partition guards that these still exist.
STORAGE_RACE_PACKAGES := \
	github.com/suxen-project/suxen/internal/blob \
	github.com/suxen-project/suxen/internal/store \
	github.com/suxen-project/suxen/internal/server \
	github.com/suxen-project/suxen/plugins/blobstore/gcs
SOURCE_RACE_PACKAGES = $(filter-out $(STORAGE_RACE_PACKAGES),$(shell go list ./...))

# `make check` races every package locally; the source CI job sets
# SUXEN_CI_RACE_SPLIT=1 to race only the source set, so internal/server is not
# raced twice across the source and storage-backends jobs.
CORE_RACE_PACKAGES := ./...
ifeq ($(SUXEN_CI_RACE_SPLIT),1)
CORE_RACE_PACKAGES := $(SOURCE_RACE_PACKAGES)
endif

# The only source selected differently by `noui` is UI routing, so the tagged
# pass need only compile every package under the tag (the -run over ./... builds
# each test binary) and assert the UI-free routes return 404.
NOUI_RUN := ^(TestUIFreeBuildRoutesReturnNotFound|TestUIFreeHandlerReturnsNotFound)$$

all: check build

build: build-server
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='$(SUXENCTL_LDFLAGS)' -o bin/suxenctl ./cmd/suxenctl

build-server:
	mkdir -p bin
	CGO_ENABLED=0 go build $(SUXEN_TAGS_ARGUMENT) -trimpath -ldflags='$(LDFLAGS)' -o $(SUXEN_OUTPUT) ./cmd/suxen

build-noui:
	$(MAKE) build-server SUXEN_BUILD_TAGS=noui SUXEN_OUTPUT=bin/suxen-noui

build-noplugins:
	$(MAKE) build-server SUXEN_BUILD_TAGS=$(SUXEN_NO_PLUGIN_TAGS) SUXEN_OUTPUT=bin/suxen-noplugins

test:
	go test ./...
	$(MAKE) test-ui
	$(MAKE) test-noui

test-ui:
	node --test internal/server/ui_testdata/*.test.mjs

test-ui-unit:
	node --test $(filter-out %.browser.test.mjs,$(wildcard internal/server/ui_testdata/*.test.mjs))

test-ui-browser:
	node --test internal/server/ui_testdata/*.browser.test.mjs

test-noui:
	go test -tags=noui -run '$(NOUI_RUN)' ./...

# The storage-dependent race job. The backend-integration tests are gated behind
# the suxen_integration build tag so a default `go test ./...` stays hermetic; this
# target builds them and races the storage packages against the CI-provided
# PostgreSQL, S3, and GCS services. Each integration test skips when its own
# SUXEN_TEST_* service is not configured.
# The browser tests run after CI stops its backend service containers. Chromium
# can be killed by the memory pressure of race instrumentation plus the services.
# The remaining server and storage tests still run under the race detector.
BROWSER_TESTS := ^(TestAuthenticatedAdministrationUIBrowserCRUD|TestAdministrationUIBrowserCookieCSRF|TestAdministrationUIBrowserOIDCCallbackAndLogout|TestAdministrationUIBrowserExactPolicyNumbers|TestAdministrationUIRendersInBrowser|TestArtifactScriptsCannotExecuteInAdministrationOrigin)$$
STORAGE_BROWSER_SKIP := $(if $(filter 1,$(SUXEN_CI_BROWSER_SPLIT)),-skip='$(BROWSER_TESTS)',)
test-race-storage:
	go test -race -p 1 -count=1 -tags=suxen_integration $(STORAGE_BROWSER_SKIP) $(STORAGE_RACE_PACKAGES)

test-browser:
	go test -parallel 1 -count=1 -run '$(BROWSER_TESTS)' ./internal/server

test-interop:
	test/e2e/run.sh

# Run every example's self-test: each boots a native suxen (built on demand) and
# drives the real client toolchain in a pinned Docker image. Needs bats + Docker;
# network-dependent cases skip cleanly when the upstream is unreachable.
test-examples:
	bats examples/*/test.bats

check: check-core check-example check-chart check-release-scripts

# The in-tree checks. Split out so CI can validate the example module in a
# separate job that also repairs its go.sum drift.
check-core:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go vet -tags=noui ./...
	go vet -tags=$(SUXEN_NO_PLUGIN_TAGS) ./...
	go vet -tags=suxen_integration ./...
	$(MAKE) check-race-partition
	go test -race $(CORE_RACE_PACKAGES)
	$(MAKE) $(if $(filter 1,$(SUXEN_CI_UI_BROWSER_SPLIT)),test-ui-unit,test-ui)
	$(MAKE) test-noui

# Prove every storage race package still exists so a rename cannot silently drop
# it from CI; the source set is `go list` minus these, so coverage and
# disjointness of the two race sets otherwise hold by construction.
check-race-partition:
	@present="$$(go list ./... | tr '\n' ' ')"; \
	for pkg in $(STORAGE_RACE_PACKAGES); do \
		case " $$present " in \
			*" $$pkg "*) ;; \
			*) echo "Storage race package not found in module: $$pkg" >&2; exit 1;; \
		esac; \
	done

# The out-of-tree example plugin is a separate module that consumes the blob,
# format, and API SPIs plus suxencmd and the default plugin import through a
# replace directive. Checking it proves an SPI change still compiles for an
# external consumer and that its manifest matches the tree.
# The host integration test builds a server subprocess from root sources, which
# are not all in the test binary's import graph. Do not reuse its test cache.
check-example:
	cd examples/plugin-memblob && go mod tidy -diff && go build ./... && go vet ./... && go test -count=1 ./...

check-chart:
	helm lint charts/suxen
	scripts/check-helm-render.sh
	scripts/check-helm-service-port.sh
	scripts/check-helm-update-strategy.sh
	scripts/check-helm-push.sh

check-release-scripts:
	scripts/check-release-download-order.sh
	scripts/check-release-verifier-isolation.sh
	scripts/check-release-tag-test.sh
	scripts/promote-release-images-test.sh
	scripts/check-sdk-build.sh

# Print the contract surface version matrix for inclusion in release notes.
print-contract-matrix:
	@scripts/print-contract-matrix.sh

clean:
	rm -rf bin data
