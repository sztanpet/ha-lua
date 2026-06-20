BIN     := ha-lua
GOFLAGS := -trimpath -ldflags="-s -w"
GOPATH  := $(shell go env GOPATH)

BENCH_BASELINE := benchmarks/baseline.txt
BENCH_CURRENT  := benchmarks/current.txt
BENCH_FLAGS    := -run='^$$' -bench=. -benchmem -count=5

.PHONY: build run test bench bench-update bench-compare vet staticcheck lint check tidy fmt hooks profile-cpu trace update-deps release push

build:
	go build $(GOFLAGS) -o $(BIN) ./cmd/ha-lua

# Push the current branch to every configured remote.
push:
	@for remote in $$(git remote); do \
	    echo "==> pushing to $$remote"; \
	    git push $$remote HEAD || exit $$?; \
	done

# Run the daemon in development mode against the standalone dev config
# (config.dev.yaml), outside Home Assistant. Ctrl-C to stop.
run:
	go run ./cmd/ha-lua --config config.dev.yaml

test:
	go test -race ./...

bench:
	go test $(BENCH_FLAGS) ./... | tee $(BENCH_CURRENT)

bench-compare: bench
	@if [ -f $(BENCH_BASELINE) ]; then \
	    $(GOPATH)/bin/benchstat $(BENCH_BASELINE) $(BENCH_CURRENT); \
	else \
	    echo "WARN: no benchmark baseline; run 'make bench-update' to create one."; \
	fi

bench-update: bench
	cp $(BENCH_CURRENT) $(BENCH_BASELINE)

vet:
	go vet ./...

staticcheck:
	$(GOPATH)/bin/staticcheck ./...

lint:
	$(GOPATH)/bin/golangci-lint run

fmt:
	gofmt -l -w .

check: vet staticcheck lint test

# Install the git pre-commit hook (gofmt + vet + staticcheck + lint)
hooks:
	git config core.hooksPath .githooks

tidy:
	go mod tidy

# Update all Go dependencies (including test and tool deps), then tidy.
# Review the go.mod diff and run 'make check' before committing.
# golangci-lint is NOT managed here — it is installed as a release binary,
# never via go install (see AI.state).
update-deps:
	go get -u -t ./...
	go mod tidy
	@echo "Dependencies updated. Review 'git diff go.mod' and run 'make check'."

# Cut a release: bump the config.yaml version, commit it, and create the
# annotated vX.Y.Z tag. Pushing the tag (which is what triggers the GHCR
# build in .github/workflows/release.yml) is left to you on purpose:
#
#   make release VERSION=1.2.0
#   git push --follow-tags
#
# Update CHANGELOG.md with the new version's section before running this.
release:
	@test -n "$(VERSION)" || { echo "ERROR: VERSION is required, e.g. make release VERSION=1.2.0"; exit 1; }
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "ERROR: VERSION must be semver X.Y.Z (no leading v)"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "ERROR: working tree is not clean; commit or stash first"; exit 1; }
	@if git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null; then echo "ERROR: tag v$(VERSION) already exists"; exit 1; fi
	@grep -q "## $(VERSION)" CHANGELOG.md || echo "WARN: CHANGELOG.md has no '## $(VERSION)' section yet"
	sed -i -E 's/^version: ".*"/version: "$(VERSION)"/' config.yaml
	@grep -qx 'version: "$(VERSION)"' config.yaml || { echo "ERROR: failed to update version in config.yaml"; exit 1; }
	git add config.yaml
	git commit -m "release: v$(VERSION)"
	git tag -a "v$(VERSION)" -m "v$(VERSION)"
	@echo "Tagged v$(VERSION). Push it to trigger the GHCR build:"
	@echo "    git push --follow-tags"

profile-cpu:
	go tool pprof -http=:8080 "http://localhost:6060/debug/pprof/profile?seconds=30"

trace:
	curl -s "http://localhost:6060/debug/pprof/trace?seconds=5" -o trace.out
	go tool trace trace.out
