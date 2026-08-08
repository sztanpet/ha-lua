# State: bundled reference examples (load-examples-spec.md)

Working state for the read-only bundled examples tree. Spec:
`load-examples-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE.** Shipped in 2.2.0 (2026-06-22, tag v2.2.0). Examples keep
growing on top; newest unreleased: per-battery ignore on the Batteries page
(2026-08-08, `26cd2e2`).

## Bundled reference examples (2026-06-22)
- The repo's example/script tree doubled as the author's personal heating
  deployment (real konyha_* entity ids). Split the two concerns: the scripts
  are now a generic REFERENCE set, the author's real config lives only in their
  /config (recoverable from git history if needed).
- `git mv scripts/ -> examples/` (history preserved). Sanitized lib/zones.lua to
  placeholder ids (climate.living_room, ...). The ONLY code coupling was the
  test const repoScriptsDir ("../../scripts" -> "../../examples"); tests use an
  inline testZonesLua fixture, so renaming/sanitizing was ~zero test churn.
- Examples are REFERENCE-ONLY: never loaded or run. Rejected an earlier
  load_examples runnable-second-source design (Supervisor multi-source +
  precedence + watcher fallback) as needless complexity — running examples
  against entities a stranger lacks is useless, and "run my own scripts from the
  bundle" is a deployment concern (git), not the add-on's job.
- New `examples/embed.go` (package bundled): `//go:embed *.lua lib/*.lua *.html`
  + `Materialize(destDir)` (overwrites every file each boot, writes a generated
  README pointing at ../scripts/). embed.go excluded by the patterns.
