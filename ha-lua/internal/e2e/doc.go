// Package e2e holds end-to-end latency benchmarks for the full event
// pipeline: fake HA WebSocket server → ha.Client → state tracker →
// registry dispatch → Lua handler → ha.call_service → back to the fake
// server. Everything except the real network and Home Assistant itself.
//
// The package contains only test files; this file exists so the package
// builds. Benchmarks run via `make bench` and are compared across changes
// with `make bench-compare` — they are the measuring stick for the
// event-latency track (see event-latency-spec.md).
package e2e
