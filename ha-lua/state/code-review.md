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

STATUS: round 3 COMPLETE. All items fixed, nothing pending. Not yet released.
