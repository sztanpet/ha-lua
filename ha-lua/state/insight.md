# State: history questions, attribution, durable reminders

No spec file — the work came out of a review of what HA users most often ask
for that HA cannot do. Global decisions live in `../AI.state`.

Status: **COMPLETE**, shipped in v4.5.0 (2026-08-09).

## Decisions and why
- **Empty context fields are ABSENT from the Lua table, not empty strings.**
  "A device reported this" and "a user with an empty id" must not look alike.
- `idx_sh_context` is partial (`WHERE context_id != ''`): seed rows carry no
  context and would double the index on a high-write table.
- Added columns are applied BEFORE the schema (the partial index needs them)
  and detected via `pragma_table_info` — SQLite has no ADD COLUMN IF NOT
  EXISTS.
- `who_changed` cause resolution sorts `automation.*`/`script.*`/`scene.*`
  first: one automation touching four lights leaves five rows on one context
  and only one is the answer. A user id on the cause propagates to the effect.
- Aggregates return `value, complete`; `complete = false` means no pre-window
  row survived retention, so the number is a lower bound rather than a
  confident zero. Rows repeating a state are not transitions (HA emits
  `state_changed` for attribute-only updates).
- Retention `keep` rules: **first matching rule wins** (each DELETE excludes
  the patterns before it); malformed rules are dropped with a warn so a typo
  cannot stop the purge.
- `lib/reminders.lua` exists because `ha.after` persists only when registered at
  load. Actions are NAMED (a closure cannot go in SQLite). A due entry is
  re-armed or dropped and SAVED before any action runs, so a raising action
  cannot re-fire every tick. `throttle`/`forget` state is in the store, so a
  restart loop cannot turn one notification per window into one per boot.
