BIN     := ha-lua
GOFLAGS := -trimpath -ldflags="-s -w"
GOPATH  := $(shell go env GOPATH)

BENCH_BASELINE := benchmarks/baseline.txt
BENCH_CURRENT  := benchmarks/current.txt
BENCH_FLAGS    := -run='^$$' -bench=. -benchmem -count=5

.PHONY: build test bench bench-update bench-compare vet staticcheck lint check tidy fmt profile-cpu trace

build:
	go build $(GOFLAGS) -o $(BIN) ./cmd/ha-lua

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
	golangci-lint run

fmt:
	gofmt -w ./...

check: vet staticcheck test

tidy:
	go mod tidy

profile-cpu:
	go tool pprof -http=:8080 "http://localhost:6060/debug/pprof/profile?seconds=30"

trace:
	curl -s "http://localhost:6060/debug/pprof/trace?seconds=5" -o trace.out
	go tool trace trace.out