- config: `ExamplesDir` forced to /config/ha-lua/examples in add-on mode (like
  LogDir/IngressPort); NO user option, NO schema change. Dev leaves it empty =>
  no materialization (don't write into a /config that may not exist).
- main: Materialize runs BEFORE the blocking HA seed wait, so the reference
  appears even when HA is unreachable; best-effort (warn, never fatal).
- Supervisor/watcher/Registry/Scheduler/Router/store UNTOUCHED. Only scripts/
  is loaded and hot-watched, exactly as before.

NOTE: the cards/ embed package (enhanced-climate-card.js → /config/www/ha-lua)
follows this same Materialize pattern — see `enhanced-climate.md`.

## mirrored_switches latency follow-up (2026-07-07)
- User benchmarked mirrored_switches.lua against the equivalent built-in HA
  automation and found ha-lua empirically slower. Root cause: the default
  100 ms event batch window (internal/lua/runner.go batchWindow), NOT the
  architecture — the inherent floor (two WS hops + WAL write before dispatch)
  is a few ms. The example never called ha.immediate_events().
- 46a0f6b examples: mirrored_switches now calls ha.immediate_events() with a
  comment explaining the human-visible-lag case AND why batching is the
  default (real event loss without it).
- e1b57e7 docs: loud callouts everywhere a user would look — lua_api.md
  immediate_events blockquote ("first thing to check when a script feels
  slower than a built-in automation") + pointer from on_state_change,
  DOCS.md "feels slower?" note after the first-script walkthrough + API
  table row, README.md design-decisions entry on the batching trade-off.
- Deliberately NOT changed: the 100 ms default itself — batching was added
  because unbatched bursts REALLY dropped events (user-confirmed history,
  not hypothetical), so immediate delivery stays per-script opt-in — and
  the tracker-write-before-dispatch ordering in main.go
  (load-bearing: handlers read partner state via ha.get_state from the
  mirror, so persisting first is what makes the handler see fresh state).
- Round 2 (2026-07-07): with immediate_events the user still saw high
  VARIANCE vs built-in automations. Cause: OpenDB set no synchronous
  pragma → SQLite default FULL → one WAL fsync per state_changed commit,
  serialized on the single write connection, on the dispatch critical
  path (queues behind every other entity's fsync too). c09f35f sets
  synchronous=NORMAL (+ regression test asserting PRAGMA synchronous=1
  on both handles). Remaining known spike sources if variance persists:
  WAL autocheckpoint (~1000 pages, runs on the committing writer) and
  the hourly purge DELETE holding the write connection; both left alone
  until actually measured as a problem.
- Both rounds shipped in v3.0.1 (2026-07-07, tag on 4cdd2a1).

## battery_levels example (2026-08-06)
- New bundled example: `battery_levels.lua` + `battery_levels.html`, a
  **Batteries** tab (`ha.ui`) listing level / last change / rundown ETA,
  sorted so the first battery to die is on top. Commit 7eb5584 (script,
  page, tests), be24e8c (DOCS.md section). RELEASED in v4.1.0.
- The design decision worth remembering: the ETA cannot come from
  `ha.get_history`. State history is purged after `retention_days` (2 by
  default) and a battery forecast needs weeks. So the script keeps its OWN
  series in its KV store (`series:<entity_id>`), one sample per observed level
  change — a handful of rows a month per entity, capped at MAX_SAMPLES=120.
- Fit is least squares over the samples PLUS the current instant as a point.
  Appending "now" is what makes a battery that stopped moving decay towards a
  flat slope instead of forever predicting last month's rate.
- A rise > RECHARGE_JUMP (2 points) wipes the series: a charge or a swap makes
  every earlier sample worthless. Guards before any ETA is shown at all:
  >= 3 samples, >= 6 h span, >= 2% drop, negative slope. Otherwise the page
  says "measuring".
- Discovery is configuration-free: `device_class: battery` (state IS the
  percentage) plus any entity with a numeric `battery_level` attribute. The
  two are NOT interchangeable for "last changed" — a device_tracker's
  last_changed tracks the phone moving, not the battery — hence the
  `level_is_state` flag, and nil (not "just now") for a first sighting.
- `changed_at` takes the OLDER of HA's last_changed and our newest sample:
  HA's is exact but resets on every HA restart, ours is honest about age but
  up to one scan interval (15 min) late.
- Series cleanup is by "entity gone from the state mirror entirely"
  (`ha.get_state == nil`), never by "missing from this pass" — an unavailable
  device must not lose weeks of samples.
- Tests: `internal/lua/battery_levels_test.go` drives the real example through
  the Router — rundown math + urgency order, discovery/exclusion, recharge
  reset, and a chromedp browser test for the rendered rows and the
  client-side sort.

## battery_levels: per-battery ignore (2026-08-08)
- Commit `26cd2e2`. UNRELEASED — no version bump yet.
- The static `IGNORE` table in the script is GONE, replaced by an `ignored`
  key in the script KV store toggled from the page (`POST /api/ignore`
  `{entity_id, ignored}`, replying with the whole rebuilt scan payload so the
  page never has to patch a row by hand).
- Semantics the user asked for and the tests pin: an ignored battery is **not
  hidden**. It keeps its row and its level, dims, and sorts last — in the
  daemon's urgency order (`by_urgency` checks `ignored` first) AND in every
  client-side sort mode (one trailing stable partition in `sorted()`).
- Ignoring means STOP TRACKING: it is never sampled, and `forget_removed` now
  also deletes the series of anything just ignored, so tracking it again
  starts from one fresh sample instead of a stale slope.
- `goto continue` is not available — gopher-lua is Lua 5.1. The scan loop
  branches with if/else instead.
- Tests: `TestBatteryLevelsIgnore` (still listed, last, no forecast, samples
  dropped, resume starts fresh, 404 unknown entity, 400 no entity_id) plus a
  click-through in the existing chromedp UI test.

## service_api example (2026-08-06)
- New bundled example: `service_api.lua`, one endpoint that calls ANY HA
  service, aimed at shell scripts. Commit 26f0acd (script + tests), ea7f320
  (DOCS.md). RELEASED in v4.1.0 (tag on defe8a2).
- Routes: `POST|GET /call` (prefix, so it also owns `/call/<domain>/<service>`),
  `GET /ping`, `GET /entities`, and `GET "/"` for the builder page. The first
  cut had no `GET "/"` on purpose (a machine-facing API should not become a
  tab); the page reversed that, and `ha.ui` came with it — the debug page flags
  a `/` route WITHOUT `ha.ui`, so the two must travel together.
- Service naming, three ways: path segments, a dotted `service` field, or
  separate `domain` + `service`. Everything not in RESERVED (token/wait/
  domain/service) is forwarded as service data verbatim — that is the whole
  point, no per-service code and nothing to update when HA gains a service.
- Three input transports: query params, form-encoded body (`curl -d k=v`, no
  Content-Type sniffing — a body starting with `{` is JSON, `[` is a 400,
  anything else is form), JSON object body. Body wins over query on collision.
- Type reconstruction for the text transports is the fiddly part:
  `true`/`false` → bool, leading `[`/`{` → json.decode, and a number ONLY when
  `tostring(tonumber(text)) == text`. That round-trip check is load-bearing:
  an alarm `code=0123` must stay a string. `entity_id` is the one field split
  on commas (a comma cannot appear in an entity id, but is ordinary text in a
  notification message).
- Auth: shared token, `crypto.equal` (constant time), accepted as
  `X-Auth-Token`, `Authorization: Bearer`, or `?token=`. Generated with
  `crypto.random_hex(16)` on first load, stored in the script KV and logged at
  warn ONCE; later loads log only a 6-char prefix. Escape hatch for a lost
  token is the `TOKEN` constant, which wins over the store — no magic reset
  key. Rationale: the LAN port (`http_port`) has no HA login in front of it.
- `ha.call_service` is wrapped in `pcall` so an HA rejection becomes a 502 with
  HA's message instead of an on_exception; gopher-lua's `file:line:` prefix is
  stripped from the message first (it means nothing to a curl user).
- GET performing an action is intentional (curl without JSON quoting), noted in
  the script and the commit.
- Builder page (`service_api.html`, `ha.ui("Service API")` + `GET "/"`, commit
  a75d11c): a form that assembles a call and hands back the URL and the curl
  command with copy buttons. New token-guarded `GET /entities` feeds the
  domain/entity pickers from the state mirror; service names CANNOT be
  enumerated (no binding for HA's service registry), so those are a static
  suggestion list over a free-text field — do not mistake it for a whitelist.
- Token delivery, REVERSED on user instruction (commit 356af45): the first cut
  asked the user to paste the token; now `service_api.lua` substitutes it into
  the page (`__SERVICE_API_TOKEN__`) as it serves it. The user was told the
  consequence and reaffirmed: anyone who can open the page on the LAN port has
  the token, so it guards against guessing the URL, not against the network.
  Do NOT "fix" this back to a prompt. The field is still editable (build a
  command for another install) and still verified against `./ping` — its own
  origin, not the typed base URL, which may not resolve from the browser.
- Injection escapes `\`, `"` AND `/` for the JS string literal (a token
  containing `</script>` would end the script block), and substitutes through a
  gsub replacement FUNCTION so a `%` in a hand-picked TOKEN is not read as a
  capture reference.
- Base URL default: page origin + `/s/service_api`, EXCEPT under ingress
  (path contains `/api/hassio_ingress/`), where it falls back to
  `<host>:8100` — the ingress URL carries an HA session curl does not have,
  and the real `http_port` is unknowable from a script.
- The JS `typeOf` mirrors the Lua `coerce` rules (incl. the number round-trip
  check) so the badge next to each value is the truth; if one side changes the
  other must follow.
- Tests: `internal/lua/service_api_test.go` boots the real example with both a
  recording sync and async call_service — every request shape, the coercion
  rules incl. `0123`, token rejection (incl. one-char-short vs the constant
  time compare), the 400/502 error contract, and `wait=false` taking the async
  path. Plus a chromedp test driving the builder: the token verifying live, the
  entity picker filtered to the chosen domain, exact URL/curl output for both
  request styles, and the generated URL fired back at the endpoint to prove it
  actually reaches light.turn_on.
