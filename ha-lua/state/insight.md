# State: history questions, attribution, durable reminders

Working state for the group of features that answer "what happened and who did
it" from the history we already keep, plus the reminders library built on the
store. No spec file — the work came out of a review of what HA users most often
ask for that HA cannot do. Global decisions live in `../AI.state`.

Status: **COMPLETE**, shipped in v4.5.0 (2026-08-09).

## Change context + attribution (2026-08-09, `4489349`, `8f2617e`, `1a9c18b`)
- `ha.Context{ID, ParentID, UserID}` decoded on `StateData` — HA has always
  sent it inside every state object, we simply dropped it. Exposed as
  `state.context` in `stateToLua`, so `get_state` / `get_entities` /
  `get_history` all carry it.
- **Empty context fields are ABSENT from the Lua table, not empty strings.**
  "A device reported this" and "a user with an empty id" must not look alike;
  scripts branch on `if s.context.user_id`.
- Persisted as three columns (`context_id`, `context_parent_id`,
  `context_user_id`) on `state_history`. Existing DBs get them via ALTER in
  `addedColumns`, which runs BEFORE the schema (the partial index needs the
  column) and reads `pragma_table_info` rather than matching an error string —
  SQLite has no ADD COLUMN IF NOT EXISTS. `TestMigrateAddsContextColumns`.
- `idx_sh_context` is **partial** (`WHERE context_id != ''`): seed rows carry
  no context and would otherwise double the index on a high-write table.
- `ha.who_changed(entity_id [, at])` → `{entity_id, state, changed_at,
  context_id, user_id?, caused_by?}` or nil. Without `at` it reads the memory
  mirror; with `at` the newest history row at or before it (the real use — the
  question is asked hours later).
- Cause resolution: look up `parent_id` if set, else `id`, in `context_id`
  across other entities; `automation.*` / `script.*` / `scene.*` sort first
  because one automation turning on four lights leaves five rows sharing a
  context and only one is an answer. A user id found on the cause propagates to
  the effect (the person who ran the script did everything it did).

## History aggregates (2026-08-09, `ce00bb9`)
- `ha.duration_in_state(entity, state, since)` and
  `ha.count_changes(entity, since [, state])`, both returning
  `value, complete`.
- Shared `historyPoints` fetch: one query for the last row BEFORE the window
  (the opening state) plus one for the rows inside it. `complete=false` means
  no pre-window row survived, so the answer is a lower bound — the number is
  still returned, but a script asking about last week under a 2-day retention
  is told rather than handed a confident zero.
- **Repeated rows with the same state are not transitions.** HA emits a
  `state_changed` for attribute-only updates, so counting raw rows would report
  a door opening nine times a minute. The walk compares against the previous
  state.
- An unparseable `changed_at` warns and skips that row rather than failing the
  whole aggregate.
- A still-current state accrues to `time.Now()`, not to its last row.

## Retention overrides (2026-08-09, `d1b7854`)
- `state_history.keep: [{pattern, days}]` in config + the add-on options
  schema; `purge.Rule` keeps the purge package free of a config import.
- **First matching rule wins**: each rule's DELETE excludes the patterns before
  it, and the default DELETE excludes them all. Without that, an overlapping
  aggressive glob would silently delete what a narrow generous one promised.
- Malformed rules (empty pattern, days <= 0) are dropped with a warning — a
  typo in an option must not stop the purge and let the DB grow unbounded.
- Aggregates over a long window are only useful with this set; the default
  stays 2 days.

## Durable reminders (2026-08-09, `c540090`)
- `examples/lib/reminders.lua` + `examples/door_reminders.lua`.
- Exists because `ha.after` persists only when registered at LOAD time, and the
  useful reminder ("still open in ten minutes") is armed from inside a handler.
  Pending work lives in the script's store; one load-time `ha.every` tick
  drives it, and `start()` ticks immediately so whatever came due during the
  downtime fires right after boot.
- Actions are **named** (`define()` at load, `schedule()` by name) — a closure
  cannot go in SQLite.
- A due entry is re-armed or dropped BEFORE its action runs: an action that
  raises must not leave the entry pending and re-fire every tick.
- `escalate` reports the step that just fired, not the next one (the re-arm
  happens first, so the fired step is captured before it).
- `throttle`/`forget` keep last-fired times in the store, so a restart loop
  cannot turn one notification per window into one per boot.
- Tests (`internal/lua/reminders_test.go`) drive the shipped file through a
  real store; the restart cases build a second LState over the same handles.

## Docs
- `ha.set_state` / `ha.remove_state` / `ha.on_command` had shipped since v3 and
  were missing from `lua_api.md` entirely — documented now alongside the new
  calls.
- README "Why" leads with durable state (the thing none of AppDaemon/pyscript/
  NodeRed gives out of the box), DOCS.md gained the `keep` option and a
  reminders section.

## Pending
- Nothing. Released as v4.5.0 (`db614a4`, tagged).
