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

- M1 dispatch-delay instrumentation — pending
- M2 call_service { wait = false } + async error → on_exception — pending
- M3 memory-authoritative state mirror — pending
- M4 background batched writer — pending
- M5 docs + release — pending

## Decisions so far

- call_service default STAYS synchronous (wait=true); async is opt-in via a
  4th opts arg. Rejected: separate _async function, flipping the default.
- Memory mirror is required before async persistence — async-only writes
  would let ha.get_state race the queue (stale reads).
- states table kept (write-only) in phase 1; dropping it is a phase 2
  decision after production soak.
- Overflow: block with warn (never drop history silently). Writer failure:
  retry once, then drop batch loudly; memory stays authoritative.
