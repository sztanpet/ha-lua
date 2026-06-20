# Repository-root Makefile. The Go project and its full build/test/lint
# targets live in the ha-lua/ add-on subfolder (see ha-lua/Makefile). This
# file covers the things that operate on the whole repository: pushing,
# cutting releases, and setting up a fresh development checkout.

.PHONY: install install-tools hooks push release

# One-shot setup for a fresh checkout: analyzers + git hooks.
install: install-tools hooks

# Every analyzer (staticcheck, benchstat, golangci-lint) is a tool
# dependency — the `tool` directives in ha-lua/go.mod — so `go tool` builds
# each on demand at the version pinned there. Nothing is installed into a
# bin directory; this just pre-fetches the module sources so the first
# `go tool` run is offline-fast.
install-tools:
	cd ha-lua && go mod download

# Point git at the repo's pre-commit hook (gofmt + vet + staticcheck + lint).
hooks:
	git config core.hooksPath .githooks

# Delegate to the Go module's Makefile.
push:
	$(MAKE) -C ha-lua push

release:
	$(MAKE) -C ha-lua release VERSION=$(VERSION)
