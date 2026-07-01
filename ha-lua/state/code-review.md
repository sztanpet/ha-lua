# Whole-codebase review (2026-07-01)

Full read of all non-test Go (~5.4k lines), the Lovelace card, and the embed/
config plumbing. Findings below, ordered by severity. Each fix is its own
commit; status is updated as they land.

## P1 — correctness bugs

1. **[DONE? pending] Seed never deletes ghost entities.**
   `state/tracker.go Seed()` only upserts. An entity removed from HA while the
   daemon was disconnected stays in the `states` mirror forever, so
   `ha.get_state` keeps reporting it. Seed already loads the whole mirror into
   `current`; delete every mirror row whose entity_id is not in the incoming
   batch (same tx). Removal *while connected* is already handled by the
   nil-new_state path in HandleStateChanged — this is only the disconnect gap.

2. **[pending] PruneScript deletes load-time ha.after rows.**
   `RegisterAfter` never adds its ID to `api.timerIDs`, and the runner calls
   `scheduler.PruneScript(ctx, scriptID, api.timerIDs)` after load — which
   DELETEs every row not in keep, including the after row just inserted. The
   in-heap timer still fires this run, but the documented persistence ("restart
   before fire → warn about the orphaned row") is silently lost. Fix: keep a
   separate keep-list that includes after IDs; the every/at seq counter must
   NOT change (IDs are stable keys carrying last_run/next_run).

3. **[pending] http.get/post have no timeout.**
   `stdlib_http.go doRequest` uses a bare `&http.Client{}`; the only bound is
   `L.Context()` = the script's *lifetime* context. The AI.state claim
   "callback context (5s) is the only timeout" is stale — no per-callback
   context exists anywhere. A wedged remote pins the script goroutine until
   the script is stopped. Fix: one package-level client with a 30s Timeout
   (still also cancellable via L.Context()). Update the stale AI.state
   decision line.

4. **[pending] smtp.SendMail can hang forever.**
   `exceptions.go` calls `smtp.SendMail` directly: no dial timeout, no I/O
   deadline, no context. A wedged SMTP server blocks the script goroutine in
   Go code — which the supervisor's 5s VM abort cannot interrupt, so
   StopScript blocks forever on `<-h.done` and hot reload of that script
   wedges. Fix: replace with a dial-with-timeout + conn deadline
   implementation (net.DialTimeout + smtp.NewClient), keep the
   `smtpSendMail` test seam signature.

## P2 — smaller fixes

5. **[pending] Invalid log_level silently ignored** — `main.go` discards
   `level.UnmarshalText`'s error; a typo'd level runs at Info with no hint.
   Warn after the logger is up.
6. **[pending] Timer-type parsing breaks on `|` in script IDs** —
   `runner.go handleTimerFired` takes `strings.Split(timerID, "|")[1]`, but the
   ID starts with the script ID which is a filename that may itself contain
   `|`. TrimPrefix the known scriptID+"|" first.
7. **[pending] store.state() loads with context.Background()** —
   `api_store.go newStateProxy` should use `L.Context()` like every other
   binding.
8. **[pending] Stray `L.Push(mod)` after RegisterModule** — stdlib_{http,
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

(filled in as work lands)
