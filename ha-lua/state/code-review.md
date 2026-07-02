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

STATUS: follow-up COMPLETE. Pending only: a release (v2.8.9) to ship card
0.3.25 + the eight review fixes — do not tag without the user asking.
