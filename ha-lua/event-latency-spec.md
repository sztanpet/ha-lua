# Event-to-action latency — Specification

> **Working state:** [`state/event-latency.md`](state/event-latency.md) — implementation progress and decisions.

Status: **awaiting review, then build.** Rounds 1–2 (the 100 ms batch window
opt-out and `synchronous=NORMAL`) shipped in v3.0.1 and are background here;
this spec covers the two remaining mechanisms. Milestones in §6.

## 1. Goal

A latency-sensitive script (a light following a wall switch) should be
indistinguishable from a built-in HA automation. After v3.0.1 the median is
close, but two structural couplings remain:

- **§2 — the script event loop blocks during `ha.call_service`.** This is the
  user-felt "half a second when turning it on and then off": the *off* event
  queues behind the *on* handler, which is parked waiting for HA's result
  frame.
- **§3 — persistence sits on the dispatch critical path.** Every
  `state_changed` commits to SQLite before any script sees it, on a write
  connection shared with script KV writes, the scheduler, and the hourly
  purge. `synchronous=NORMAL` removed the per-commit fsync; checkpoint and
  head-of-line stalls remain.

## 2. Problem A: synchronous call_service blocks the event loop

Since v2.3.0, `ha.call_service` uses `SendCommandWaitResult`: it registers a
waiter for the command id and blocks the script goroutine until HA's `result`
frame arrives (10 s liveness ceiling). HA sends that frame only after the
service call **completes** — for a Zigbee switch that includes the radio
round trip, so 100–500 ms is normal.

The block is per-script and serializes the script's whole event loop
(`runner.go` handles events strictly in order). Timeline of the observed bug:

```
t=0     switch A "on" event  → handler: call_service turn_on(B) — blocks
t≈?     B's "on" echo event  → queued  (handler still parked)
t=Δ     switch A "off" event → queued  (handler still parked)
t=ack   result frame arrives → echo handled (fast) → A "off" finally handled
```

The off command leaves the daemon `ack` late. Nothing about persistence,
batching, or SQLite is involved — §3 alone would not fix this.

### 2.1 Fix: opt-in non-blocking call

`ha.call_service(domain, service, data, opts)` gains an optional 4th argument:

```lua
ha.call_service("switch", "turn_on", { entity_id = partner }, { wait = false })
```

- **Default stays `wait = true`** — the v2.3.0 synchronous semantics
  (inline error raising) are a deliberate decision and plenty of scripts
  want them. Non-breaking ⇒ minor version bump.
- With `wait = false`, the binding still **raises inline** on marshal errors
  and no-connection (send-side failures are synchronous either way), then
  returns as soon as the command is written to the socket.
- The result frame is consumed by a goroutine. On HA-side failure it routes
  an error to the script's **`ha.on_exception`** handler — matching the
  project's error philosophy — with `slog.Error` as the usual fallback.
  Delivery mechanism: a new runner event kind (alongside `HAEvent` /
  `TimerFired`) so the exception handler runs on the script's own goroutine;
  the LState is never touched from outside.
- **Ordering:** commands are written to the WS in call order regardless of
  `wait`; the client already serializes writes. Two async calls cannot
  overtake each other on the wire. (HA may still *execute* long-running
  services concurrently — same as today.)

Rejected: a separate `ha.call_service_async` function (two names for one
operation), and flipping the default to async (breaking, and inline errors
are the right default for scripts that then read state).

## 3. Problem B: persistence on the dispatch critical path

`main.go`'s router goroutine runs `tracker.HandleStateChanged` (a committed
transaction) before `reg.Dispatch(ev)` — for **every** state change in the
home, serialized on the `SetMaxOpenConns(1)` write handle. The ordering is
load-bearing today: it is what makes `ha.get_state` reflect every event
dispatched before the one being handled.

Remaining latency couplings even at `synchronous=NORMAL`:

- WAL autocheckpoint (~1000 pages) runs inside a commit on the write path.
- The hourly purge DELETE holds the write connection for its duration.
- `store.set` / `global.set` from any script goroutine queues ahead of the
  tracker write.

### 3.1 Fix: memory-authoritative mirror + background writer

The naive fix (same writes, async) is wrong: `ha.get_state` would race the
queue and read pre-event state. Decoupling persistence requires moving the
**read path** off SQLite too:

1. **In-memory current-state map** owned by the tracker
   (`map[string]ha.StateData` + `sync.RWMutex`). The router applies each
   `state_changed` (upsert or delete-on-nil-new_state) to the map
   synchronously — microseconds — then dispatches. The consistency guarantee
   is unchanged, just backed by memory.
