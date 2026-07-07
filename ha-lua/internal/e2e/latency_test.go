package e2e

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestPipelineDelivers is the harness sanity check that runs under
// `make test`: events flow end to end, produce the right service calls, and
// a quick on→off preserves command order. No timing assertions — those live
// in the benchmarks, where flakiness costs nothing.
func TestPipelineDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPipeline(t, ctx, mirrorScript, benchSeed(), 50*time.Millisecond)

	if err := p.HA.injectStateChanged(ctx, "switch.a", "on"); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, p)
	if first.Domain != "switch" || first.Service != "turn_on" {
		t.Fatalf("first call = %s.%s, want switch.turn_on", first.Domain, first.Service)
	}

	// Inject the off while the on handler is still parked in its ack wait.
	if err := p.HA.injectStateChanged(ctx, "switch.a", "off"); err != nil {
		t.Fatal(err)
	}
	second := nextCall(t, p)
	if second.Domain != "switch" || second.Service != "turn_off" {
		t.Fatalf("second call = %s.%s, want switch.turn_off", second.Domain, second.Service)
	}
	if second.RecvAt.Before(first.RecvAt) {
		t.Error("turn_off overtook turn_on")
	}
}

// BenchmarkEventToServiceCall is the headline number: wall time from a
// state_changed frame leaving the fake HA server to the resulting
// call_service command arriving back, through the real client, tracker
// write, dispatch, and Lua handler. Instant acks — nothing is parked.
func BenchmarkEventToServiceCall(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPipeline(b, ctx, mirrorScript, benchSeed(), 0)

	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if err := p.HA.injectStateChanged(ctx, "switch.a", toggle(i)); err != nil {
			b.Fatal(err)
		}
		nextCall(b, p)
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	drainLastAck()
	reportPercentiles(b, samples)
}

// BenchmarkEventToServiceCallBusyKV is the same path while another goroutine
// hammers global.Set on the shared write handle — the head-of-line blocking
// a busy script (or the purge job) inflicts on event dispatch today. The
// event-latency spec's §3 (background persistence) should collapse this back
// to BenchmarkEventToServiceCall's numbers.
func BenchmarkEventToServiceCallBusyKV(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPipeline(b, ctx, mirrorScript, benchSeed(), 0)

	noiseCtx, stopNoise := context.WithCancel(ctx)
	defer stopNoise()
	go func() {
		for i := 0; noiseCtx.Err() == nil; i++ {
			_ = p.Global.Set(noiseCtx, "noise", i)
		}
	}()

	samples := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if err := p.HA.injectStateChanged(ctx, "switch.a", toggle(i)); err != nil {
			b.Fatal(err)
		}
		nextCall(b, p)
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	drainLastAck()
	reportPercentiles(b, samples)
}

// BenchmarkQuickToggle reproduces the reported half-second: the ack for the
// first call is slow (deviceAck stands in for HA awaiting the Zigbee round
// trip), and the second event arrives while the handler is still parked in
// SendCommandWaitResult. "off-ns/op" is the number that matters: today it
// tracks deviceAck because the event loop is serialized; after the spec's §2
// ({ wait = false }) it should drop to the plain pipeline latency.
func BenchmarkQuickToggle(b *testing.B) {
	const deviceAck = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPipeline(b, ctx, mirrorScript, benchSeed(), deviceAck)

	var totalOff time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.HA.injectStateChanged(ctx, "switch.a", "on"); err != nil {
			b.Fatal(err)
		}
		nextCall(b, p) // turn_on arrives; its ack is now pending for deviceAck

		offSent := time.Now()
		if err := p.HA.injectStateChanged(ctx, "switch.a", "off"); err != nil {
			b.Fatal(err)
		}
		offCall := nextCall(b, p)
		totalOff += offCall.RecvAt.Sub(offSent)

		// Let the off ack drain so iterations don't overlap.
		time.Sleep(deviceAck + 10*time.Millisecond)
	}
	b.StopTimer()
	b.ReportMetric(float64(totalOff.Nanoseconds())/float64(b.N), "off-ns/op")
}

// drainLastAck gives the final iteration's in-flight ack a moment to reach
// the parked handler before the deferred cancel aborts it mid-call — pure
// teardown-noise prevention, outside the timed section.
func drainLastAck() { time.Sleep(20 * time.Millisecond) }

// reportPercentiles attaches p50/p99 to the benchmark output — the variance,
// not just the mean, is what a wall switch user feels.
func reportPercentiles(b *testing.B, samples []time.Duration) {
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p99 := samples[len(samples)*99/100]
	b.ReportMetric(float64(p50.Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns")
}
