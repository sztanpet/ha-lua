//go:build tools

package tools

import (
	_ "golang.org/x/perf/cmd/benchstat"
	_ "honnef.co/go/tools/cmd/staticcheck"
)