2. **Reads go to the map**: `GetState`, `GetEntities`, `GetEntityIDs`. Glob
   matching via `filepath.Match` (already the runner's pattern matcher, so
   script-facing glob semantics converge). `GetHistory` stays on SQLite.
3. **One background writer goroutine** owns all tracker persistence: a
   buffered channel (cap ~1024) of state-change records; each wakeup drains
   everything available and commits it as **a single batched transaction**
   (bursts amortize into one commit — cheaper than today). Single FIFO
   consumer ⇒ `state_history` order preserved. On shutdown, drain, then
   close.
4. **Overflow policy: block** (backpressure on the router). Dropping rows
   silently corrupts history; with batching, the writer outruns any realistic
   event rate, so blocking is a theoretical safety, not a hot path. Log a
   warning if a send blocks.
5. **Writer failure policy:** a failed batch is retried once; on repeated
   failure, log loudly and drop the batch. Memory stays authoritative —
   scripts keep working even if the disk is dying; only history has gaps.
6. **Seed** populates the map synchronously (scripts load after the first
   seed, so pre-seed reads don't exist) and keeps its existing synchronous
   transaction — startup is not latency-sensitive, and seed's
   dedup-vs-mirror logic compares against the map instead of a SELECT.

### 3.2 Semantic changes to accept (document in lua_api.md)

- `ha.get_history` immediately after an event may not yet contain that
  event's row (append is now async). Retention-window observability data;
  acceptable.
- Crash durability class is unchanged from the v3.0.1 decision: queued rows
  can be lost on a crash; the mirror re-seeds from HA.

### 3.3 The `states` table becomes write-only

After step 2, nothing reads SQLite `states` (verified: its only readers are
the tracker methods moving to memory). **Phase 1 keeps writing it** inside
the batch transaction — cheap, and it de-risks the switch. Dropping the table
(and its writes, and the schema migration) is a **phase 2 decision**, taken
only after the memory mirror has run in production for a while.

## 4. What does NOT change

- The 100 ms batch window default and `ha.immediate_events()` opt-in.
- Apply-before-dispatch ordering (now to memory) — the `get_state` freshness
  guarantee survives verbatim.
- KV stores stay synchronous on the shared write handle: `store.set` is a
  script's *own* write and read-your-writes matters there.
- Scheduler, purge, and the WS client are untouched (purge can no longer
  stall dispatch, but its own code doesn't change).
- Default `call_service` stays synchronous.

## 5. Instrumentation first

Before touching either mechanism, make the latency visible: stamp each
`Event` with its enqueue time and `slog.Debug` the queue-to-handler delay at
dispatch (plus `slog.Warn` above, say, 250 ms). This confirms §2's diagnosis
on the user's real network, catches regressions, and is trivially small. It
also tells us afterwards what the fixes actually bought.

## 6. Milestones

Each is a bisectable commit (or a few) that compiles and passes `make test`.

0. **M0 — measurement harness (done).** `internal/e2e`: fake HA WS server →
   real client → the verbatim main.go router loop → supervisor-run mirror
   script → `call_service` back to the fake server, on file-backed SQLite.
   Three benchmarks map one-to-one onto this spec's claims:
   `EventToServiceCall` (headline latency + p50/p99),
   `EventToServiceCallBusyKV` (§3 head-of-line blocking),
   `QuickToggle` (§2; `off-ns/op` is the user's half second). Baselines are
   committed in `benchmarks/baseline.txt`; every milestone below must show
   its effect in `make bench-compare`. Dev-machine baseline: event→command
   mean ~0.4 ms, p99 ~5 ms; KV noise lifts p99 to ~7–10 ms; `off-ns/op`
   tracks the simulated 100 ms device ack to the millisecond — both
   diagnoses confirmed.
1. **M1 — dispatch-delay instrumentation.** Event timestamps + debug/warn
   logging in the runner.
2. **M2 — `{ wait = false }` for `ha.call_service`** + async error routing to
   `on_exception` + `mirrored_switches` example uses it + `lua_api.md`.
3. **M3 — memory mirror.** Tracker map, apply-before-dispatch, reads from
   memory, seed reworked. SQLite writes still synchronous at this point.
4. **M4 — background writer.** Batched-transaction goroutine, shutdown
   drain, overflow/failure policy. Router no longer touches SQLite.
5. **M5 — docs + release.** lua_api.md/DOCS.md/README.md updates, working
   state, minor version bump.

## 7. Open questions (decide during build, none block M1–M2)

- Warn threshold for M1 (250 ms proposed).
- Channel capacity for M4 (1024 proposed; only matters under sustained
  writer stall).
- Whether `GetEntities` should return map iteration in sorted order for
  deterministic tests (probably yes, sort by entity_id).
