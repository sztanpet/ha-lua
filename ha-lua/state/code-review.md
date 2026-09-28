# Whole-codebase review (2026-07-01)

Full read of all non-test Go (~5.4k lines), the Lovelace card, and the embed/
config plumbing. Findings below, ordered by severity. Each fix is its own
commit; status is updated as they land.

## P1 — correctness bugs

1. **[DONE f79500f] Seed never deletes ghost entities.**
   `state/tracker.go Seed()` only upserts. An entity removed from HA while the
   daemon was disconnected stays in the `states` mirror forever, so
   `ha.get_state` keeps reporting it. Seed already loads the whole mirror into
   `current`; delete every mirror row whose entity_id is not in the incoming
   batch (same tx). Removal *while connected* is already handled by the
   nil-new_state path in HandleStateChanged — this is only the disconnect gap.

2. **[DONE f71f78e] PruneScript deletes load-time ha.after rows.**
   `RegisterAfter` never adds its ID to `api.timerIDs`, and the runner calls
   `scheduler.PruneScript(ctx, scriptID, api.timerIDs)` after load — which
   DELETEs every row not in keep, including the after row just inserted. The
   in-heap timer still fires this run, but the documented persistence ("restart
   before fire → warn about the orphaned row") is silently lost. Fix: keep a
   separate keep-list that includes after IDs; the every/at seq counter must
   NOT change (IDs are stable keys carrying last_run/next_run).

3. **[DONE 528a33f] http.get/post have no timeout.**
   `stdlib_http.go doRequest` uses a bare `&http.Client{}`; the only bound is
   `L.Context()` = the script's *lifetime* context. The AI.state claim
   "callback context (5s) is the only timeout" is stale — no per-callback
   context exists anywhere. A wedged remote pins the script goroutine until
   the script is stopped. Fix: one package-level client with a 30s Timeout
   (still also cancellable via L.Context()). Update the stale AI.state
   decision line.

4. **[DONE 46e8135] smtp.SendMail can hang forever.**
   `exceptions.go` calls `smtp.SendMail` directly: no dial timeout, no I/O
   deadline, no context. A wedged SMTP server blocks the script goroutine in
   Go code — which the supervisor's 5s VM abort cannot interrupt, so
   StopScript blocks forever on `<-h.done` and hot reload of that script
   wedges. Fix: replace with a dial-with-timeout + conn deadline
   implementation (net.DialTimeout + smtp.NewClient), keep the
   `smtpSendMail` test seam signature.

## P2 — smaller fixes

5. **[DONE 222a73f] Invalid log_level silently ignored** — `main.go` discards
   `level.UnmarshalText`'s error; a typo'd level runs at Info with no hint.
   Warn after the logger is up.
6. **[DONE abe9e7f] Timer-type parsing breaks on `|` in script IDs** —
   `runner.go handleTimerFired` takes `strings.Split(timerID, "|")[1]`, but the
   ID starts with the script ID which is a filename that may itself contain
   `|`. TrimPrefix the known scriptID+"|" first.
7. **[DONE ee403d8] store.state() loads with context.Background()** —
   `api_store.go newStateProxy` should use `L.Context()` like every other
   binding.
8. **[DONE c00e999] Stray `L.Push(mod)` after RegisterModule** — stdlib_{http,
   crypto,re,strings,json,fs}.go push the module table and never pop; the
   values sit on the LState stack for the process lifetime. Drop the pushes.

## Reviewed and deliberately NOT changed

- `client.go` reconnect/backoff, pending-command drain, seed-batch channel:
  sound; the mid-getStates `deliverResult` routing covers the startup race.
- Registry/Supervisor stop ordering (Remove-before-Close, RLock dispatch):
  correct; router reqCh never closed by design.
- `logwriter.Rotating` best-effort rotation semantics: acceptable for a log.
- Card JS (0.3.24): module-level `sentConfigures` keyed by (entity,hash) and
  microtask-deferred preview check match the documented configure-storm fix;
  no new issues found.
- `luaTableToAny` array detection, re LRU cache (256, O(n) but tiny),
  GetHistory string-prefix `since` bound: all fine.
- Slow-loris on the LAN UI port (ReadHeaderTimeout only): LAN-only surface,
  router bounds handler wait at 5s; not worth hardening now.
- `GetHistory` negative limit = unlimited (SQLite): scripts are trusted.

## Commits

All landed 2026-07-01, each green on make check:
- f79500f state: drop mirror entities missing from a seed
- f71f78e lua: keep load-time ha.after rows across PruneScript
- 528a33f lua: give http.get/http.post a 30s timeout
- 46e8135 lua: bound the SMTP exchange with a deadline
- 222a73f cmd: warn when log_level is unparseable
- abe9e7f lua: parse timer type after the script-ID prefix
- ee403d8 lua: load store.state() under the script context
- c00e999 lua: drop stray module pushes in RegisterStdlib

Notes for future sessions:
- Seed now has FULL-SNAPSHOT semantics: entities absent from the batch are
  deleted from the mirror (empty batch exempt). Tests must upsert single
  entities via HandleStateChanged, never via repeated one-entity Seeds.
- haAPI.keepIDs (all load-time timer IDs, incl. after) is what PruneScript
  keeps; haAPI.timerSeq numbers every/at only — do not merge them back.
- The AI.state "http timeout" decision was stale (claimed a 5s callback
  context that never existed); corrected in 528a33f.

STATUS: review COMPLETE, all items fixed. Nothing pending.

# Follow-up: card review + simplification pass (2026-07-01/02)

## Enhanced-climate card — verdict: NO rewrite

The card (0.3.24 -> 0.3.25) is structurally sound: vanilla element by
documented decision, configure-storm guards hard-won across v2.8.2–2.8.8,
i18n/pure-helper split clean and browser-tested. A rewrite (e.g. Lit) would
re-open every lifecycle bug for zero user-visible gain. One real flaw fixed:

- **[DONE 9bcf713] Full DOM rebuild on every hass push.** HA pushes hass for
  every state change of ANY entity; the card tore down and rebuilt its shadow
  DOM each time. Now `set hass` reference-compares the climate entity, its
  companion, and the language (HA replaces state objects immutably) and only
  then schedules a render. Marker-based harness test proves skip + rebuild.
  Card VERSION bumped to 0.3.25 — needs a release to reach users (patch;
  next would be v2.8.9).

## Simplification pass — done

- **[DONE 4ef708c]** api_store.go: store/global Lua bindings were copy-paste;
  folded into one kvTable(name, kv) over a kvStore interface (-36 lines,
  error strings unchanged).
- **[DONE 7ea46a6]** api_ha.go: stateToLua/eventToLua shared a 9-line
  "unmarshal or empty table" dance; hoisted to luaUnmarshalOrEmpty in json.go.
- **[DONE e796662]** enhanced_climate.lua: publish-heartbeat comment claimed
  the mirror is never pruned on reconnect — stale since f79500f. Comment
  fixed; heartbeat behavior deliberately kept.

## Simplification candidates considered and REJECTED (don't redo the analysis)

- `cards/embed.go` + `examples/embed.go` Materialize duplication (~20 lines):
  a shared internal package for one trivial WalkDir loop costs more than the
  copy; they already differ (README, error strings).
- `store/kv.go` Store vs GlobalStore Go-side duplication: queries differ
  (2-col vs 1-col PK); a generic would trade clear SQL for indirection.
- `registerHaAPI` (330 lines): a flat, linear registration table; splitting
  adds names, not clarity.
- `web.Start` vs `debug.Start`: near-identical 35-line server starters;
  different concerns, leave.
- `luaTableToAny` two-pass array detection, re LRU cache: fine as-is.
- examples/lib/{card,control,schedule,zones}.lua: read in full, clean, pure,
  nothing to change.

STATUS: follow-up COMPLETE. Released as v2.8.9 on 2026-07-02 (tag on release
commit 27761c8; ships card 0.3.25 + the eight review fixes). Track CLOSED.

---

# Round 2 — whole-codebase review (2026-07-25)

Second full read of all non-test Go, prompted by "check the code for bugs".
Five real bugs, all fixed and released as v3.2.2. Findings 1, 2 and 4 were
confirmed empirically (throwaway probes) before being written up — worth doing
again next round, it separated the real from the theoretical fast.

## Fixed

1. **[DONE 86e030a] Cyclic Lua table crashed the whole daemon.**
   `json.go luaToAny`/`luaTableToAny` recursed with no bound, so `t.self = t`
   ran the goroutine stack out. A Go stack overflow is `fatal error`, NOT a
   panic: `pcall` can't catch it, `dispatchException` never runs, the process
   dies and takes every other script's LState with it. Confirmed with a probe
   test (`fatal error: stack overflow` at json.go:56). Fixed with a depth cap
   (`maxTableDepth = 100`) threaded through `luaToAnyDepth`. Chose a depth cap
   over a visited-set: one int, no allocation, also catches absurd non-cyclic
   nesting. Blast radius was every table-encoding binding — `store.set`,
   `global.set`, `store.state` assignment, `ha.call_service`, `ha.fire_event`,
   `ha.set_state` — not just `json.encode`.

2. **[DONE cef27ad] StopScript could unregister a *newer* runner.**
   The handle left `s.scripts` under `s.mu`, but `reg.Remove` +
   `Scheduler.RemoveScript` ran after unlocking. A `StartScript` in that gap
   installed a fresh runner into both maps; the stale removals then
   unregistered it. End state: live script, tracked by the supervisor (so every
   later Reload is a no-op) but absent from the Registry — no events, no
   timers, 404 from the router. Reachable because the watcher deletes a
   script's debounce entry *before* running the reload, so a second save while
   the first Reload is still inside StopScript (≤ stopTimeout = 5s) issues two
   overlapping stops. Fix: both removals moved inside the critical section,
   mirroring StartScript's add. Lock order sup.mu → reg.mu / sched.mu already
   existed in StartScript, so no new deadlock risk; only the drain wait stays
   outside.
   **Test seam:** the natural window is a few instructions wide and 300 rounds
   of concurrent Reload did NOT reproduce it, so the regression test parks a
   stop in the old gap via `stopScriptHook` (a no-op prod var, same pattern as
   `smtpSendMail`). Verified it fails on the old ordering.

3. **[DONE 2b26c95] `keepIDs` leaked one ID per callback `ha.after`.**
   `PruneScript` is keepIDs' only reader and runs once after the main chunk,
   but the `ha.after` binding appended unconditionally — and ha.after is
   explicitly callable from callbacks. A debounce firing a few times a second
   retained a random ID per call for the script's lifetime. Fix: `haAPI.loaded`
   flag + `keepTimer()` helper; runner nils the slice after pruning. Same class
   as the timerFns leak fixed in M-whatever; keepIDs was missed because it only
   started carrying after-IDs in f71f78e (round 1, finding 2).

4. **[DONE d8a041d] `tostring(time.now())` printed a userdata address.**
   `__tostring` sat in `timeMethods`, which is installed as the metatable's
   `__index` — Lua looks metamethods up on the metatable itself, so it was only
   reachable as an explicit `t:__tostring()`. Confirmed with a probe
   (`userdata: 0x1c1ae8981bc0`). Moved onto the metatable.

5. **[DONE 886df72] Data race on `time.Local`.**
   Assigned in main.go *after* `debug.Start` and `tracker.Start` had spawned
   goroutines that log, and every slog record reads time.Local via time.Now().
   Invisible to `go test -race` because main has no tests. Moved to just after
   logger setup, while the process is still single-threaded (ResolveLocation
   warns through slog, so it can't move earlier than that).

## Checked and NOT bugs (don't re-derive)

- `ha/client.go` subscribe/reconnect bookkeeping: `conn` and `subscribed` are
  always swapped inside one critical section, so a mark can never land on a
  connection it wasn't taken from. No double- or missed subscription.
- `purge.go` cutoff is `time.RFC3339` (`…Z`) while `changed_at` is HA's
  `last_changed` verbatim (`…+00:00`, often fractional). Lexical comparison
  resolves at the date field, so the mismatch can only differ *within the same
  second*. Left alone.
- In-place slice filtering in `Router.Unregister` and
  `Scheduler.RemoveScript`: write index never runs ahead of read index.
- `Runner.Close()` vs `Registry.Dispatch`: Remove blocks on reg.mu until
  in-flight dispatches finish, and the registry is keyed by script ID, so no
  send-on-closed-channel path exists.
- `cachedEventHandlers` read from the OnLoaded goroutine while the script
  goroutine appends to `api.eventHandlers`: append writes at index len, which
  the reader's slice never covers.
- Event coalescing keeps the newest state_changed at the entity's *first*
  arrival position, so it can reorder against a custom event that arrived
  later. Documented behavior ("other events are kept in order"),
  `ha.immediate_events()` is the escape hatch. Left.

---

# Round 3 — whole-codebase review (2026-09-24)

Third full read of all non-test Go (~8.2k lines), prompted by "review the go
code for bugs, architectural and code maintenance issues, and simplifications".
Two real defects, one latent trap, five simplifications, one comment pass.
Fourteen commits, `bd3c185`..`4de4492`, each green on `make check`.

## Fixed — defects

1. **[DONE bd3c185] A script's load-time shape was readable mid-load.**
   `Runner.Stats/UITitle/Routes/MQTTFilters` read `cachedUITitle`,
   `cachedRoutes`, `cachedStateHandlers`, `cachedMQTTHandlers` — fields the
   script goroutine assigns at the end of `Start`, just before
   `close(LoadedCh)`. Every caller reaches them through the Registry, which
   lists a runner from `StartScript` onward, so the debug page's 2s poll and
   `DispatchMQTT`'s filter walk could both read a field while it was being
   written. Round 2 cleared a *different* pair (append-at-len vs a reader's
   older slice header); this is the field assignment itself.
   `wantsHAEvent` already had the guard inline — it became `loaded()` and the
   rest of the accessors use it, reporting nothing until the load finishes.
   Deterministic test: a hand-built Runner with an open LoadedCh.

2. **[DONE 5b441d5] http.get/post read an unbounded body.**
   `fs.read` has capped at 8 MiB since the fs module landed; the HTTP module
   read whatever a remote sent into one Lua string. Now `io.LimitReader` to
   `maxResponseBytes+1` and an error past the cap — one byte over, so an
   oversized body is an error rather than a truncated document the script would
   parse as the answer.

3. **[DONE 58a63a5] Latent: `historyPoints` parked a per-row error in its named
   return.** `p.at, err = time.Parse(...)` then `continue`; correct today only
   because the final return names `rows.Err()`. Now a local.

## Simplifications

- **[DONE 92a334d]** `logwriter.Rotating.openErr` had three writers and no
  reader. Removing it also removed the second (O_TRUNC) open path and its
  fallback dance: rotate closes + renames and leaves `file` nil, the next Write
  reopens and reports the error. A failed rename truncates instead, which is
  what keeps the byte budget honest.
- **[DONE 2e78b57]** `store.Store`/`GlobalStore`: one embedded `table` holding
  the four statements and the bind prefix. See the reversal note below.
- **[DONE f829133]** `re.*`: five copies of read-pattern/take-cache/compile/
  raise became one `compiled()` helper; `re.find` matched the subject twice to
  tell an empty match from no match, now `FindStringIndex`.
- **[DONE 2332dcf]** store.state's cache wrapped in a struct with an unread
  field; a hand-rolled `trimSpace`; an explicit `RawSetString("event", LNil)`;
  `min`/`max`/`new` shadowed as locals.
- **[DONE b62dfaf]** `registerHaAPI` split by area. See the reversal note.
- **[DONE 1f2ec53]** `main.go`: `serviceCall()` for the frame both CallService
  and CallServiceAsync marshalled, `materialize()` for the examples/cards
  mkdir-then-write, MQTT closures replaced by method values; `ha.Client` had
  `NextID` calling `nextID`.
- **[DONE 114cabe]** mqtt's retry and shutdown goroutines now run under
  `pprof.Do` like every other goroutine in the daemon.
- **[DONE 707490b]** `ha.readLoop`'s event send had both a `ctx.Done` arm and a
  `default`, so the former only ever won a coin toss; cancellation is observed
  by the next `conn.Read`.
- **[DONE 4de4492]** `haAPI.loaded` → `pruned`: it only ever meant "PruneScript
  has consumed keepIDs", and `Runner.loaded()` now answers something else.
- **[DONE be1c9b0]** Comment pass: removed the copies of the line below them
  ("Persist timer functions", "Deliver initial states", "Object", RegisterStdlib's
  numbered steps) and the bug stories (StopScript's race, the require sandbox,
  the states table, the timezone assignment, a load error's log-only past). One
  comment still promised sandboxing "in milestone 10".

## Two round-1 rejections reversed (deliberately, don't flip back)

Round 1 rejected both after analysis. What changed:

- **store Store/GlobalStore.** The objection was that a generic would trade
  clear SQL for indirection. The shared `table` keeps every statement written
  out verbatim at its constructor and shares only the execution, so the SQL is
  as readable as before and the marshal/ErrNoRows/scan logic exists once.
- **registerHaAPI.** The objection was that it is a flat linear registration
  table. It was 330 lines then and 460 now, with the wait=false path nested
  four deep inside it; at that size "flat" stops being a virtue. Grouped by
  area (logging, state reads, history reads, commands, timers, handlers,
  serve), which also gives the async service call a name of its own.

## Checked and NOT changed (don't re-derive)

- `msgID` is `atomic.Int32` on purpose: `int` is 32-bit on the armv7 HA boxes,
  so widening it would truncate at the `int()` conversion. Commands (not
  events) consume ids, so the wrap is years away.
- `scheduler.fireDue` holds the heap lock across its DB writes and `onFire`:
  onFire is a non-blocking channel send and the write handle serialises anyway.
- Lock order stays `Supervisor.mu → {Registry.mu, Scheduler.mu}` and
  `Scheduler.mu → Registry.mu`; Registry.mu leads nowhere, so there is no cycle.
- `purge.exec` returning `(0, nil)` on a RowsAffected failure: the DELETE
  succeeded, the count only feeds a log line (now commented, `4f82bb1`).
- `web.Start` vs `debug.Start` duplication, `cards`/`examples` Materialize
  duplication: rejected in round 1, still the right call.
- `err != http.ErrServerClosed`: ListenAndServe returns that exact value,
  unwrapped; `errors.Is` would be churn.
- `logbuf` re-parsing each record's level string per snapshot: 500 records on a
  human-driven poll.
- `stdlib_fs.go`'s per-function `root == nil` check: stubbing the module out at
  registration would have to keep `fs.exists` returning false, not `(nil, err)`.

STATUS: round 3 COMPLETE. All items fixed, nothing pending. Released as v4.9.1
on 2026-09-24 (tag on release commit `0bae40c`). Patch, not minor: two fixes and
a pile of internal refactoring, no new Lua API and no new feature.

---

# Round 4 — whole-codebase review (2026-09-25)

Prompted by "review all code to see if it's fit for purpose" — so wider than
rounds 1–3: all non-test Go, the Lua examples and `lib/`, the Lovelace card, the
web assets, and the config/embed plumbing. Verdict: fit for purpose. One real
defect, two latent traps, one documentation gap. Three commits, `0a46fa4`..
`f99c705`, each green on `make check`.

## Fixed

1. **[DONE 0a46fa4] `Seed` rolled the memory mirror backwards.**
   A `get_states` batch is a snapshot of the instant HA rendered it, and on a
   busy install it is megabytes. `Seed` built a new map from the batch and
   swapped it in unconditionally, so any entity that changed after the snapshot
   was rendered — or whose `state_changed` landed between the old read-then-swap
   gap — reverted. The mirror is authoritative for `ha.get_state` and
   `ha.get_entities`, so a script read the superseded state until the entity
   next changed: a door that closed during a reconnect stayed "open" until
   somebody opened it again. `client.go` pushes `States` *before* it subscribes
   and the seed goroutine is separate from the event router, so the two really
   do run concurrently.
   Fix: an entity whose mirror entry carries a later `LastUpdated` than the
   batch keeps it, and the diff/merge/swap all run under one write lock.
   Unparseable or absent stamps cannot order the two, so the batch wins there
   (what the mirror did before). The cold-start baseline query moved into
   `historyBaseline` and now runs only when the mirror is actually empty; a warm
   re-seed compares against the live entry it already looked up. History appends
   moved after the swap so a full write queue cannot block `ha.get_state`.
   Confirmed with a probe before the fix, and the regression test was verified
   to fail on the old code.

2. **[DONE 55743c2] `ha.every`/`ha.at` from a callback leaked.**
   Their ids carry a registration-order sequence, which is what keeps
   `last_run`/`next_run` stable across a reload — so a call from a callback
   allocated a *fresh* id every time instead of re-arming: a heap entry, a
   `timers` row and a `timerFns` entry per call, retained for the life of the
   script and swept only by the NEXT reload's `PruneScript`. Both now raise once
   `api.pruned` is set, pointing at `ha.after`. Same class as round 2's keepIDs
   leak, and the same flag already marked the boundary. Documented in
   `lua_api.md`.

3. **[DONE f99c705] Unreachable mqtt nil branches.** `main.go` wires
   `MQTTSubscribe`/`MQTTPublish` from method values on a never-nil client, so
   the bindings' `== nil` "no broker configured" arms could not run: the real
   condition arrives as `mqtt.ErrDisabled` from the client. Two paths, two
   strings, one condition. `NewRunner` now installs stubs returning
   `ErrDisabled`, so an unwired runner and a real client with no broker answer
   identically.

## Raised and REJECTED by the user (2026-09-25 — do not re-raise)

- **`scripts/lib/*.lua` is not watched.** `NewScriptWatcher` does a
  non-recursive `w.Add(dir)`, so editing a shared module reloads nothing,
  silently — while `DOCS.md` tells users to put helpers in `scripts/lib/` and
  says saved changes reload automatically. Restart to pick up a lib edit.
  WON'T FIX — asked and declined. Do not add a recursive watch, and do not
  "fix" the DOCS wording into a caveat either; it was read and left as is.
- **`/debug/` is mounted on the unauthenticated LAN port** as well as ingress,
  so `api/logs` (and `api/goroutines`) are reachable by anyone on the network.
  `config.yaml`'s `ports_description` warns the port is unauthenticated, and
  `service_api.lua` deliberately logs its token at first load. WON'T FIX —
  asked and declined ("debug is fine"). The LAN port is LAN-trust by design;
  do not move `/debug/` behind ingress or gate it.
- `RouteSpec` marshals as `Method`/`Prefix` while the rest of the JSON is
  snake_case; `config.go` is the one `encoding/json` v1 holdout. Cosmetic.

## Checked and NOT changed (don't re-derive)

- **`stdlib.go`'s os-restriction loop mutates the table it is iterating.**
  Safe: `LTable.ForEach` ranges a Go map, and only the key already produced is
  deleted. Not a hole in the sandbox.
- The `require` sandbox, `purge`'s first-match-wins rule chaining, MQTT filter
  matching and `$SYS` exclusion, the scheduler's DST handling and its
  fire-once catch-up.
- The Lovelace card (0.3.32): the module-level `sentConfigures` keyed by
  (entity, hash), the `_relevantChanged` reference-compare render gate, and the
  `entriesFromSchedule`/`scheduleFromEntries` round trip all hold up.
- Every `innerHTML` site in the example pages and the web assets is static
  markup; data goes through `.value`/`textContent`, and `thermostat.html`'s
  `html:` attribute carries only constant SVG.
- `examples/lib/*`: clean. `reminders.tick` deleting from `pending` during
  `pairs` is explicitly allowed in Lua.
- `cmd/ha-lua` at 0% coverage: accepted (it is why round 2's `time.Local` race
  was invisible), every other package sits between 70% and 92%.

STATUS: round 4 COMPLETE. Three items fixed; the lib watcher and the LAN debug
surface were raised and deliberately left. Released as v4.10.0 on 2026-09-25
(tag on release commit `a17b0e9`). MINOR, not patch: `ha.every`/`ha.at` now
raise from a callback, which is a visible Lua API behaviour change — a script
that did it kept working before, badly. Not major: the only scripts affected
were leaking a timer per call.

# Round 5 — enhanced climate + overshoot review (2026-09-28)

Prompted by "review the enhanced-climate card and its functionality with
special attention for the overshoot protection, is the code fit for purpose".
Scope: `cards/enhanced-climate-card.js`, `examples/enhanced_climate.lua` and
`.html`, `lib/{overshoot,control,climate,schedule,card}.lua`, both specs,
checked against the live box. Verdict: the card and the observe-only learner are
fit; the armed correction and the controller under it are not.

STATUS: PLANNED on 2026-09-28, nothing executed. The user asked for the plan
"for later execution".

How it was checked, so the next round does not redo it:
- Box scripts byte-identical to HEAD `9a98e51`, card 0.3.40 materialized.
  `go test -race ./...` green; the chromedp card tests run, not skip.
- Recorder history Sep 22–28 for the three radiator zones replayed through the
  real `lib/overshoot.lua` from a scratch gopher-lua harness that steps it the
  way `overshoot_step` does (1-min tick, relay close, window change). Only the
  children's room produces runs; the other two sit above their setpoints
  outside boosts. The approach is in the `childrens-room-instrumentation`
  memory.
- Every finding below was reproduced by a scratch test against the real
  `enhanced_climate.lua` in a copy of the repo. Those tests were ephemeral; the
  plan describes each scenario, and each regression test must fail on the
  current code before its fix goes in.

## Findings

1. **The overshoot hold outlives a room that has turned below the request —
   rule 1.** `lib/overshoot.lua:203` releases only when `room + c·lead <
   request`, but c is calibrated at the cut: once the room has peaked there is
   no rise left, yet `c·lead` still adds one. Scratch: cut at 23.3 (lead 9.4,
   c 0.02), peak 23.4, back to 23.3 with the radiator at 36 → still holding
   (predicted 23.55). The heat stays off below the setpoint until the radiator
   is within 0.1/c of the room, the room reads 0.2 under its peak, or 90 min
   pass. The better a cut lands, the more often this bites.
2. **c = 0 cuts at the setpoint on the 0/0 node.** Since the 2026-09-28 flash
   the node heats while room ≤ setpoint, so a run often starts with the reading
   exactly at the request, where `predicted >= requested - EPSILON`
   (`lib/overshoot.lua:176`) already holds: the first tick after the radiator
   gains RAD_RISE writes the hold (scratch: c 0, room = request, radiator +1.1 →
   22.9 written). The node itself stops only at room > request, so spec §5's
   "c = 0 never cuts before the node" is false.
3. **The room sensor is too coarse for the learner.** `sensor.temp8_temperature`
   (HOBEIAN ZG-227Z via Z2M) reports only when it moves ≥0.2 °C; a 0.1 change
   waits for its ~55-min heartbeat (178 of 231 reports this week were 0.2
   steps, median gap 33 min). A coast peak under 0.2 is invisible: the replay's
   one c_obs of 0.00 (09-26 10:10) is a cut at 23.6 followed by 55 min of
   silence. The ESP thresholds on the same signal.
4. **One-step dial changes are swallowed.** `lib/control.lua:26` counts
   `|target − written| <= 0.1` as our own write while the device step is 0.1,
   so float rounding decides: |23.3 − 23.4| = 0.0999… → "ours" → the next tick
   writes 23.4 back. 64 of the 150 setpoints 15.0–29.9 swallow a +0.1 tap, 65 a
   −0.1 one, 23.4 → 23.3 among them. Seen live 2026-09-27 06:23:42: the card
   took the device 23.6 → 23.7, the request stayed 23.6, the tick wrote 23.6
   back at 06:24:19.
5. **Frost, or an overshoot hold, is latched as a 24 h dial hold after heating
   goes off and on.** `enhanced_climate.lua:525` stores the request as
   `written` in every mode, though nothing is written outside heat. Window open
   (15 on the device) → mode off → window closed while off → mode heat: 15 ≠
   `written` → manual hold at 15. The 10:00 switch-off during morning airing is
   this sequence; an armed hold on the device at 10:00 goes the same way.
6. **A boost puts our own frost or hold back.** The override handler deletes
   the manual hold (`:766`) and snapshots the device setpoint as the way back
   (`:759`). With frost or a hold on the device at that moment, the boost's end
   — nothing left under it — writes 15° or 22.8° back.
7. **A schedule-less climate drops out of control 24 h after a dial change**
   (`:584`). The children's room has no schedule; its hold from 2026-09-27
   18:11 expires 2026-09-28 18:11. From then `desired()` is nil: no control, no
   window pause, no episodes, and a frost or hold on the device at that moment
   stays for good (scratch: both reproduced). After every boost on it too — the
   companion history shows `30 → off` until the user re-nudged.
8. Minor: `enhanced_climate.html:235` prints "undefined window sensor(s)" when
   the list arrives as `{}` (fürdő). "Reset learning" wipes c and the whole
   journal on one tap, card and page. The card says "overshoot idle" on a
   climate with no radiator sensor, where the correction can never act.

## Fix plan

Decided with the user 2026-09-28: a dial change on a schedule-less climate
holds until replaced (finding 7). Not a schedule entry, which would revert every
dial change at midnight.

One commit per step, each green on `make test`; `make check` before the round
is declared done. Mark steps `[DONE <hash>]` as they land, like rounds 1–4. The
controller goes first, because those bugs bite while the correction is still
observe-only.

- **A1** `climate: take a one-step dial change as manual`. In
  `lib/control.lua` `is_manual`, `<= 0.1` becomes `< 0.075`: a write the
  device rounded lands at most 0.05 off, a dial change at least 0.1, and 0.075
  clears float error both ways. The existing assertions still hold (21.05 vs 21
  is ours, 21.2 is manual). Add the trap pairs to `TestControlPureLib`
  (23.3/23.4, 22.8/22.9, 23.7/23.6, 23.5/23.4 all manual), plus an
  enhanced-climate test: schedule 23.4, device → 23.3 gives a manual hold at
  23.3 and no 23.4 written back. `thermostat.lua` shares the helper, so run its
  tests. Amend enhanced-climate-spec §7 item 2 (">0.1").
- **A2** `climate: record only the setpoints actually written`. In
  `apply_climate`, set `written` inside the heat branch only; outside heat, seed
  it from the device's setpoint when it is unset (first configure while off),
  so the first heat event is not a dial change. The handler also re-applies at
  once when a climate enters heat (`old_state.state ~= "heat"`), not up to a
  tick later. The fixture's `pushClimate` hard-codes heat on both sides, so it
  needs a mode-aware helper.
  - Test: frost 15 → off → window closed → heat gives no manual hold and the
    request is written.
  - Test: a setpoint changed while off IS a manual hold after heat returns, as
    the user did on 2026-09-27 16:11. This guards against over-fixing.
  - `thermostat.lua:291` has the same pattern. It is not live; fix it in its
    own commit or leave it with a note.
- **A3** `climate: keep the dial hold under a boost`. Drop
  `store.delete(manual_key(climate))` from the override handler: the boost
  already outranks the hold in `control.desired`, and when it ends the hold
  takes back over. That fixes finding 6 for every controlled climate. The
  device snapshot is then only used on an uncontrolled climate, where its value
  is the user's own.
  - Test: hold 21, window open (frost 15), boost, window closed, boost ends →
    21 written, not 15. Existing restore tests use uncontrolled climates and
    must stay green.
  - Behaviour change to note in the CHANGELOG: after a boost, a scheduled
    climate returns to a still-valid dial hold rather than the schedule.
  - With the hold alive, the card shows the held badge during a boost. Check
    that it reads sensibly beside the countdown.
- **A4** `climate: hold a dial change until replaced without a schedule`.
  - A climate has no schedule when `schedule.resolve` yields no temperature
    (every day empty).
  - `manual_change` stores such a hold without `expires`, and `active_manual`
    treats it as live. A stored `expires` is ignored while there is no
    schedule, which also covers holds written before this change.
  - The `schedule` command bounds any existing hold to the new schedule's next
    transition, or unbounds it when the new schedule is empty.
  - `remove_climate` also drops `manual:`, so an unbounded hold cannot come
    back with a re-added climate.
  - The card needs no change: without `until` there is no held badge.
  - Tests: an unbounded hold is created; an expired `expires` still controls a
    schedule-less climate; saving a schedule bounds the hold; remove drops it.
  - Amend spec §7 item 2 and §9, and check DOCS.md for "until the next
    transition".
  - Add a Key decision to AI.state with the why: without it, a schedule-less
    room loses control 24 h after each dial change, and the window pause and
    the learner go with it.
- **B1** `overshoot: release the hold once the room turns below the request`.
  - In the coast branch, also release when `room < requested - EPSILON and room
    < peak - EPSILON`; the peak restarted at the cut, so this is a visible
    fall.
  - Record `released_by` ("predicted" / "turned") in the episode and
    `record()`, and log which one.
  - A room sitting flat at its cut reading keeps the hold, because the stored
    heat has not landed yet.
  - Pure-lib tests: turned-below releases with `released_by` "turned";
    flat-at-cut keeps holding.
  - Controller test: the finding-1 scenario writes the request back when the
    room turns.
  - Existing tests are unaffected: `run` and `early` still pass, and the
    Cuts-on-evidence test releases on "predicted".
  - Amend spec §5 (release rule), §9.1 (`released_by`) and §9.3 (log line).
  - A turned release also leaves the learner the full peak rather than a
    truncated one.
- **B2** `overshoot: cut only when the prediction overshoots`.
  - Line 176 becomes `predicted > episode.requested + EPSILON`, and the
    observe-only would-cut uses the same line. The hold keeps releasing on `<`,
    so a predicted exact landing stays held.
  - Fix the `EPSILON` comment and `learned_c`'s comment in
    `enhanced_climate.lua`.
  - Replace the pure-lib "c=0 cuts at the request" case: at room = request, no
    cut; past the request with the relay still reported on, cut.
  - Controller test: c 0, relay closes at the request, radiator warms → no
    write.
  - Amend the spec §5 c = 0 paragraph.
- **C1** `climate: count window sensors from an empty list` —
  `enhanced_climate.html:235` guarded with `Array.isArray`. There is no page
  test, so model one on `thermostat_ui_test.go` if it is cheap, otherwise check
  it in headless Chromium.
- **C2** `card: ask before resetting the overshoot learning`.
  - Use `window.confirm` (the card already uses `window.prompt`), with
    translated en/hu text. Do the same on the Ingress page's reset.
  - Bump VERSION to 0.3.41.
  - Test with `window.confirm` stubbed false → no command, true → the reset
    command. No card test clicks reset today.
- **C3** (optional) `card: say when there is no radiator to act on` — a
  status label in place of "overshoot idle" when `radiator_entity` is unset.
  Bump VERSION again.
- **D1** (box, with the user's go-ahead at execution time): in the Z2M
  frontend, temp8 → Reporting → msTemperatureMeasurement, set min 10 s, max
  300 s, change 10 (0.1 °C). It costs battery. A day later, confirm in the
  recorder that 0.1 steps arrive within minutes. If the firmware ignores it,
  the options are the pvxx ZigbeeTLc firmware (Z2M model `ZG-227Z-z`, which
  has a measurement interval) or another sensor.
- **E** Release v4.15.0 when the user asks. It is MINOR: dial-hold semantics
  change for schedule-less climates and under boosts.
  - Deploy: new add-on image (card), re-copy `enhanced_climate.lua`, `.html`,
    `lib/control.lua` and `lib/overshoot.lua` into `/config/ha-lua/scripts/`,
    then restart, because `lib/` is not watched.
  - Then set the children's room temperature once from the card; its last hold
    expires 2026-09-28 18:11, and until the next dial change nothing controls it.
  - Watch observe-only runs. The replay puts c near 0.02.
  - Arming stays the user's call.

## Expect once armed (from the replay — not bugs)

- Runs that start 0.1–0.2 below the request get cut 1–3 ticks after the
  radiator starts rising. The replay puts the would-cut at 7–11 min into runs
  the node ran for 12–22 min, with the radiator 7–14 °C above the room
  instead of 21–26.
- A run that starts with the reading exactly at the request is cut at the
  first warming tick: a ~5–6 min relay pulse, usually with under 3 °C of lead,
  so it is journaled `radiator_cold` with a warn. That is expected, not a
  learner failure.
- c measured at armed cuts will sit above observe-only's 0.02: the actuator
  keeps heating ~3 min after the relay opens, whatever the lead. It climbs until
  cuts land under MIN_LEAD, where discards stop it. That is bounded, not a
  runaway.
- Until D1, peaks move in 0.2 steps, and a landed cut often reads as no rise.
- In the journal, watch `released_by`, the lowest reading before the next run
  (no more than a sensor step under the request) and relay cycles per hour.

## Deferred (with the reason)

- **Put the request back when control lapses with our frost or hold on the
  device.** After A3 and A4 the only paths left are a schedule cleared, or a
  climate removed, while a window is open or a hold is in force. Do it if it
  ever shows up.
- **Stale echo guard.** A late state still carrying a setpoint we just replaced
  would read as a dial change. The ESP published once on off→heat in the
  recorder, so this has not been seen.
- **Tuning, only if armed data asks:** cut on radiator updates instead of the
  minute tick (the radiator climbs 3–4 °C/min there); a lower GAIN (per-run c
  swings 0.00–0.04); requiring MIN_LEAD before an armed cut, if pulses and
  discards dominate.

## Checked and NOT changed (don't re-derive)

- Card: the configure fire-once Set keyed `entity|hash`, the preview guard, and
  the stepper echo plus debounce. Finding 4 is the daemon's tolerance, not the
  card.
- Observe-only learning converges on real runs: c 0.0205 from 8 runs, per-run
  0.00–0.04, matching the first live 0.018. The v4.13 open-loop drift is gone.
- MIN_LEAD discards remove the high-c samples of early cuts, so they bias c
  down and bound the armed learner. They are a brake, not a bug.
- Load-time abandonment, first-reason-wins, journal ring, reset and observe
  paths, and invalidation on window, mode and setpoint changes.
- The radiator sensor updates every 60 s at 1/64 °C, which is fine. The IKEA
  window contacts bounce off/on within 1 s, costing one extra write pair: the
  correct reaction to what the sensor said.
