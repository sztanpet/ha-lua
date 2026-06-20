# Repository-root Makefile. The Go project and its full build/test/lint
# targets live in the ha-lua/ add-on subfolder (see ha-lua/Makefile). This
# file covers the things that operate on the whole repository: pushing,
# cutting releases, and setting up a fresh development checkout.

# golangci-lint is pinned and installed as a release binary, never via
# `go install` (see ha-lua/AI.state). Bump in lockstep with .golangci.yml.
GOLANGCI_VERSION := v2.12.2
GOBIN            := $(shell go env GOPATH)/bin

.PHONY: install install-tools hooks push release

# One-shot setup for a fresh checkout: analyzers + git hooks.
install: install-tools hooks

# Install every static analyzer the build uses. staticcheck and benchstat
# are tracked in ha-lua/tools.go, so `go install` (run from the module)
# resolves them at the versions pinned in ha-lua/go.mod.
install-tools:
	cd ha-lua && go install honnef.co/go/tools/cmd/staticcheck
	cd ha-lua && go install golang.org/x/perf/cmd/benchstat
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
	    | sh -s -- -b "$(GOBIN)" $(GOLANGCI_VERSION)

# Point git at the repo's pre-commit hook (gofmt + vet + staticcheck + lint).
hooks:
	git config core.hooksPath .githooks

# Delegate to the Go module's Makefile.
push:
	$(MAKE) -C ha-lua push

release:
	$(MAKE) -C ha-lua release VERSION=$(VERSION)
