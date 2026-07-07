# State: event-to-action latency (event-latency-spec.md)

Working state for the latency track. Spec: `event-latency-spec.md`.
Global decisions live in `../AI.state`.

Status: **spec written 2026-07-07, awaiting user review before build.**

## History (pre-spec rounds, shipped in v3.0.1)

Rounds 1–2 are recorded in `bundled-examples.md` ("mirrored_switches latency
follow-up"): the 100 ms batch window (example now opts into
ha.immediate_events(), window documented loudly) and SQLite
synchronous=NORMAL (per-event fsync jitter was on the dispatch path).

## Round 3 diagnosis (2026-07-07, led to this spec)

- User: variance much lower after v3.0.1, but a quick on→off still shows
  ~500 ms on the off.
- Cause (code-confirmed, not yet field-confirmed — M1 instruments it):
  ha.call_service is synchronous since v2.3.0 (SendCommandWaitResult blocks
  until HA's result frame, which HA sends only after the service COMPLETES —
  Zigbee radio round trip included). The script event loop is serialized, so
  the off event queues behind the on handler's ack wait.
- Persistence decoupling (spec §3) does NOT fix this — separate mechanism,
  both are in the spec.

## Milestones

- M0 measurement harness — **DONE** (6c2f5f9 internal/e2e + baseline
  commit). Fake HA WS server, real client, verbatim main.go router loop,
  file-backed SQLite, supervisor-run mirror script. Benchmarks:
  EventToServiceCall (+p50/p99 via ReportMetric), ...BusyKV, QuickToggle
  (off-ns/op = the user's half second). Dev-machine baseline: mean ~0.4ms,
  p99 ~5ms, KV noise p99 ~7-10ms, off-ns/op = 100.6ms vs the 100ms
  simulated ack — BOTH spec diagnoses confirmed by measurement.
  Workflow: make bench-compare after each milestone; baselines in
  benchmarks/baseline.txt. NOTE: make bench-update re-RUNS the suite
  (bench-update: bench); to promote an existing run, cp current.txt
  baseline.txt.
- M1 dispatch-delay instrumentation — **DONE** (96ed468). ha.Event.ReceivedAt
  stamped in the WS read loop; runner logs queue-to-handler delay (debug,
  warn ≥250ms — clear of the 100ms batch window, so a warn is always real).
- M2 call_service { wait = false } — **DONE** (3c9ede5 client split +
  022e224 binding/docs). SendCommandAsync: synchronous ordered write,
  verdict on a 1-buffered channel, pending-map cleanup in its goroutine;
  SendCommandWaitResult is a blocking wrapper. Binding: opts 4th arg;
  wait=false sends inline (send errors raise) then awaits the verdict off
  the goroutine; failures ride runner.asyncErrCh (never closed, like reqCh
  — the event channel closes on stop, a late verdict must not panic) into
  dispatchException → on_exception. Nil-dep check is per-path now (async
  wiring alone is enough for wait=false). MEASURED: QuickToggleNoWait
  off-ns/op 0.37ms vs QuickToggle 100.7ms (~270x). mirrored_switches
  example + lua_api.md + DOCS.md updated.
- M3 memory-authoritative state mirror — pending
- M4 background batched writer — pending
- M5 docs + release — pending (bench-compare against baseline at M5)

## Decisions so far

- call_service default STAYS synchronous (wait=true); async is opt-in via a
  4th opts arg. Rejected: separate _async function, flipping the default.
- Memory mirror is required before async persistence — async-only writes
  would let ha.get_state race the queue (stale reads).
- states table kept (write-only) in phase 1; dropping it is a phase 2
  decision after production soak.
- Overflow: block with warn (never drop history silently). Writer failure:
  retry once, then drop batch loudly; memory stays authoritative.
