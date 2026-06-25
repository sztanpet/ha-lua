# State: core daemon (plan.md)

Working state for the core daemon track. Spec: `plan.md`. Global decisions live
in `../AI.state`.

Status: **COMPLETE — milestones 1–12.** Released as add-on 1.0.0; the daemon has
been stable since. Later tracks (thermostat UI, fs plugin, bundled examples,
enhanced climate) build on it — see their own state files.

## Post-1.0 daemon changes (unreleased on main, 2026-06-25)
- **Bounded daemon log** (internal/logwriter): ha-lua.log capped at 5 MiB total
  (active file + one `.1` backup, each ≤ budget/2 so the sum stays under budget,
  retaining the previous segment). main.openLogFile now returns logwriter.New.
  The per-script ha.exceptions.log_file paths are still unbounded (separate, only
  written on errors) — cap those too if it ever matters.
- **Per-script event buffer 64 -> 256** (internal/lua/runner.go): wildcard
  subscriptions (enhanced_climate's binary_sensor.*/climate.*) can overflow the
  channel and drop events (Send warns "event channel full, dropping"). 256 matches
  the upstream ha.Client.Events buffer. A buffer only defers drops under sustained
  overload — a hot script ultimately needs a narrower subscription.

## Completed (commits on main)
- `0740db9` scaffold — module, deps, Makefile, .golangci.yml, tools.go, benchmarks/
- `647818b` M1 HA client — auth flow, reconnect with backoff, event stream (internal/ha)
- `5103275` M2 state tracker — schema, migrations, upsert + history append (internal/state, internal/testutil)
- `2a1eaa4` M3 Lua runner — runner, ha.*/store.*/global.* API, store.state() proxy, JSON round-trip
- `ee30da4` M4+M5 — event dispatch (registry), service calls, fire_event, config, pprof, main wiring
- `4373da1` M6 hot reload — script supervisor + fsnotify watcher
- `11225f0` M7 purge — state_history retention job
- `747eb21` M8 scheduling — SQLite-backed timer engine
- `7d11f10` M9 error handling — verify pcall coverage for timer callbacks and test exception system
- `796d9d8` lua: fix memory leak in one-shot after timers
- `e7f8444` scheduler: add concurrency stress benchmark

### Landed earlier than planned (already in the commits above)
- `ha.get_history`, `ha.get_entities`, `ha.get_entity_ids` (planned for M7)
- `ha.on_exception` + `ha.exceptions.email` / `ha.exceptions.log_file` (planned for M9)
- Restricted `require` (scripts/lib/ only) in internal/lua/stdlib.go (planned for M10)
- `ha.on_state_change` opts.initial=true delivery (planned for M10)

## Milestone 12 — Add-on packaging (2026-06-15)
- config.yaml: HA add-on manifest (slug ha-lua, arch aarch64/amd64,
  homeassistant_api, homeassistant_config map with path:/config override,
  options + schema). Options/schema
  deliberately omit url/token/scripts_dir/database — add-on mode forces those.
- config.dev.yaml: standalone dev config; the ONE file that must carry
  homeassistant.url + token (dev mode reads them straight from YAML; only
  add-on mode injects them from the Supervisor). Keys mirror config.go yaml
  tags exactly; verified it round-trips through config.Load.
- Dockerfile: multi-stage golang:1.26-bookworm builder (NOT 1.24 — go.mod
  requires 1.26.4) → base-debian. No GOARCH mapping: home-assistant/builder
  runs the build emulated under the target arch, so plain `go build` is
  correct; mapping HA's aarch64→Go's arm64 by hand is the classic broken-image
  trap. CGO_ENABLED=0 static, ca-certificates for TLS. Built clean and the
  binary runs (`--config` flag present); final image content ~38MB.
- .dockerignore: keeps the build context clean and never copies a host-built
  ha-lua binary / .git / *.db into the image.
- run.sh: bashio entrypoint, no flags (single prod config channel).
- build.yaml: base-debian for both arches.
- .github/workflows/release.yml: RELEASE ONLY per user decision (no PR
  test/lint workflow). Tag push (v*) → home-assistant/builder@2026.06.0,
  matrix aarch64/amd64, GHCR login, push ghcr.io/<owner>/ha-lua-{arch}.
  config.yaml version must be kept in sync with the tag.
- DOCS.md: user-facing add-on docs (install, paths, first script, every
  option, API table). CHANGELOG.md: 1.0.0 entry.
- README.md: removed the stale "Planned" section — timers, purge, sandbox,
  and stdlib are all implemented; added timer rows + stdlib note.

## Milestone 11 — Testing & benchmarks (2026-06-15)
- Established performance baseline in `benchmarks/baseline.txt`.
- Added benchmarks for registry dispatch, event parsing, and config loading.
- Silenced logging during benchmarks to keep output clean and results files small.
- Verified that all packages have unit tests and they pass `make test`.

## Milestone 10 — Lua stdlib (2026-06-15)
- Full sandboxing: `lua.NewState(lua.Options{SkipOpenLibs: true})` used in `newLState`.
- `RegisterStdlib` selectively opens base, table, string, math, os (restricted), and coroutine.
- Dangerous globals (`load`, `dofile`, `package`, etc.) and `os` functions (`execute`, `exit`, etc.) removed/nil'd.
- Implemented `strings`, `time`, `json`, `re`, `http`, and `crypto` modules.
- Augmented `math` with `round`, `clamp`, `log2`, and `sign`.
- `re` module includes a per-LState 256-entry LRU regex cache.
- `http` module uses `L.Context()` for 5s callback timeout enforcement.
- `crypto` provides MD5, SHA1/256/512, HMAC, Base64, Hex, and CSRNG bytes/hex.
- Tests and benchmarks added to `internal/lua/stdlib_test.go`.

## Milestone 9 — error handling (2026-06-13)
- Verify `pcall` coverage for timer callbacks via new `TestTimerExceptionHandling` unit test in `internal/lua/api_ha_test.go`.
- Ensure exceptions occurring in timer callbacks (every/at/after) are caught, protected, and routed to `ha.on_exception` with real stack tracebacks.
- Confirm email cooldown behavior (which was implemented early in M8) is fully tested.
- Fix memory leak by deleting one-shot `ha.after` timers from the script's `timerFns` map after firing.
- Write `BenchmarkSchedulerConcurrencyStress` in `internal/scheduler/scheduler_test.go` to stress test the scheduler under highly concurrent registration, pruning, firing, and script removals.

## Milestone 8 — scheduling (2026-06-13)
- internal/scheduler/scheduler.go: SQLite-backed timer engine with min-heap.
- ha.every, ha.at, ha.after Lua APIs implemented in internal/lua/api_ha.go.
- Timers persisted in SQLite; catch-up logic fires missed recurring timers once on startup.
- ha.after is best-effort: orphaned rows cleaned up on startup.
- ha.at timezone resolution via config.timezone or $TZ, fallback to UTC.
- TimerFiredEvent dispatch wired into internal/lua/runner.go.
- PruneScript removes orphaned every/at timers when script reloads.
- RemoveScript cleans up heap and after-timers on script stop.
- Tests: TestTimerAPI in api_ha_test.go, full scheduler tests in scheduler_test.go.

## Milestone 7 — purge (2026-06-13)
- internal/purge/purge.go: Purger with New(db, retentionDays, interval) —
  plain params, not the config struct (no reason to couple purge to
  config). Start(ctx) spawns the ticker goroutine; RunOnce(ctx) is the
  whole job: one DELETE with a Go-computed RFC3339 cutoff per plan.
- Start runs one purge immediately before the first tick: with the
  default 1h interval a frequently restarted daemon would never purge.
- Uses the write DB handle (DELETE is a write). Logs rows deleted only
  when > 0.
- Tests: retention boundary table test, same-day RFC3339-cutoff
  regression test (retention 0 + row 1h old), empty table. BenchmarkPurge
  (10k expired rows) landed with the tests, not deferred to M11.
- Dependencies also updated this session (e6651b2): go-isatty, x/mod,
  x/tools, modernc.org/libc — all indirect.

## Milestone 6 — hot reload (4373da1)
- internal/lua/supervisor.go: Supervisor owns start/stop/reload; Deps
  struct carries tracker/global/NewKV/CallService/FireEvent/OnLoaded.
  Stop = registry.Remove → Runner.Close() (drain) → wait done, 5s timeout
  then per-script ctx cancel (aborts the gopher-lua VM). Reload =
  Stop + Start; also starts brand-new files.
- internal/lua/watcher.go: ScriptWatcher; NewScriptWatcher registers the
  inotify watch synchronously (no missed-file window vs LoadAll), Run
  debounces 300ms per script and acts on the settled file state (present →
  reload, absent → stop) — event types lie under atomic saves.
- Runner gains Close() and EventTypes(); Registry lost EventTypes()/RunAll
  (supervisor replaced their only caller); DispatchToTimer sends under the
  read lock so a runner's channel can't be closed mid-Send.
- OnLoaded hook → client.AddEventType for each script's event types on
  every (re)load; never unsubscribes (plan §"On hot reload").
- main.go: MkdirAll(scripts_dir) on startup (fresh add-on install), wires
  supervisor + watcher, ends with sup.Wait().

## Code review pass (2026-06-13, commits fc14d45..4373da1)
- **WAL was never on.** The DSN used mattn-style `_journal_mode=WAL`, which
  modernc.org/sqlite silently ignores (verified: journal_mode=delete).
  Fixed to `_pragma=journal_mode(WAL)` + busy_timeout(5000) +
  foreign_keys(on); write handle adds `_txlock=immediate`.
  TestOpenDBEnablesWAL guards the syntax. testutil DSNs also fixed and made
  unique per test (shared-cache memory DBs with a fixed name leak state
  between parallel tests). (fc14d45)
- **HA client lifecycle** (9f3dffe): re-seed on every reconnect per plan
  (States channel: cap 1, newest wins, never closed; main consumes
  forever); AddEventType is mutex-protected, dedups, and subscribes on the
  live connection (was: race + dead until next reconnect); conn published
  to SendRaw only after auth_ok; backoff resets after a connection that
  lived >1min; per-connection msgID reset removed (raced concurrent
  SendRaw; HA only needs increasing IDs per connection).
- Dead MakeCallService/MakeFireEvent removed (d8ea14f).
- on_exception traceback is now the real Lua stack trace via
  *lua.ApiError.StackTrace (was: a second copy of the error message);
  on_state_change rejects malformed glob patterns at load time (7a1451e).
- fsnotify was NOT in go.mod despite earlier notes claiming it was — added
  v1.10.1 with the M6 work.

## Tooling pass (2026-06-12, commits 8814eff..11903db)
- golangci-lint had NEVER run: not installed, v1-format config that v2
  rejects, and `make check` silently omitted the lint step. Now: v2.12.2
  installed, config migrated to v2 format, check includes lint, and the
  pre-commit hook makes forgetting impossible. First real run found 38
  issues, all fixed (package comments, noctx, errcheck, ST1023).
- `make fmt` was broken (`gofmt -w ./...` — gofmt takes paths, not package
  patterns). Fixed to `gofmt -l -w .`.
- Latent bug exposed: json/v2 marshals map keys in RANDOM order by default;
  the plan wrongly claimed sorted-by-default. luaMarshal now passes
  json.Deterministic(true); TestLuaJSONRoundTrip had been passing by luck.
- golangci-lint install script (install.sh) has a checksum-verification bug:
  it greps the checksums file and matches the .sbom.json line. The release
  tarball itself verified fine against the published sha256; installed from
  the manually verified tarball. Re-check when bumping versions.

## Plan review fixes (applied to plan.md 2026-06-12)
All 10 review findings are now folded into plan.md: Go-computed RFC3339 purge
cutoff + idx_sh_time index; seed-on-reconnect history dedup; require caching +
cycle detection; http uses callback context only (StdlibOpts/HTTPTimeout knob
removed); email handler cooldown (default 15m, suppressed-count reporting);
ha.at timezone resolution (timezone option → $TZ → UTC warn, time/tzdata);
run.sh passes no flags (options.json is the single prod config channel);
golangci-lint rationale corrected + .golangci.yml migrated to v2 format;
websocket lib is github.com/coder/websocket.

## Code follow-ups — ALL DONE (commits 83a0617..d49ca17, 2026-06-12)
- websocket import swapped to github.com/coder/websocket (83a0617)
- idx_sh_time(changed_at) added to the schema (1a5ea29)
- Seed appends history only when state/attributes differ from the mirror;
  mirror still upserted unconditionally so last_changed stays current (e961255)
- require caches module return values per LState (module returning nothing →
  true, standard Lua convention); circular require raises a clear error;
  tests in internal/lua/stdlib_test.go (46c0d81)
- Email handler cooldown: default 15m, config.cooldown override, suppressed
  count reported in next send, failed sends also start the window;
  smtp.SendMail is behind the smtpSendMail package var for tests (b170fa0)
- Flag overrides removed from main.go; add-on mode (no --config) forces
  URL/token/paths from the Supervisor environment and ignores any connection
  fields in options.json; tests in internal/config/config_test.go (677d96d)
- Tree gofmt'ed — earlier milestones had misaligned fields (d49ca17)
